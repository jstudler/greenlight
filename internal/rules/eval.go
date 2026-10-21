package rules

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apiserver/pkg/cel/lazy"
)

// variablesTypeName is the CEL object type backing the lazily evaluated
// `variables` map, mirroring Kubernetes' "kubernetes.variables".
const variablesTypeName = "greenlight.variables"

// EvalError describes one failed expression. Evaluation errors never block
// readiness: the affected field is omitted and the error is reported here so
// that the caller can log it.
type EvalError struct {
	// Path is the location of the expression, for example "status.prefix".
	Path string
	Err  error
}

func (e EvalError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e EvalError) Unwrap() error { return e.Err }

// ConditionValue is a managed condition after evaluation.
type ConditionValue struct {
	Type    string
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// Evaluation is the desired status computed from one rule.
type Evaluation struct {
	// Rule is the name of the rule that produced this evaluation.
	Rule string
	// Fields are the top-level status fields, already converted to plain
	// JSON-compatible values. Omitted fields are absent.
	Fields map[string]any
	// Conditions are the managed conditions. The ready condition is first.
	Conditions []ConditionValue
	// Errors lists every expression that failed.
	Errors []EvalError
}

// Ready reports the status of the managed ready condition.
func (e *Evaluation) Ready() metav1.ConditionStatus {
	if len(e.Conditions) == 0 {
		return metav1.ConditionUnknown
	}
	return e.Conditions[0].Status
}

// FieldNames returns the names of the status fields that were produced, sorted.
func (e *Evaluation) FieldNames() []string {
	out := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// evaluator holds the per-object evaluation state.
type evaluator struct {
	ctx        context.Context
	activation map[string]any
	// evaluating guards against variables that reference each other in a cycle.
	evaluating map[string]bool
	errs       []EvalError
}

func (r *Rule) newEvaluator(ctx context.Context, obj *unstructured.Unstructured, params map[string]any) *evaluator {
	e := &evaluator{
		ctx:        ctx,
		evaluating: map[string]bool{},
	}
	object := map[string]any{}
	if obj != nil {
		object = obj.Object
	}
	e.activation = map[string]any{
		"object": object,
		"params": params,
	}
	lazyMap := lazy.NewMapValue(types.NewObjectType(variablesTypeName))
	for i := range r.variables {
		v := r.variables[i]
		lazyMap.Append(v.name, func(*lazy.MapValue) ref.Val {
			if e.evaluating[v.name] {
				return types.NewErr("variable %q refers to itself", v.name)
			}
			e.evaluating[v.name] = true
			defer delete(e.evaluating, v.name)

			out, _, err := v.prog.ContextEval(e.ctx, e.activation)
			if err != nil {
				return types.NewErr("%s", err.Error())
			}
			return out
		})
	}
	e.activation["variables"] = lazyMap
	return e
}

func (e *evaluator) eval(path string, prog cel.Program) (ref.Val, bool) {
	out, _, err := prog.ContextEval(e.ctx, e.activation)
	if err != nil {
		e.errs = append(e.errs, EvalError{Path: path, Err: err})
		return nil, false
	}
	return out, true
}

// MatchConditions evaluates the rule's matchConditions. All of them must be
// true for the object to be processed. A failing expression is treated as true,
// so that a broken expression never silently disables a rule.
func (r *Rule) MatchConditions(ctx context.Context, set *Set, obj *unstructured.Unstructured) (bool, []EvalError) {
	if len(r.matchConditions) == 0 {
		return true, nil
	}
	e := r.newEvaluator(ctx, obj, set.params)
	for _, mc := range r.matchConditions {
		out, ok := e.eval(mc.path, mc.prog)
		if !ok {
			continue
		}
		value, err := convert(out)
		if err != nil {
			e.errs = append(e.errs, EvalError{Path: mc.path, Err: err})
			continue
		}
		matched, ok := value.(bool)
		if !ok {
			e.errs = append(e.errs, EvalError{Path: mc.path,
				Err: fmt.Errorf("the expression returned %T instead of a bool", value)})
			continue
		}
		if !matched {
			return false, e.errs
		}
	}
	return true, e.errs
}

// Evaluate computes the desired status for obj.
func (r *Rule) Evaluate(ctx context.Context, set *Set, obj *unstructured.Unstructured) *Evaluation {
	e := r.newEvaluator(ctx, obj, set.params)
	result := &Evaluation{Rule: r.Name, Fields: map[string]any{}}

	for _, field := range r.statusKeys {
		out, ok := e.eval("status."+field, r.status[field])
		if !ok {
			continue
		}
		value, err := convert(out)
		if err != nil {
			if !errors.Is(err, errOmit) {
				e.errs = append(e.errs, EvalError{Path: "status." + field, Err: err})
			}
			continue
		}
		result.Fields[field] = value
	}

	result.Conditions = append(result.Conditions, e.condition(r.ready))
	for _, cond := range r.conditions {
		result.Conditions = append(result.Conditions, e.condition(cond))
	}

	result.Errors = e.errs
	return result
}

// condition evaluates one managed condition. A failing statusExpression keeps
// the condition True, and a failing messageExpression falls back to the literal
// message.
func (e *evaluator) condition(c *compiledCondition) ConditionValue {
	status := metav1.ConditionTrue
	if c.statusProg != nil {
		if out, ok := e.eval(c.path+".statusExpression", c.statusProg); ok {
			value, err := convert(out)
			switch {
			case err != nil && !errors.Is(err, errOmit):
				e.errs = append(e.errs, EvalError{Path: c.path + ".statusExpression", Err: err})
			case err == nil:
				if ready, ok := value.(bool); ok && !ready {
					status = metav1.ConditionFalse
				} else if !ok {
					e.errs = append(e.errs, EvalError{Path: c.path + ".statusExpression",
						Err: fmt.Errorf("the expression returned %T instead of a bool", value)})
				}
			}
		}
	}

	out := ConditionValue{Type: c.conditionType, Status: status, Message: c.message}
	if status == metav1.ConditionTrue {
		out.Reason = c.reason
	} else {
		out.Reason = c.falseReason
	}

	if c.messageProg != nil {
		if val, ok := e.eval(c.path+".messageExpression", c.messageProg); ok {
			value, err := convert(val)
			if err != nil && !errors.Is(err, errOmit) {
				e.errs = append(e.errs, EvalError{Path: c.path + ".messageExpression", Err: err})
			} else if message, ok := value.(string); ok && message != "" {
				out.Message = message
			}
		}
	}
	return out
}
