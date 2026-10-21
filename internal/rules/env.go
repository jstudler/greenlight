package rules

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"k8s.io/apiserver/pkg/cel/library"

	"github.com/jstudler/greenlight/internal/celfuncs"
)

// costLimit caps the cost of a single expression evaluation so that a
// pathological rule cannot stall a worker.
const costLimit = 1_000_000

// interruptCheckFrequency is how often the CEL interpreter checks for
// cancellation and for the cost limit.
const interruptCheckFrequency = 100

// newEnv builds the CEL environment used for every greenlight expression. It
// mirrors the Kubernetes ValidatingAdmissionPolicy conventions: `object`,
// `params` and `variables` are the only root variables.
func newEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("params", cel.DynType),
		cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)),
		cel.OptionalTypes(),
		ext.Strings(),
		ext.Lists(),
		ext.Sets(),
		ext.Math(),
		ext.Encoders(),
		library.IP(),
		library.CIDR(),
		library.Format(),
		library.Lists(),
		library.Regex(),
		library.Quantity(),
		library.URLs(),
		celfuncs.Library(),
	)
	if err != nil {
		return nil, fmt.Errorf("building the CEL environment: %w", err)
	}
	return env, nil
}

// outputKind describes the type an expression must produce.
type outputKind int

const (
	outputAny outputKind = iota
	outputBool
	outputString
)

func (k outputKind) celType() *cel.Type {
	switch k {
	case outputBool:
		return cel.BoolType
	case outputString:
		return cel.StringType
	default:
		return cel.DynType
	}
}

// compile compiles a single expression and checks its output type.
func compile(env *cel.Env, path, expression string, want outputKind) (cel.Program, error) {
	if expression == "" {
		return nil, fmt.Errorf("%s: the expression must not be empty", path)
	}
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("%s: %w", path, issues.Err())
	}
	if want != outputAny && !assignableTo(ast.OutputType(), want.celType()) {
		return nil, fmt.Errorf("%s: the expression must return %s, but it returns %s",
			path, want.celType(), ast.OutputType())
	}
	prog, err := env.Program(ast,
		cel.CostLimit(costLimit),
		cel.InterruptCheckFrequency(interruptCheckFrequency),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return prog, nil
}

// assignableTo reports whether an expression of type got may be used where a
// value of type want is expected. `dyn` is accepted because it defers the check
// to runtime, exactly like Kubernetes does.
func assignableTo(got, want *cel.Type) bool {
	if got == nil {
		return false
	}
	switch got.Kind() {
	case cel.DynKind, cel.AnyKind, cel.TypeParamKind:
		return true
	}
	if got.IsExactType(want) {
		return true
	}
	// An optional of the wanted type is acceptable: the value is unwrapped
	// before it is used.
	if got.Kind() == cel.OpaqueKind && got.TypeName() == "optional_type" &&
		len(got.Parameters()) == 1 {
		return assignableTo(got.Parameters()[0], want)
	}
	return false
}
