package rules_test

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/jstudler/greenlight/internal/rules"
)

func loadExample(t *testing.T, name string) *rules.Set {
	t.Helper()
	set, err := rules.Load([]string{filepath.Join("..", "..", "examples", name)}, rules.DefaultOptions())
	if err != nil {
		t.Fatalf("loading examples/%s: %v", name, err)
	}
	return set
}

func evaluate(t *testing.T, set *rules.Set, gvk schema.GroupVersionKind, obj map[string]any) *rules.Evaluation {
	t.Helper()
	u := &unstructured.Unstructured{Object: obj}
	rule := set.Select(gvk)
	matched, errs := rule.MatchConditions(context.Background(), set, u)
	if len(errs) != 0 {
		t.Fatalf("matchConditions reported errors: %v", errs)
	}
	if !matched {
		t.Fatalf("the object did not match rule %q", rule.Name)
	}
	evaluation := rule.Evaluate(context.Background(), set, u)
	if len(evaluation.Errors) != 0 {
		t.Fatalf("evaluation errors: %v", evaluation.Errors)
	}
	return evaluation
}

func conditionOf(t *testing.T, evaluation *rules.Evaluation, conditionType string) rules.ConditionValue {
	t.Helper()
	for _, cond := range evaluation.Conditions {
		if cond.Type == conditionType {
			return cond
		}
	}
	t.Fatalf("the condition %q was not produced; got %+v", conditionType, evaluation.Conditions)
	return rules.ConditionValue{}
}

func TestNetboxExamplePrefixClaim(t *testing.T) {
	set := loadExample(t, "netbox.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "netbox.dev", Version: "v1", Kind: "PrefixClaim"}

	evaluation := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "topology", "namespace": "default"},
		"spec": map[string]any{
			"prefixLength":          "/25",
			"parentPrefixSelector":  map[string]any{"family": "IPv4", "environment": "test"},
			"tenant":                "example",
			"site":                  "example",
			"parentPrefixSelectorX": nil,
		},
	})

	prefix, ok := evaluation.Fields["prefix"].(string)
	if !ok {
		t.Fatalf("status.prefix = %#v, want a string", evaluation.Fields["prefix"])
	}
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil {
		t.Fatalf("status.prefix %q is not a CIDR: %v", prefix, err)
	}
	if parsed.Bits() != 25 {
		t.Fatalf("status.prefix %q should be a /25", prefix)
	}
	// The consumers reject a prefix that still has host bits set.
	if parsed.Masked() != parsed {
		t.Fatalf("status.prefix %q is not masked", prefix)
	}
	if !netip.MustParsePrefix("10.0.0.0/8").Contains(parsed.Addr()) {
		t.Fatalf("status.prefix %q is outside the default pool", prefix)
	}
	if evaluation.Fields["parentPrefix"] != "10.0.0.0/8" {
		t.Fatalf("status.parentPrefix = %#v", evaluation.Fields["parentPrefix"])
	}
	if evaluation.Fields["prefixName"] != "topology" {
		t.Fatalf("status.prefixName = %#v", evaluation.Fields["prefixName"])
	}

	ready := conditionOf(t, evaluation, "Ready")
	if ready.Status != metav1.ConditionTrue || ready.Reason != "PrefixClaimResourceReady" {
		t.Fatalf("unexpected ready condition: %+v", ready)
	}
	assigned := conditionOf(t, evaluation, "PrefixAssigned")
	if assigned.Status != metav1.ConditionTrue || assigned.Reason != "PrefixCRCreated" {
		t.Fatalf("unexpected PrefixAssigned condition: %+v", assigned)
	}

	// Determinism: the same name always yields the same prefix.
	again := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "topology", "namespace": "default"},
		"spec": map[string]any{
			"prefixLength":         "/25",
			"parentPrefixSelector": map[string]any{"family": "IPv4"},
		},
	})
	if again.Fields["prefix"] != prefix {
		t.Fatalf("the allocation is not deterministic: %v != %v", again.Fields["prefix"], prefix)
	}
}

