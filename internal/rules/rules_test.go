package rules

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func mustLoad(t *testing.T, yaml string) *Set {
	t.Helper()
	set, err := LoadBytes([]byte(yaml), DefaultOptions())
	if err != nil {
		t.Fatalf("loading the rules: %v", err)
	}
	return set
}

func object(fields map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: fields}
}

const header = "apiVersion: statuspatcher.dev/v1alpha1\nkind: GreenlightConfig\n"

func TestLoadRejectsBadFiles(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "wrong apiVersion",
			yaml:    "apiVersion: other/v1\nkind: GreenlightConfig\n",
			wantErr: "unsupported apiVersion",
		},
		{
			name:    "wrong kind",
			yaml:    "apiVersion: statuspatcher.dev/v1alpha1\nkind: Other\n",
			wantErr: "unsupported kind",
		},
		{
			name: "duplicate rule name",
			yaml: header + `rules:
  - name: dup
    match: {apiGroup: example.com, kinds: [A]}
  - name: dup
    match: {apiGroup: example.com, kinds: [B]}
`,
			wantErr: "duplicate rule name",
		},
		{
			name: "rule name is not a DNS label",
			yaml: header + `rules:
  - name: Not_A_Label
    match: {apiGroup: example.com, kinds: [A]}
`,
			wantErr: "must be a DNS label",
		},
		{
			name: "no kinds",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com}
`,
			wantErr: "must list at least one kind",
		},
		{
			name: "reserved status field conditions",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    status:
      conditions: "[]"
`,
			wantErr: "reserved",
		},
		{
			name: "reserved status field observedGeneration",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    status:
      observedGeneration: "1"
`,
			wantErr: "reserved",
		},
		{
			name: "invalid reason",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    readyCondition:
      reason: Not A Reason
`,
			wantErr: "does not match",
		},
		{
			name: "invalid falseReason with a dash",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    readyCondition:
      falseReason: not-ready
`,
			wantErr: "does not match",
		},
		{
			name: "compile error",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    status:
      field: "object.metadata.name +"
`,
			wantErr: "status.field",
		},
		{
			name: "status expression is not a bool",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    readyCondition:
      statusExpression: "'yes'"
`,
			wantErr: "must return bool",
		},
		{
			name: "message expression is not a string",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    readyCondition:
      messageExpression: "42"
`,
			wantErr: "must return string",
		},
		{
			name: "extra condition reuses the ready type",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    conditions:
      - {type: Ready}
`,
			wantErr: "managed ready condition",
		},
		{
			name: "duplicate variable",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    variables:
      - {name: x, expression: "1"}
      - {name: x, expression: "2"}
`,
			wantErr: "duplicate variable",
		},
		{
			name: "bad delay",
			yaml: header + `rules:
  - name: a
    match: {apiGroup: example.com, kinds: [A]}
    delay: tomorrow
`,
			wantErr: "delay",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml), DefaultOptions())
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error containing %q, got %q", tc.wantErr, err)
			}
		})
	}
}

