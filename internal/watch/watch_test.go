package watch

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	discoveryfake "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestParsePattern(t *testing.T) {
	tests := []struct {
		raw     string
		want    Pattern
		wantErr string
	}{
		{raw: "netbox.dev/PrefixClaim", want: Pattern{Group: "netbox.dev", Kind: "PrefixClaim"}},
		{raw: "netbox.dev/v1/PrefixClaim", want: Pattern{Group: "netbox.dev", Version: "v1", Kind: "PrefixClaim"}},
		{raw: "example.com/*", want: Pattern{Group: "example.com", Kind: "*", AnyKind: true}},
		{raw: "example.com/v1/*", want: Pattern{Group: "example.com", Version: "v1", Kind: "*", AnyKind: true}},
		{raw: "core/ConfigMap", want: Pattern{Group: "", Kind: "ConfigMap"}},
		{raw: "/ConfigMap", want: Pattern{Group: "", Kind: "ConfigMap"}},

		{raw: "PrefixClaim", wantErr: "invalid watch pattern"},
		{raw: "a/b/c/d", wantErr: "invalid watch pattern"},
		{raw: "netbox.dev/", wantErr: "must not be empty"},
		{raw: "netbox.dev//Kind", wantErr: "must not be empty"},
		{raw: "*/Kind", wantErr: "must not be a wildcard"},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParsePattern(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error containing %q, got %v (%+v)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.want.Raw = tc.raw
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParsePatternsSplitsCommas(t *testing.T) {
	got, err := ParsePatterns([]string{"a.io/One, a.io/Two", "b.io/*", "", " "})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d patterns, want 3: %+v", len(got), got)
	}
	if got[1].Kind != "Two" {
		t.Fatalf("the comma-separated list was not split: %+v", got)
	}
}

func newFakeDiscovery() *discoveryfake.FakeDiscovery {
	return &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{
		Resources: []*metav1.APIResourceList{
			{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{
					{Name: "widgetclaims", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"list", "watch"}},
					{Name: "widgetclaims/status", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"patch"}},
					{Name: "gadgets", Kind: "Gadget", Namespaced: false, Verbs: metav1.Verbs{"list", "watch"}},
					{Name: "gadgets/status", Kind: "Gadget", Namespaced: false, Verbs: metav1.Verbs{"patch"}},
					// No status subresource: must be skipped.
					{Name: "notes", Kind: "Note", Namespaced: true, Verbs: metav1.Verbs{"list", "watch"}},
					// No list/watch: must be skipped.
					{Name: "tokens", Kind: "Token", Namespaced: true, Verbs: metav1.Verbs{"create"}},
					{Name: "tokens/status", Kind: "Token", Namespaced: true, Verbs: metav1.Verbs{"patch"}},
				},
			},
			{
				GroupVersion: "example.com/v2",
				APIResources: []metav1.APIResource{
					{Name: "widgetclaims", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"list", "watch"}},
					{Name: "widgetclaims/status", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"patch"}},
				},
			},
		},
	}}
}

func TestResolveWildcardExpansion(t *testing.T) {
	resolver := NewResolver(newFakeDiscovery())
	patterns, err := ParsePatterns([]string{"example.com/v1/*"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := resolver.Resolve(patterns)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, resource := range result.Resources {
		names = append(names, resource.GVR.Resource)
	}
	if len(names) != 2 {
		t.Fatalf("got %v, want gadgets and widgetclaims", names)
	}
	for _, name := range names {
		if strings.Contains(name, "/") {
			t.Fatalf("a subresource leaked into the watch set: %q", name)
		}
	}

	for _, resource := range result.Resources {
		if resource.GVR.Resource == "gadgets" && resource.Namespaced {
			t.Fatal("gadgets is cluster-scoped")
		}
		if resource.GVR.Resource == "widgetclaims" && !resource.Namespaced {
			t.Fatal("widgetclaims is namespaced")
		}
	}

	warnings := strings.Join(result.Warnings, "\n")
	if !strings.Contains(warnings, "notes") || !strings.Contains(warnings, "no /status subresource") {
		t.Fatalf("expected a warning about notes, got: %s", warnings)
	}
	if !strings.Contains(warnings, "tokens") || !strings.Contains(warnings, "list and watch") {
		t.Fatalf("expected a warning about tokens, got: %s", warnings)
	}
}

func TestResolveExactKindAndVersion(t *testing.T) {
	resolver := NewResolver(newFakeDiscovery())
	patterns, err := ParsePatterns([]string{"example.com/v2/WidgetClaim"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := resolver.Resolve(patterns)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 {
		t.Fatalf("got %d resources, want 1", len(result.Resources))
	}
	if result.Resources[0].GVK.Version != "v2" {
		t.Fatalf("got version %q, want v2", result.Resources[0].GVK.Version)
	}
}

func TestResolveMissingKindOnlyWarns(t *testing.T) {
	resolver := NewResolver(newFakeDiscovery())
	patterns, err := ParsePatterns([]string{"example.com/v1/Missing", "absent.io/Thing"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := resolver.Resolve(patterns)
	if err != nil {
		t.Fatalf("an unresolvable pattern must not be fatal: %v", err)
	}
	if len(result.Resources) != 0 {
		t.Fatalf("got %d resources, want 0", len(result.Resources))
	}
	if len(result.Warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %v", result.Warnings)
	}
}

func TestResolveDeduplicates(t *testing.T) {
	resolver := NewResolver(newFakeDiscovery())
	patterns, err := ParsePatterns([]string{"example.com/v1/WidgetClaim", "example.com/v1/*"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := resolver.Resolve(patterns)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, resource := range result.Resources {
		seen[resource.GVR.String()]++
	}
	for gvr, count := range seen {
		if count != 1 {
			t.Fatalf("%s appears %d times", gvr, count)
		}
	}
	if len(result.ByPattern["example.com/v1/WidgetClaim"]) != 1 {
		t.Fatalf("the per-pattern mapping is wrong: %+v", result.ByPattern)
	}
}
