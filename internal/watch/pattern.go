// Package watch turns `--watch` patterns into concrete API resources by asking
// the API server's discovery endpoint.
package watch

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Pattern is a parsed `--watch` value.
//
// Supported forms:
//
//	group/Kind          the preferred version of the group
//	group/version/Kind  an exact group/version
//	group/*             every suitable resource in the preferred version
//
// The core group is written as "core" or as an empty group ("/Kind").
type Pattern struct {
	Group   string
	Version string
	Kind    string
	// AnyKind is true for the `group/*` form.
	AnyKind bool
	// Raw is the original pattern, for messages.
	Raw string
}

func (p Pattern) String() string { return p.Raw }

// ParsePatterns parses a list of pattern arguments. Each argument may itself be
// a comma-separated list.
func ParsePatterns(args []string) ([]Pattern, error) {
	var out []Pattern
	for _, arg := range args {
		for _, item := range strings.Split(arg, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			p, err := ParsePattern(item)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// ParsePattern parses a single watch pattern.
func ParsePattern(raw string) (Pattern, error) {
	parts := strings.Split(raw, "/")
	p := Pattern{Raw: raw}
	switch len(parts) {
	case 2:
		p.Group, p.Kind = normalizeGroup(parts[0]), parts[1]
	case 3:
		p.Group, p.Version, p.Kind = normalizeGroup(parts[0]), parts[1], parts[2]
	default:
		return Pattern{}, fmt.Errorf("invalid watch pattern %q: expected group/Kind, group/version/Kind or group/*", raw)
	}
	if p.Kind == "" {
		return Pattern{}, fmt.Errorf("invalid watch pattern %q: the kind must not be empty", raw)
	}
	if p.Kind == "*" {
		p.AnyKind = true
	}
	if len(parts) == 3 && p.Version == "" {
		return Pattern{}, fmt.Errorf("invalid watch pattern %q: the version must not be empty", raw)
	}
	if strings.Contains(p.Group, "*") {
		return Pattern{}, fmt.Errorf("invalid watch pattern %q: the API group must not be a wildcard", raw)
	}
	return p, nil
}

// normalizeGroup maps the "core" alias to the empty core API group.
func normalizeGroup(group string) string {
	if group == "core" {
		return ""
	}
	return group
}

// Resource is a resolved, watchable resource.
type Resource struct {
	GVR        schema.GroupVersionResource
	GVK        schema.GroupVersionKind
	Namespaced bool
}

func (r Resource) String() string {
	return fmt.Sprintf("%s (%s)", r.GVK, r.GVR.Resource)
}