func TestLoadSupportsYAMLAnchorsAndMergeKeys(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: first
    match: {apiGroup: example.com, kinds: [A]}
    matchConditions: &optOut
      - name: not-opted-out
        expression: "object.metadata.?annotations[?'statuspatcher.dev/skip'].orValue('') != 'true'"
    readyCondition: &ready
      reason: Greenlit
      falseReason: NotGreenlit
      message: hello
  - name: second
    match: {apiGroup: example.com, kinds: [B]}
    matchConditions: *optOut
    readyCondition:
      <<: *ready
      message: world
`)

	if len(set.Rules()) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(set.Rules()))
	}
	second := set.Select(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "B"})
	if second.Name != "second" {
		t.Fatalf("selected %q, want second", second.Name)
	}
	if len(second.matchConditions) != 1 {
		t.Fatal("the aliased matchConditions were not applied")
	}
	if second.ready.reason != "Greenlit" {
		t.Fatalf("the merge key did not carry the reason over: %q", second.ready.reason)
	}
	if second.ready.message != "world" {
		t.Fatalf("the merge key was not overridden: %q", second.ready.message)
	}

	skipped := object(map[string]any{
		"metadata": map[string]any{
			"name": "x", "namespace": "ns",
			"annotations": map[string]any{"statuspatcher.dev/skip": "true"},
		},
	})
	matched, errs := second.MatchConditions(context.Background(), set, skipped)
	if len(errs) != 0 {
		t.Fatalf("unexpected evaluation errors: %v", errs)
	}
	if matched {
		t.Fatal("the opt-out annotation should have excluded the object")
	}
}

func TestSelectPrecedence(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: wildcard
    match: {apiGroup: example.com, kinds: ["*"]}
  - name: wildcard-versioned
    match: {apiGroup: example.com, kinds: ["*"], versions: [v1]}
  - name: exact
    match: {apiGroup: example.com, kinds: [Widget]}
  - name: exact-versioned
    match: {apiGroup: example.com, kinds: [Widget], versions: [v1]}
  - name: core
    match: {apiGroup: core, kinds: [ConfigMap]}
`)

	tests := []struct {
		gvk  schema.GroupVersionKind
		want string
	}{
		{gvk: schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}, want: "exact-versioned"},
		{gvk: schema.GroupVersionKind{Group: "example.com", Version: "v2", Kind: "Widget"}, want: "exact"},
		{gvk: schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}, want: "wildcard-versioned"},
		{gvk: schema.GroupVersionKind{Group: "example.com", Version: "v2", Kind: "Gadget"}, want: "wildcard"},
		{gvk: schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}, want: "core"},
		{gvk: schema.GroupVersionKind{Group: "other.io", Version: "v1", Kind: "Thing"}, want: "default"},
	}

	for _, tc := range tests {
		t.Run(tc.gvk.String(), func(t *testing.T) {
			got := set.Select(tc.gvk)
			if got.Name != tc.want {
				t.Fatalf("selected %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestDefaultRuleOnlySetsReady(t *testing.T) {
	set := mustLoad(t, header)
	rule := set.Select(schema.GroupVersionKind{Group: "x.io", Version: "v1", Kind: "Y"})
	if !rule.IsDefault() {
		t.Fatal("expected the default rule")
	}
	evaluation := rule.Evaluate(context.Background(), set, object(map[string]any{}))
	if len(evaluation.Fields) != 0 {
		t.Fatalf("the default rule produced status fields: %v", evaluation.Fields)
	}
	if len(evaluation.Conditions) != 1 {
		t.Fatalf("expected exactly one condition, got %d", len(evaluation.Conditions))
	}
	cond := evaluation.Conditions[0]
	if cond.Type != "Ready" || cond.Status != metav1.ConditionTrue || cond.Reason != "Greenlit" {
		t.Fatalf("unexpected ready condition: %+v", cond)
	}
}

func TestEvaluateValueConversion(t *testing.T) {
	set := mustLoad(t, `apiVersion: statuspatcher.dev/v1alpha1
kind: GreenlightConfig
params:
  base: 4200000000
  names: [a, b]
rules:
  - name: conversion
    match: {apiGroup: example.com, kinds: [Widget]}
    variables:
      - {name: id, expression: "7"}
    status:
      bigInt: "params.base + variables.id"
      text: "'literal'"
      flag: "true"
      list: "params.names"
      nested: "{'a': 1, 'b': [true, 'x']}"
      floating: "1.5"
      omittedOptional: "optional.none()"
      omittedNull: "null"
      presentOptional: "optional.of('here')"
`)

	rule := set.Select(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"})
	evaluation := rule.Evaluate(context.Background(), set, object(map[string]any{}))
	if len(evaluation.Errors) != 0 {
		t.Fatalf("unexpected evaluation errors: %v", evaluation.Errors)
	}

	got := evaluation.Fields
	if value, ok := got["bigInt"].(int64); !ok || value != 4200000007 {
		t.Fatalf("bigInt = %#v, want the int64 4200000007", got["bigInt"])
	}
	if got["text"] != "literal" {
		t.Fatalf("text = %#v", got["text"])
	}
	if got["flag"] != true {
		t.Fatalf("flag = %#v", got["flag"])
	}
	if list, ok := got["list"].([]any); !ok || len(list) != 2 || list[0] != "a" {
		t.Fatalf("list = %#v", got["list"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %#v", got["nested"])
	}
	if value, ok := nested["a"].(int64); !ok || value != 1 {
		t.Fatalf("nested.a = %#v, want an int64", nested["a"])
	}
	if value, ok := got["floating"].(float64); !ok || value != 1.5 {
		t.Fatalf("floating = %#v", got["floating"])
	}
	if _, present := got["omittedOptional"]; present {
		t.Fatal("optional.none() should have been omitted")
	}
	if _, present := got["omittedNull"]; present {
		t.Fatal("null should have been omitted")
	}
	if got["presentOptional"] != "here" {
		t.Fatalf("presentOptional = %#v, want the unwrapped value", got["presentOptional"])
	}
}

func TestEvaluationErrorSemantics(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: broken
    match: {apiGroup: example.com, kinds: [Widget]}
    variables:
      - {name: broken, expression: "object.spec.missing"}
      - {name: fine, expression: "'ok'"}
    status:
      good: "variables.fine"
      bad: "variables.broken"
      alsoBad: "object.spec.nope.deeper"
    readyCondition:
      statusExpression: "object.spec.missing == 'x'"
      reason: Greenlit
      falseReason: NotGreenlit
      messageExpression: "object.spec.missing"
`)

	rule := set.Select(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"})
	evaluation := rule.Evaluate(context.Background(), set, object(map[string]any{
		"metadata": map[string]any{"name": "w", "namespace": "ns"},
		"spec":     map[string]any{},
	}))

	if evaluation.Fields["good"] != "ok" {
		t.Fatalf("a healthy field was dropped: %#v", evaluation.Fields)
	}
	if _, present := evaluation.Fields["bad"]; present {
		t.Fatal("a field whose variable failed should have been omitted")
	}
	if _, present := evaluation.Fields["alsoBad"]; present {
		t.Fatal("a field whose expression failed should have been omitted")
	}
	if evaluation.Ready() != metav1.ConditionTrue {
		t.Fatalf("a failing statusExpression must not block readiness, got %q", evaluation.Ready())
	}
	if evaluation.Conditions[0].Message != "Status set by greenlight" {
		t.Fatalf("a failing messageExpression must fall back to the literal message, got %q",
			evaluation.Conditions[0].Message)
	}

	paths := map[string]bool{}
	for _, err := range evaluation.Errors {
		paths[err.Path] = true
	}
	for _, want := range []string{"status.bad", "status.alsoBad", "readyCondition.statusExpression", "readyCondition.messageExpression"} {
		if !paths[want] {
			t.Fatalf("expected an error for %q, got %v", want, paths)
		}
	}
}

func TestFailingMatchConditionIsTreatedAsTrue(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: broken-match
    match: {apiGroup: example.com, kinds: [Widget]}
    matchConditions:
      - name: boom
        expression: "object.spec.missing == 'x'"
`)
	rule := set.Select(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"})
	matched, errs := rule.MatchConditions(context.Background(), set, object(map[string]any{"spec": map[string]any{}}))
	if !matched {
		t.Fatal("a failing matchCondition must be treated as true")
	}
	if len(errs) != 1 || errs[0].Path != "matchConditions[boom]" {
		t.Fatalf("expected one error for matchConditions[boom], got %v", errs)
	}
}

func TestReadyFalseUsesTheFalseReason(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: annotated
    match: {apiGroup: example.com, kinds: [Widget]}
    readyCondition:
      statusExpression: "object.metadata.?annotations[?'statuspatcher.dev/ready'].orValue('true') != 'false'"
      reason: AllChecksPassed
      falseReason: NotAllChecksPassed
      message: fine
      messageExpression: "object.metadata.?annotations[?'statuspatcher.dev/ready'].orValue('true') == 'false' ? 'forced false' : ''"
`)
	rule := set.Select(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"})

	ready := rule.Evaluate(context.Background(), set, object(map[string]any{
		"metadata": map[string]any{"name": "w", "namespace": "ns"},
	}))
	if ready.Ready() != metav1.ConditionTrue || ready.Conditions[0].Reason != "AllChecksPassed" {
		t.Fatalf("unexpected ready condition: %+v", ready.Conditions[0])
	}
	if ready.Conditions[0].Message != "fine" {
		t.Fatalf("an empty messageExpression must not override the message: %q", ready.Conditions[0].Message)
	}

	notReady := rule.Evaluate(context.Background(), set, object(map[string]any{
		"metadata": map[string]any{
			"name": "w", "namespace": "ns",
			"annotations": map[string]any{"statuspatcher.dev/ready": "false"},
		},
	}))
	if notReady.Ready() != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %q", notReady.Ready())
	}
	if notReady.Conditions[0].Reason != "NotAllChecksPassed" {
		t.Fatalf("expected the falseReason, got %q", notReady.Conditions[0].Reason)
	}
	if notReady.Conditions[0].Message != "forced false" {
		t.Fatalf("expected the computed message, got %q", notReady.Conditions[0].Message)
	}
}

func TestVariableCycleIsReportedNotHung(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: cyclic
    match: {apiGroup: example.com, kinds: [Widget]}
    variables:
      - {name: a, expression: "variables.a"}
    status:
      field: "variables.a"
`)
	rule := set.Select(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"})
	evaluation := rule.Evaluate(context.Background(), set, object(map[string]any{}))
	if _, present := evaluation.Fields["field"]; present {
		t.Fatal("a cyclic variable must not produce a value")
	}
	if len(evaluation.Errors) == 0 {
		t.Fatal("a cyclic variable must be reported")
	}
}

func TestParamsKeepIntegers(t *testing.T) {
	params, err := DecodeParams([]byte(`{"int": 4200000000, "float": 1.5, "nested": {"int": 7}, "list": [1, 2]}`))
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := params["int"].(int64); !ok || value != 4200000000 {
		t.Fatalf("int = %#v, want an int64", params["int"])
	}
	if value, ok := params["float"].(float64); !ok || value != 1.5 {
		t.Fatalf("float = %#v, want a float64", params["float"])
	}
	nested, _ := params["nested"].(map[string]any)
	if value, ok := nested["int"].(int64); !ok || value != 7 {
		t.Fatalf("nested.int = %#v, want an int64", nested["int"])
	}
	list, _ := params["list"].([]any)
	if value, ok := list[0].(int64); !ok || value != 1 {
		t.Fatalf("list[0] = %#v, want an int64", list[0])
	}
}

func TestParseParamValue(t *testing.T) {
	tests := []struct {
		in   string
		want any
	}{
		{in: "10.0.0.0/8", want: "10.0.0.0/8"},
		{in: "42", want: int64(42)},
		{in: "1.5", want: 1.5},
		{in: "true", want: true},
	}
	for _, tc := range tests {
		got, err := ParseParamValue(tc.in)
		if err != nil {
			t.Fatalf("ParseParamValue(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseParamValue(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestWatchPatternsAreDerivedFromMatchBlocks(t *testing.T) {
	set := mustLoad(t, header+`rules:
  - name: a
    match: {apiGroup: netbox.dev, kinds: [PrefixClaim, IpAddressClaim]}
  - name: b
    match: {apiGroup: example.com, kinds: ["*"], versions: [v1]}
  - name: c
    match: {apiGroup: core, kinds: [ConfigMap]}
`)
	got := set.WatchPatterns()
	want := []string{
		"netbox.dev/IpAddressClaim",
		"netbox.dev/PrefixClaim",
		"example.com/v1/*",
		"core/ConfigMap",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestCLIDefaultsAreOverriddenByTheFile(t *testing.T) {
	set, err := LoadBytes([]byte(header+`defaults:
  readyCondition:
    reason: FromFile
    falseReason: NotFromFile
    message: file message
`), Options{ConditionType: "Available", Reason: "FromFlag", FalseReason: "NotFromFlag", Message: "flag message"})
	if err != nil {
		t.Fatal(err)
	}
	evaluation := set.Select(schema.GroupVersionKind{Group: "x", Version: "v1", Kind: "Y"}).
		Evaluate(context.Background(), set, object(map[string]any{}))
	cond := evaluation.Conditions[0]
	if cond.Type != "Available" {
		t.Fatalf("condition type = %q, want Available", cond.Type)
	}
	if cond.Reason != "FromFile" || cond.Message != "file message" {
		t.Fatalf("the file defaults were not applied: %+v", cond)
	}
}