func TestNetboxExamplePrefixClaimIPv6AndExplicitParent(t *testing.T) {
	set := loadExample(t, "netbox.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "netbox.dev", Version: "v1", Kind: "PrefixClaim"}

	v6 := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "six", "namespace": "default"},
		"spec": map[string]any{
			"prefixLength":         "/64",
			"parentPrefixSelector": map[string]any{"family": "IPv6"},
		},
	})
	if !strings.HasPrefix(v6.Fields["prefix"].(string), "fd00:") {
		t.Fatalf("expected an IPv6 prefix, got %v", v6.Fields["prefix"])
	}

	explicit := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "pool", "namespace": "default"},
		"spec": map[string]any{
			"prefixLength": "/28",
			"parentPrefix": "192.0.2.0/24",
		},
	})
	if explicit.Fields["parentPrefix"] != "192.0.2.0/24" {
		t.Fatalf("an explicit spec.parentPrefix must win: %v", explicit.Fields["parentPrefix"])
	}
	if !strings.HasPrefix(explicit.Fields["prefix"].(string), "192.0.2.") {
		t.Fatalf("the prefix must come from spec.parentPrefix: %v", explicit.Fields["prefix"])
	}
}

func TestNetboxExampleIPAddressClaim(t *testing.T) {
	set := loadExample(t, "netbox.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "netbox.dev", Version: "v1", Kind: "IpAddressClaim"}

	const parent = "10.112.64.0/25"
	evaluation := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "topology-ipv4-virtual", "namespace": "default"},
		"spec":     map[string]any{"parentPrefix": parent},
	})

	dotted, ok := evaluation.Fields["ipAddressDotDecimal"].(string)
	if !ok {
		t.Fatalf("status.ipAddressDotDecimal = %#v", evaluation.Fields["ipAddressDotDecimal"])
	}
	addr, err := netip.ParseAddr(dotted)
	if err != nil {
		t.Fatalf("status.ipAddressDotDecimal %q is not a bare IP: %v", dotted, err)
	}
	if !netip.MustParsePrefix(parent).Contains(addr) {
		t.Fatalf("status.ipAddressDotDecimal %q is outside %s", dotted, parent)
	}
	if evaluation.Fields["ipAddress"] != dotted+"/32" {
		t.Fatalf("status.ipAddress = %#v, want %q", evaluation.Fields["ipAddress"], dotted+"/32")
	}
	if evaluation.Fields["ipAddressName"] != "topology-ipv4-virtual" {
		t.Fatalf("status.ipAddressName = %#v", evaluation.Fields["ipAddressName"])
	}

	ready := conditionOf(t, evaluation, "Ready")
	if ready.Status != metav1.ConditionTrue || ready.Reason != "IPAddressResourceReady" {
		t.Fatalf("unexpected ready condition: %+v", ready)
	}
	conditionOf(t, evaluation, "IPAssigned")
}

func TestNetboxExampleAnnotations(t *testing.T) {
	set := loadExample(t, "netbox.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "netbox.dev", Version: "v1", Kind: "PrefixClaim"}
	rule := set.Select(gvk)

	skipped := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "x", "namespace": "ns",
			"annotations": map[string]any{"statuspatcher.dev/skip": "true"},
		},
		"spec": map[string]any{"prefixLength": "/25"},
	}}
	matched, errs := rule.MatchConditions(context.Background(), set, skipped)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if matched {
		t.Fatal("statuspatcher.dev/skip=true must exclude the object")
	}

	notReady := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{
			"name": "x", "namespace": "ns",
			"annotations": map[string]any{"statuspatcher.dev/ready": "false"},
		},
		"spec": map[string]any{"prefixLength": "/25"},
	})
	ready := conditionOf(t, notReady, "Ready")
	if ready.Status != metav1.ConditionFalse {
		t.Fatalf("statuspatcher.dev/ready=false must set Ready=False, got %q", ready.Status)
	}
	if ready.Reason != "PrefixClaimResourceNotReady" {
		t.Fatalf("expected the falseReason, got %q", ready.Reason)
	}
	if !strings.Contains(ready.Message, "annotation") {
		t.Fatalf("expected the computed message, got %q", ready.Message)
	}
}

