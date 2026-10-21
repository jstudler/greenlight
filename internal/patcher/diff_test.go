package patcher

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
)

func readyCondition(status, reason, message string, observedGeneration int64) map[string]any {
	return map[string]any{
		"type":               "Ready",
		"status":             status,
		"reason":             reason,
		"message":            message,
		"observedGeneration": observedGeneration,
		"lastTransitionTime": "2025-01-01T00:00:00Z",
	}
}

func TestNeedsPatch(t *testing.T) {
	desired := Desired{
		Fields: map[string]any{"prefix": "10.0.0.0/25", "count": int64(3)},
		Conditions: []Managed{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit", Message: "ok", ObservedGeneration: 1},
		},
	}
	upToDate := map[string]any{
		"prefix":     "10.0.0.0/25",
		"count":      int64(3),
		"conditions": []any{readyCondition("True", "Greenlit", "ok", 1)},
	}

	tests := []struct {
		name    string
		current map[string]any
		desired Desired
		owned   sets.Set[string]
		want    bool
	}{
		{name: "identical", current: upToDate, desired: desired, want: false},
		{
			name: "int64 against float64 from JSON",
			current: map[string]any{
				"prefix": "10.0.0.0/25", "count": float64(3),
				"conditions": []any{readyCondition("True", "Greenlit", "ok", 1)},
			},
			desired: desired,
			want:    false,
		},
		{
			name:    "empty status",
			current: map[string]any{},
			desired: desired,
			want:    true,
		},
		{
			name: "different field value",
			current: map[string]any{
				"prefix": "10.0.1.0/25", "count": int64(3),
				"conditions": []any{readyCondition("True", "Greenlit", "ok", 1)},
			},
			desired: desired,
			want:    true,
		},
		{
			name: "generation bump",
			current: map[string]any{
				"prefix": "10.0.0.0/25", "count": int64(3),
				"conditions": []any{readyCondition("True", "Greenlit", "ok", 1)},
			},
			desired: Desired{
				Fields: desired.Fields,
				Conditions: []Managed{{
					Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit", Message: "ok",
					ObservedGeneration: 2,
				}},
			},
			want: true,
		},
		{
			name:    "annotation flip without a generation bump",
			current: upToDate,
			desired: Desired{
				Fields: desired.Fields,
				Conditions: []Managed{{
					Type: "Ready", Status: metav1.ConditionFalse, Reason: "NotGreenlit", Message: "ok",
					ObservedGeneration: 1,
				}},
			},
			want: true,
		},
		{
			name:    "missing managed condition",
			current: map[string]any{"prefix": "10.0.0.0/25", "count": int64(3)},
			desired: desired,
			want:    true,
		},
		{
			name: "field we own is no longer produced",
			current: map[string]any{
				"prefix": "10.0.0.0/25", "count": int64(3), "stale": "value",
				"conditions": []any{readyCondition("True", "Greenlit", "ok", 1)},
			},
			desired: desired,
			owned:   sets.New("prefix", "count", "stale"),
			want:    true,
		},
		{
			name: "field someone else owns is ignored",
			current: map[string]any{
				"prefix": "10.0.0.0/25", "count": int64(3), "foreign": "value",
				"conditions": []any{readyCondition("True", "Greenlit", "ok", 1)},
			},
			desired: desired,
			owned:   sets.New("prefix", "count"),
			want:    false,
		},
		{
			name: "nested value differs",
			current: map[string]any{
				"details":    map[string]any{"a": int64(1)},
				"conditions": []any{readyCondition("True", "Greenlit", "", 0)},
			},
			desired: Desired{
				Fields:     map[string]any{"details": map[string]any{"a": int64(2)}},
				Conditions: []Managed{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit"}},
			},
			want: true,
		},
		{
			name: "list order matters",
			current: map[string]any{
				"list":       []any{"a", "b"},
				"conditions": []any{readyCondition("True", "Greenlit", "", 0)},
			},
			desired: Desired{
				Fields:     map[string]any{"list": []any{"b", "a"}},
				Conditions: []Managed{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Greenlit"}},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owned := tc.owned
			if owned == nil {
				owned = sets.New[string]()
			}
			got, reason := NeedsPatch(tc.current, tc.desired, owned)
			if got != tc.want {
				t.Fatalf("NeedsPatch = %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestOwnedStatusFields(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"managedFields": []any{
				map[string]any{
					"manager":     "greenlight",
					"operation":   "Apply",
					"subresource": "status",
					"fieldsV1": map[string]any{
						"f:status": map[string]any{
							"f:prefix":     map[string]any{},
							"f:conditions": map[string]any{},
						},
					},
				},
				map[string]any{
					"manager":     "greenlight",
					"operation":   "Update",
					"fieldsV1":    map[string]any{"f:status": map[string]any{"f:spec-owned": map[string]any{}}},
					"subresource": "",
				},
				map[string]any{
					"manager":     "other-controller",
					"subresource": "status",
					"fieldsV1":    map[string]any{"f:status": map[string]any{"f:foreign": map[string]any{}}},
				},
			},
		},
	}}

	got := OwnedStatusFields(obj, "greenlight")
	want := sets.New("prefix", "conditions")
	if !got.Equal(want) {
		t.Fatalf("OwnedStatusFields = %v, want %v", sets.List(got), sets.List(want))
	}

	if fields := OwnedStatusFields(&unstructured.Unstructured{Object: map[string]any{}}, "greenlight"); fields.Len() != 0 {
		t.Fatalf("expected no owned fields, got %v", sets.List(fields))
	}
}

func TestSemanticEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{name: "int64 and float64", a: int64(3), b: float64(3), want: true},
		{name: "different numbers", a: int64(3), b: int64(4), want: false},
		{name: "nil and nil", a: nil, b: nil, want: true},
		{name: "nil and value", a: nil, b: "x", want: false},
		{name: "strings", a: "x", b: "x", want: true},
		{name: "string and number", a: "3", b: int64(3), want: false},
		{name: "bools", a: true, b: true, want: true},
		{name: "maps", a: map[string]any{"a": int64(1)}, b: map[string]any{"a": float64(1)}, want: true},
		{name: "maps with extra keys", a: map[string]any{"a": int64(1)}, b: map[string]any{"a": int64(1), "b": int64(2)}, want: false},
		{name: "lists", a: []any{int64(1), "x"}, b: []any{float64(1), "x"}, want: true},
		{name: "lists of different length", a: []any{int64(1)}, b: []any{int64(1), int64(2)}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := semanticEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("semanticEqual(%#v, %#v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
