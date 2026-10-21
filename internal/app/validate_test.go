package app

import (
	"bytes"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	discoveryfake "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/jstudler/greenlight/internal/rules"
	"github.com/jstudler/greenlight/internal/watch"
)

const testRules = `apiVersion: statuspatcher.dev/v1alpha1
kind: GreenlightConfig
rules:
  - name: widget
    match: {apiGroup: example.com, kinds: [WidgetClaim]}
    status:
      endpoint: "'10.0.0.1'"
  - name: unused
    match: {apiGroup: absent.io, kinds: [Nothing]}
`

func fakeDiscovery() *discoveryfake.FakeDiscovery {
	return &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{
		Resources: []*metav1.APIResourceList{{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgetclaims", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"list", "watch"}},
				{Name: "widgetclaims/status", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"patch"}},
				{Name: "gadgets", Kind: "Gadget", Namespaced: false, Verbs: metav1.Verbs{"list", "watch"}},
				{Name: "gadgets/status", Kind: "Gadget", Namespaced: false, Verbs: metav1.Verbs{"patch"}},
			},
		}},
	}}
}

func load(t *testing.T) *rules.Set {
	t.Helper()
	set, err := rules.LoadBytes([]byte(testRules), rules.DefaultOptions())
	if err != nil {
		t.Fatalf("loading the rules: %v", err)
	}
	return set
}

func TestValidatePrintsTheResolutionTable(t *testing.T) {
	ruleSet := load(t)
	patterns, err := watch.ParsePatterns([]string{"example.com/*", "absent.io/Nothing"})
	if err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := validate(ruleSet, watch.NewResolver(fakeDiscovery()), patterns, out); err != nil {
		t.Fatalf("validate: %v", err)
	}
	text := out.String()

	for _, want := range []string{
		"PATTERN", "example.com/v1, Kind=WidgetClaim", "widgetclaims", "Namespaced", "widget",
		"gadgets", "Cluster", "<default: ready only>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the output to contain %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, `the rule "unused" does not match any watched resource`) {
		t.Fatalf("expected a warning about the unused rule:\n%s", text)
	}
	if !strings.Contains(text, "only gets the default rule") {
		t.Fatalf("expected a warning about the default-only kind:\n%s", text)
	}
}

func TestValidateFailsWhenNothingResolves(t *testing.T) {
	ruleSet := load(t)
	patterns, err := watch.ParsePatterns([]string{"absent.io/Nothing"})
	if err != nil {
		t.Fatal(err)
	}
	err = validate(ruleSet, watch.NewResolver(fakeDiscovery()), patterns, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error when no resource resolves")
	}
	if !strings.Contains(err.Error(), "no watchable resource") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateWithoutAClusterPrintsTheRules(t *testing.T) {
	ruleSet := load(t)
	patterns, err := watch.ParsePatterns([]string{"example.com/*"})
	if err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := validate(ruleSet, nil, patterns, out); err != nil {
		t.Fatalf("validate: %v", err)
	}
	text := out.String()
	for _, want := range []string{"RULE", "widget", "example.com/WidgetClaim", "Ready", "watch: example.com/*"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected the output to contain %q:\n%s", want, text)
		}
	}
}