func TestNetboxExampleSaltChangesTheAllocation(t *testing.T) {
	set := loadExample(t, "netbox.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "netbox.dev", Version: "v1", Kind: "PrefixClaim"}
	obj := map[string]any{
		"metadata": map[string]any{"name": "topology", "namespace": "default"},
		"spec":     map[string]any{"prefixLength": "/25"},
	}

	before := evaluate(t, set, gvk, obj).Fields["prefix"]
	set.SetParam("salt", "second-run/")
	after := evaluate(t, set, gvk, obj).Fields["prefix"]

	if before == after {
		t.Fatalf("params.salt did not change the allocation (%v)", before)
	}
}

func TestGenericExample(t *testing.T) {
	set := loadExample(t, "generic.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "WidgetClaim"}

	withID := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "w", "namespace": "ns", "generation": int64(3)},
		"spec":     map[string]any{"generateId": true},
	})
	if withID.Fields["lastOperationStatus"] != "COMPLETED" {
		t.Fatalf("status.lastOperationStatus = %#v", withID.Fields["lastOperationStatus"])
	}
	endpoint, ok := withID.Fields["endpoint"].(string)
	if !ok || !strings.HasPrefix(endpoint, "192.0.2.") {
		t.Fatalf("status.endpoint = %#v", withID.Fields["endpoint"])
	}
	if withID.Fields["endpointCIDR"] != endpoint+"/32" {
		t.Fatalf("status.endpointCIDR = %#v", withID.Fields["endpointCIDR"])
	}
	generated, ok := withID.Fields["generatedId"].(string)
	if !ok || generated == "" {
		t.Fatalf("status.generatedId = %#v, want a non-empty string", withID.Fields["generatedId"])
	}
	upstream, ok := withID.Fields["upstreamIds"].([]any)
	if !ok || len(upstream) != 2 {
		t.Fatalf("status.upstreamIds = %#v", withID.Fields["upstreamIds"])
	}
	details, ok := withID.Fields["details"].(map[string]any)
	if !ok || details["provisionedBy"] != "greenlight" {
		t.Fatalf("status.details = %#v", withID.Fields["details"])
	}
	if value, ok := details["observedSpecGeneration"].(int64); !ok || value != 3 {
		t.Fatalf("status.details.observedSpecGeneration = %#v, want the int64 3", details["observedSpecGeneration"])
	}
	conditionOf(t, withID, "EndpointAssigned")

	withoutID := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "w", "namespace": "ns"},
		"spec":     map[string]any{"generateId": false},
	})
	if _, present := withoutID.Fields["generatedId"]; present {
		t.Fatal("status.generatedId must be omitted when spec.generateId is false")
	}
}

func TestGenericExampleCatchAll(t *testing.T) {
	set := loadExample(t, "generic.rules.yaml")
	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}

	rule := set.Select(gvk)
	if rule.Name != "example-catch-all" {
		t.Fatalf("selected %q, want example-catch-all", rule.Name)
	}
	evaluation := evaluate(t, set, gvk, map[string]any{
		"metadata": map[string]any{"name": "g", "namespace": "ns"},
	})
	if len(evaluation.Fields) != 0 {
		t.Fatalf("the catch-all rule must not write status fields: %v", evaluation.Fields)
	}
	ready := conditionOf(t, evaluation, "Ready")
	if ready.Status != metav1.ConditionTrue || ready.Message != "Reconciled by greenlight" {
		t.Fatalf("unexpected ready condition: %+v", ready)
	}
}
