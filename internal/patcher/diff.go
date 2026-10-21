package patcher

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Desired is the status greenlight wants an object to have.
type Desired struct {
	// Fields are the top-level status fields produced by the rule.
	Fields map[string]any
	// Conditions are the managed conditions, ready condition first.
	Conditions []Managed
}

// NeedsPatch reports whether the current status differs from the desired one,
// together with a short human-readable reason for the logs.
//
// Besides the obvious value comparison it also detects fields that greenlight
// wrote earlier but no longer produces: those are owned by our field manager
// and server-side apply will remove them, which is a real change.
func NeedsPatch(current map[string]any, desired Desired, owned sets.Set[string]) (bool, string) {
	for _, field := range sortedKeys(desired.Fields) {
		if !semanticEqual(current[field], desired.Fields[field]) {
			return true, fmt.Sprintf("status.%s differs", field)
		}
	}

	for _, field := range sets.List(owned) {
		if field == "conditions" {
			continue
		}
		if _, wanted := desired.Fields[field]; wanted {
			continue
		}
		if _, present := current[field]; present {
			return true, fmt.Sprintf("status.%s is no longer produced", field)
		}
	}

	existing := Conditions(current)
	for _, m := range desired.Conditions {
		index := indexOfCondition(existing, m.Type)
		if index < 0 {
			return true, fmt.Sprintf("the condition %s is missing", m.Type)
		}
		cond, _ := existing[index].(map[string]any)
		if stringValue(cond, "status") != string(m.Status) {
			return true, fmt.Sprintf("the condition %s has a different status", m.Type)
		}
		if stringValue(cond, "reason") != m.Reason {
			return true, fmt.Sprintf("the condition %s has a different reason", m.Type)
		}
		if stringValue(cond, "message") != m.Message {
			return true, fmt.Sprintf("the condition %s has a different message", m.Type)
		}
		if !semanticEqual(cond["observedGeneration"], m.ObservedGeneration) {
			return true, fmt.Sprintf("the condition %s has a different observedGeneration", m.Type)
		}
	}
	return false, ""
}

// semanticEqual compares two decoded JSON values. Numbers are compared by
// value, so that an int64 from a rule and a float64 from the API server's JSON
// decoding are considered equal.
func semanticEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if af, aok := asFloat(a); aok {
		bf, bok := asFloat(b)
		return bok && af == bf
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !semanticEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			other, present := bv[k]
			if !present || !semanticEqual(v, other) {
				return false
			}
		}
		return true
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// OwnedStatusFields returns the top-level status fields that the given field
// manager owns, according to metadata.managedFields.
func OwnedStatusFields(obj *unstructured.Unstructured, fieldManager string) sets.Set[string] {
	out := sets.New[string]()
	if obj == nil {
		return out
	}
	entries, found, err := unstructured.NestedSlice(obj.Object, "metadata", "managedFields")
	if err != nil || !found {
		return out
	}
	for _, item := range entries {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(entry, "manager") != fieldManager {
			continue
		}
		if stringValue(entry, "subresource") != "status" {
			continue
		}
		fields, ok := entry["fieldsV1"].(map[string]any)
		if !ok {
			continue
		}
		status, ok := fields["f:status"].(map[string]any)
		if !ok {
			continue
		}
		for key := range status {
			if name, ok := strings.CutPrefix(key, "f:"); ok {
				out.Insert(name)
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
