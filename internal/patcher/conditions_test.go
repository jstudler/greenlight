package patcher

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func condition(conditionType, status, reason, message, lastTransitionTime string) map[string]any {
	return map[string]any{
		"type":               conditionType,
		"status":             status,
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": lastTransitionTime,
	}
}

func TestUpsertConditions(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	const before = "2025-01-01T00:00:00Z"

	t.Run("appends a new condition", func(t *testing.T) {
		got := UpsertConditions(nil, []Managed{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit", Message: "ok", ObservedGeneration: 2,
		}}, now)

		if len(got) != 1 {
			t.Fatalf("got %d conditions, want 1", len(got))
		}
		entry := got[0].(map[string]any)
		if entry["type"] != "Ready" || entry["status"] != "True" || entry["reason"] != "Greenlit" {
			t.Fatalf("unexpected condition: %#v", entry)
		}
		if entry["observedGeneration"] != int64(2) {
			t.Fatalf("observedGeneration = %#v, want the int64 2", entry["observedGeneration"])
		}
		if entry["lastTransitionTime"] == "" {
			t.Fatal("lastTransitionTime was not set")
		}
	})

	t.Run("keeps lastTransitionTime when the status does not change", func(t *testing.T) {
		existing := []any{condition("Ready", "True", "Greenlit", "ok", before)}
		got := UpsertConditions(existing, []Managed{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit", Message: "changed", ObservedGeneration: 3,
		}}, now)

		entry := got[0].(map[string]any)
		if entry["lastTransitionTime"] != before {
			t.Fatalf("lastTransitionTime = %v, want it preserved", entry["lastTransitionTime"])
		}
		if entry["message"] != "changed" {
			t.Fatalf("message = %v, want it updated", entry["message"])
		}
	})

	t.Run("refreshes lastTransitionTime when the status changes", func(t *testing.T) {
		existing := []any{condition("Ready", "True", "Greenlit", "ok", before)}
		got := UpsertConditions(existing, []Managed{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "NotGreenlit", Message: "nope",
		}}, now)

		entry := got[0].(map[string]any)
		if entry["lastTransitionTime"] == before {
			t.Fatal("lastTransitionTime should have been refreshed")
		}
	})

	t.Run("keeps foreign conditions", func(t *testing.T) {
		existing := []any{
			condition("Synced", "True", "Done", "by someone else", before),
			condition("Ready", "False", "Waiting", "", before),
		}
		got := UpsertConditions(existing, []Managed{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit"},
			{Type: "PrefixAssigned", Status: metav1.ConditionTrue, Reason: "PrefixCRCreated"},
		}, now)

		if len(got) != 3 {
			t.Fatalf("got %d conditions, want 3", len(got))
		}
		foreign := got[0].(map[string]any)
		if foreign["type"] != "Synced" || foreign["message"] != "by someone else" {
			t.Fatalf("the foreign condition was modified: %#v", foreign)
		}
		if got[1].(map[string]any)["status"] != "True" {
			t.Fatalf("the managed condition was not upserted in place: %#v", got[1])
		}
		if got[2].(map[string]any)["type"] != "PrefixAssigned" {
			t.Fatalf("the extra condition was not appended: %#v", got[2])
		}
	})

	t.Run("does not mutate the input", func(t *testing.T) {
		original := condition("Ready", "True", "Greenlit", "ok", before)
		existing := []any{original}
		UpsertConditions(existing, []Managed{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NotGreenlit"}}, now)
		if original["status"] != "True" {
			t.Fatalf("the input condition was mutated: %#v", original)
		}
	})
}
