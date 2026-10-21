package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// reasonRegex is the pattern the Kubernetes API enforces on
// `status.conditions[].reason`: no spaces and no dashes.
var reasonRegex = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

const (
	maxReasonLength        = 1024
	maxConditionTypeLength = 316
)

// Load reads, validates and compiles the given rules files. The files are
// concatenated in order: later files may override `params` and `defaults`, and
// their rules are appended.
//
// Compilation errors are fatal and are reported here, so that a broken rules
// file never reaches the reconcile loop.
func Load(paths []string, opts Options) (*Set, error) {
	files := make([]fileSpec, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the rules file %s: %w", path, err)
		}
		spec, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf("parsing the rules file %s: %w", path, err)
		}
		files = append(files, *spec)
	}
	return Compile(files, opts)
}

// LoadBytes compiles a single in-memory rules file. It exists for tests and for
// embedding.
func LoadBytes(data []byte, opts Options) (*Set, error) {
	spec, err := parse(data)
	if err != nil {
		return nil, err
	}
	return Compile([]fileSpec{*spec}, opts)
}

// parse decodes one rules file. sigs.k8s.io/yaml goes through go-yaml, so YAML
// anchors (&x), aliases (*x) and merge keys (<<:) are supported.
func parse(data []byte) (*fileSpec, error) {
	var spec fileSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	if spec.APIVersion != APIVersion {
		return nil, fmt.Errorf("unsupported apiVersion %q, expected %q", spec.APIVersion, APIVersion)
	}
	if spec.Kind != Kind {
		return nil, fmt.Errorf("unsupported kind %q, expected %q", spec.Kind, Kind)
	}
	return &spec, nil
}

// Compile validates and compiles already-parsed rules files.
func Compile(files []fileSpec, opts Options) (*Set, error) {
	if opts.ConditionType == "" {
		opts = DefaultOptions()
	}
	if err := validateConditionType(opts.ConditionType); err != nil {
		return nil, fmt.Errorf("--condition-type: %w", err)
	}
	if err := validateReason(opts.Reason); err != nil {
		return nil, fmt.Errorf("--reason: %w", err)
	}
	if err := validateReason(opts.FalseReason); err != nil {
		return nil, fmt.Errorf("--false-reason: %w", err)
	}

	env, err := newEnv()
	if err != nil {
		return nil, err
	}

	set := &Set{params: map[string]any{}, opts: opts}
	seen := map[string]bool{}

	for _, file := range files {
		params, err := DecodeParams(file.Params)
		if err != nil {
			return nil, fmt.Errorf("params: %w", err)
		}
		for k, v := range params {
			set.params[k] = v
		}
		applyDefaults(&set.opts, file.Defaults)
	}

	for _, file := range files {
		for i := range file.Rules {
			spec := file.Rules[i]
			if seen[spec.Name] {
				return nil, fmt.Errorf("rule %q: duplicate rule name", spec.Name)
			}
			seen[spec.Name] = true
			rule, err := compileRule(env, spec, set.opts)
			if err != nil {
				return nil, err
			}
			set.rules = append(set.rules, rule)
		}
	}

	set.defaultRule = &Rule{
		Name:      "default",
		anyKind:   true,
		isDefault: true,
		ready: &compiledCondition{
			conditionType: set.opts.ConditionType,
			reason:        set.opts.Reason,
			falseReason:   set.opts.FalseReason,
			message:       set.opts.Message,
			path:          "readyCondition",
		},
	}
	return set, nil
}

func applyDefaults(opts *Options, d *defaultsSpec) {
	if d == nil || d.ReadyCondition == nil {
		return
	}
	if d.ReadyCondition.Reason != nil {
		opts.Reason = *d.ReadyCondition.Reason
	}
	if d.ReadyCondition.FalseReason != nil {
		opts.FalseReason = *d.ReadyCondition.FalseReason
	}
	if d.ReadyCondition.Message != nil {
		opts.Message = *d.ReadyCondition.Message
	}
}

func validateReason(reason string) error {
	if len(reason) > maxReasonLength {
		return fmt.Errorf("the reason %q is longer than %d characters", reason, maxReasonLength)
	}
	if !reasonRegex.MatchString(reason) {
		return fmt.Errorf("the reason %q does not match %s (no spaces and no dashes are allowed)",
			reason, reasonRegex)
	}
	return nil
}

func validateConditionType(conditionType string) error {
	if len(conditionType) > maxConditionTypeLength {
		return fmt.Errorf("the condition type %q is longer than %d characters",
			conditionType, maxConditionTypeLength)
	}
	if errs := validation.IsQualifiedName(conditionType); len(errs) > 0 {
		return fmt.Errorf("the condition type %q is not a qualified name: %s",
			conditionType, strings.Join(errs, "; "))
	}
	return nil
}

// DecodeParams converts the raw `params` block into plain Go values, keeping
// integers as int64. The default encoding/json behaviour would turn every
// number into a float64, which breaks CEL arithmetic such as
// `params.base + variables.offset` with "no such overload double + int".
func DecodeParams(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	normalized, err := normalizeNumbers(out)
	if err != nil {
		return nil, err
	}
	m, _ := normalized.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// ParseParamValue parses a single `--param key=value` value as a YAML scalar,
// preserving integers.
func ParseParamValue(value string) (any, error) {
	jsonValue, err := yaml.YAMLToJSON([]byte(value))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(jsonValue))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return normalizeNumbers(out)
}

func normalizeNumbers(v any) (any, error) {
	switch val := v.(type) {
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return i, nil
		}
		f, err := val.Float64()
		if err != nil {
			return nil, fmt.Errorf("the number %q is not representable", val.String())
		}
		return f, nil
	case map[string]any:
		for k, item := range val {
			converted, err := normalizeNumbers(item)
			if err != nil {
				return nil, err
			}
			val[k] = converted
		}
		return val, nil
	case []any:
		for i, item := range val {
			converted, err := normalizeNumbers(item)
			if err != nil {
				return nil, err
			}
			val[i] = converted
		}
		return val, nil
	default:
		return v, nil
	}
}

func parseDelay(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("the delay %q must not be negative", raw)
	}
	return d, nil
}
