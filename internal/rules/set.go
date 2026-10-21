package rules

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Options carries the CLI defaults that the rules file may override.
type Options struct {
	// ConditionType is the type of the managed ready condition.
	ConditionType string
	// Reason is used when the ready condition is True.
	Reason string
	// FalseReason is used when the ready condition is False.
	FalseReason string
	// Message is the default ready condition message.
	Message string
}

// DefaultOptions returns the built-in defaults.
func DefaultOptions() Options {
	return Options{
		ConditionType: "Ready",
		Reason:        "Greenlit",
		FalseReason:   "NotGreenlit",
		Message:       "Status set by greenlight",
	}
}

// Set is a compiled, immutable collection of rules.
type Set struct {
	params      map[string]any
	rules       []*Rule
	defaultRule *Rule
	opts        Options
}

// Rule is a single compiled rule.
type Rule struct {
	Name string

	group    string
	kinds    map[string]bool
	anyKind  bool
	versions map[string]bool

	delay time.Duration

	matchConditions []compiledExpr
	variables       []compiledExpr
	statusKeys      []string
	status          map[string]cel.Program

	ready      *compiledCondition
	conditions []*compiledCondition

	// isDefault marks the implicit rule used when nothing else matches.
	isDefault bool
}

type compiledExpr struct {
	name string
	path string
	prog cel.Program
}

type compiledCondition struct {
	conditionType string
	statusProg    cel.Program
	reason        string
	falseReason   string
	message       string
	messageProg   cel.Program
	path          string
}

// Params returns the merged parameters that are exposed to CEL as `params`.
func (s *Set) Params() map[string]any { return s.params }

// SetParam overrides or adds a single parameter. It is used by `--param`.
func (s *Set) SetParam(key string, value any) { s.params[key] = value }

// WatchPatterns returns the deduplicated `--watch` patterns implied by the
// match blocks of all rules, in file order.
func (s *Set) WatchPatterns() []string {
	seen := map[string]bool{}
	var out []string
	for _, rule := range s.rules {
		for _, pattern := range rule.WatchPatterns() {
			if seen[pattern] {
				continue
			}
			seen[pattern] = true
			out = append(out, pattern)
		}
	}
	return out
}

// Rules returns the compiled rules in file order.
func (s *Set) Rules() []*Rule { return s.rules }

// Options returns the effective defaults of the set.
func (s *Set) Options() Options { return s.opts }

// Delay returns the per-rule delay.
func (r *Rule) Delay() time.Duration { return r.delay }

// IsDefault reports whether this is the implicit fall-back rule.
func (r *Rule) IsDefault() bool { return r.isDefault }

// Match reports whether the rule applies to the given kind, ignoring
// matchConditions.
func (r *Rule) Match(gvk schema.GroupVersionKind) bool {
	if r.isDefault {
		return true
	}
	if r.group != gvk.Group {
		return false
	}
	if !r.anyKind && !r.kinds[gvk.Kind] {
		return false
	}
	if len(r.versions) > 0 && !r.versions[gvk.Version] {
		return false
	}
	return true
}

// specificity orders matching rules: an exact kind beats a wildcard, and a
// pinned version beats "any version".
func (r *Rule) specificity(gvk schema.GroupVersionKind) int {
	score := 0
	if !r.anyKind && r.kinds[gvk.Kind] {
		score += 2
	}
	if len(r.versions) > 0 {
		score++
	}
	return score
}

// Select returns the rule that applies to gvk. It never returns nil: when no
// rule matches, the implicit default rule (managed ready condition only) is
// returned.
func (s *Set) Select(gvk schema.GroupVersionKind) *Rule {
	var best *Rule
	bestScore := -1
	for _, r := range s.rules {
		if !r.Match(gvk) {
			continue
		}
		if score := r.specificity(gvk); score > bestScore {
			best, bestScore = r, score
		}
	}
	if best == nil {
		return s.defaultRule
	}
	return best
}

// MatchesAnyOf reports whether the rule matches at least one of the given
// kinds. It is used to warn about rules that can never fire.
func (r *Rule) MatchesAnyOf(gvks []schema.GroupVersionKind) bool {
	for _, gvk := range gvks {
		if r.Match(gvk) {
			return true
		}
	}
	return false
}

// WatchPatterns returns the `--watch` patterns implied by the rule's match
// block. It is used when no explicit --watch flag is given.
func (r *Rule) WatchPatterns() []string {
	group := r.group
	if group == "" {
		group = "core"
	}
	versions := sortedKeys(r.versions)
	kinds := []string{"*"}
	if !r.anyKind {
		kinds = sortedKeys(r.kinds)
	}
	var out []string
	for _, kind := range kinds {
		if len(versions) == 0 {
			out = append(out, group+"/"+kind)
			continue
		}
		for _, version := range versions {
			out = append(out, strings.Join([]string{group, version, kind}, "/"))
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// String renders the rule's match block, for logs and for --validate.
func (r *Rule) String() string {
	if r.isDefault {
		return "<default>"
	}
	return fmt.Sprintf("%s (%s)", r.Name, strings.Join(r.WatchPatterns(), ","))
}

// StatusFieldCount returns the number of status fields the rule produces.
func (r *Rule) StatusFieldCount() int { return len(r.statusKeys) }

// ConditionTypes returns the types of every condition the rule manages, with
// the ready condition first.
func (r *Rule) ConditionTypes() []string {
	out := []string{r.ready.conditionType}
	for _, cond := range r.conditions {
		out = append(out, cond.conditionType)
	}
	return out
}
