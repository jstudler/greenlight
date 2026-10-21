// Package rules loads, validates, compiles and evaluates greenlight rules
// files. Expressions are written in CEL, following the Kubernetes
// ValidatingAdmissionPolicy conventions.
package rules

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/util/validation"
)

// compileRule validates one rule and compiles every expression it contains.
func compileRule(env *cel.Env, spec ruleSpec, opts Options) (*Rule, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("every rule needs a name")
	}
	if errs := validation.IsDNS1123Label(spec.Name); len(errs) > 0 {
		return nil, fmt.Errorf("rule %q: the name must be a DNS label: %s",
			spec.Name, strings.Join(errs, "; "))
	}

	rule := &Rule{
		Name:     spec.Name,
		group:    normalizeGroup(spec.Match.APIGroup),
		kinds:    map[string]bool{},
		versions: map[string]bool{},
		status:   map[string]cel.Program{},
	}

	if len(spec.Match.Kinds) == 0 {
		return nil, fmt.Errorf("rule %q: match.kinds must list at least one kind (use \"*\" for all)", spec.Name)
	}
	for _, kind := range spec.Match.Kinds {
		if kind == "*" {
			rule.anyKind = true
			continue
		}
		if kind == "" {
			return nil, fmt.Errorf("rule %q: match.kinds must not contain an empty kind", spec.Name)
		}
		rule.kinds[kind] = true
	}
	for _, version := range spec.Match.Versions {
		if version == "" {
			return nil, fmt.Errorf("rule %q: match.versions must not contain an empty version", spec.Name)
		}
		rule.versions[version] = true
	}

	delay, err := parseDelay(spec.Delay)
	if err != nil {
		return nil, fmt.Errorf("rule %q: delay: %w", spec.Name, err)
	}
	rule.delay = delay

	for i, mc := range spec.MatchConditions {
		name := mc.Name
		if name == "" {
			name = fmt.Sprintf("%d", i)
		}
		path := fmt.Sprintf("matchConditions[%s]", name)
		prog, err := compile(env, path, mc.Expression, outputBool)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", spec.Name, err)
		}
		rule.matchConditions = append(rule.matchConditions, compiledExpr{name: name, path: path, prog: prog})
	}

	seenVariables := map[string]bool{}
	for _, v := range spec.Variables {
		if v.Name == "" {
			return nil, fmt.Errorf("rule %q: every variable needs a name", spec.Name)
		}
		if seenVariables[v.Name] {
			return nil, fmt.Errorf("rule %q: duplicate variable %q", spec.Name, v.Name)
		}
		seenVariables[v.Name] = true
		path := "variables." + v.Name
		prog, err := compile(env, path, v.Expression, outputAny)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", spec.Name, err)
		}
		rule.variables = append(rule.variables, compiledExpr{name: v.Name, path: path, prog: prog})
	}

	for field, expression := range spec.Status {
		switch field {
		case conditionsField, observedGenerationField:
			return nil, fmt.Errorf("rule %q: status.%s is reserved and managed by greenlight itself",
				spec.Name, field)
		case "":
			return nil, fmt.Errorf("rule %q: status field names must not be empty", spec.Name)
		}
		path := "status." + field
		prog, err := compile(env, path, expression, outputAny)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", spec.Name, err)
		}
		rule.status[field] = prog
		rule.statusKeys = append(rule.statusKeys, field)
	}
	sort.Strings(rule.statusKeys)

	ready, err := compileCondition(env, spec.ReadyCondition, opts.ConditionType, "readyCondition", opts)
	if err != nil {
		return nil, fmt.Errorf("rule %q: %w", spec.Name, err)
	}
	rule.ready = ready

	seenTypes := map[string]bool{opts.ConditionType: true}
	for i := range spec.Conditions {
		cond := spec.Conditions[i]
		path := fmt.Sprintf("conditions[%d]", i)
		if cond.Type == "" {
			return nil, fmt.Errorf("rule %q: %s: type is required", spec.Name, path)
		}
		if cond.Type == opts.ConditionType {
			return nil, fmt.Errorf("rule %q: %s: the type %q is the managed ready condition; "+
				"configure it under readyCondition instead", spec.Name, path, cond.Type)
		}
		if seenTypes[cond.Type] {
			return nil, fmt.Errorf("rule %q: %s: duplicate condition type %q", spec.Name, path, cond.Type)
		}
		seenTypes[cond.Type] = true
		compiled, err := compileCondition(env, &cond, cond.Type, path, opts)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", spec.Name, err)
		}
		rule.conditions = append(rule.conditions, compiled)
	}

	return rule, nil
}

func compileCondition(env *cel.Env, spec *conditionSpec, conditionType, path string, opts Options) (*compiledCondition, error) {
	out := &compiledCondition{
		conditionType: conditionType,
		reason:        opts.Reason,
		falseReason:   opts.FalseReason,
		message:       opts.Message,
		path:          path,
	}
	if err := validateConditionType(conditionType); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if spec == nil {
		return out, nil
	}
	if spec.Reason != nil {
		out.reason = *spec.Reason
	}
	if spec.FalseReason != nil {
		out.falseReason = *spec.FalseReason
	}
	if spec.Message != nil {
		out.message = *spec.Message
	}
	if err := validateReason(out.reason); err != nil {
		return nil, fmt.Errorf("%s.reason: %w", path, err)
	}
	if err := validateReason(out.falseReason); err != nil {
		return nil, fmt.Errorf("%s.falseReason: %w", path, err)
	}
	if spec.StatusExpression != "" {
		prog, err := compile(env, path+".statusExpression", spec.StatusExpression, outputBool)
		if err != nil {
			return nil, err
		}
		out.statusProg = prog
	}
	if spec.MessageExpression != "" {
		prog, err := compile(env, path+".messageExpression", spec.MessageExpression, outputString)
		if err != nil {
			return nil, err
		}
		out.messageProg = prog
	}
	return out, nil
}

// normalizeGroup maps the "core" alias to the empty core API group.
func normalizeGroup(group string) string {
	if group == "core" {
		return ""
	}
	return group
}
