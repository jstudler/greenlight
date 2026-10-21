package patcher

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Managed is one condition that greenlight owns.
type Managed struct {
	Type               string
	Status             metav1.ConditionStatus
	Reason             string
	Message            string
	ObservedGeneration int64
}

// UpsertConditions merges the managed conditions into the existing condition
// list and returns the complete list.
//
// The condition lists of the targeted CRDs are atomic (they carry no
// x-kubernetes-list-type: map), so server-side apply replaces the whole list.
// Conditions owned by other controllers must therefore be written back
// verbatim, and lastTransitionTime must be preserved whenever the status of a
// managed condition does not change.
func UpsertConditions(existing []any, managed []Managed, now metav1.Time) []any {
	out := make([]any, 0, len(existing)+len(managed))
	for _, item := range existing {
		if cond, ok := item.(map[string]any); ok {
			out = append(out, copyMap(cond))
			continue
		}
		out = append(out, item)
	}

	for _, m := range managed {
		entry := map[string]any{
			"type":               m.Type,
			"status":             string(m.Status),
			"reason":             m.Reason,
			"message":            m.Message,
			"observedGeneration": m.ObservedGeneration,
			"lastTransitionTime": now.UTC().Format(metav1.RFC3339Micro),
		}
		if index := indexOfCondition(out, m.Type); index >= 0 {
			previous, _ := out[index].(map[string]any)
			if stringValue(previous, "status") == string(m.Status) {
				if last := stringValue(previous, "lastTransitionTime"); last != "" {
					entry["lastTransitionTime"] = last
				}
			}
			out[index] = entry
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Conditions reads status.conditions from an object as a raw list.
func Conditions(status map[string]any) []any {
	list, _ := status["conditions"].([]any)
	return list
}

func indexOfCondition(list []any, conditionType string) int {
	for i, item := range list {
		cond, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(cond, "type") == conditionType {
			return i
		}
	}
	return -1
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
