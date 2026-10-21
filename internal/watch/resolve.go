package watch

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// Resolver turns patterns into resources using discovery. A resource is only
// returned when it supports list and watch and has a /status subresource:
// greenlight can only write status subresources.
type Resolver struct {
	discovery discovery.DiscoveryInterface
}

// NewResolver returns a resolver backed by the given discovery client.
func NewResolver(d discovery.DiscoveryInterface) *Resolver {
	return &Resolver{discovery: d}
}

// Result is the outcome of resolving a set of patterns.
type Result struct {
	// Resources is the deduplicated set of watchable resources, sorted.
	Resources []Resource
	// ByPattern maps each pattern to the resources it resolved to.
	ByPattern map[string][]Resource
	// Warnings describes patterns that could not (yet) be resolved, and
	// resources that had to be skipped.
	Warnings []string
}

// Resolve resolves all patterns. Unresolvable patterns produce a warning rather
// than an error: a CRD may simply not be installed yet, and the rediscovery
// loop will pick it up later.
func (r *Resolver) Resolve(patterns []Pattern) (*Result, error) {
	result := &Result{ByPattern: map[string][]Resource{}}
	seen := map[schema.GroupVersionResource]bool{}

	groupVersions := map[string]*metav1.APIResourceList{}
	loadGroupVersion := func(gv string) (*metav1.APIResourceList, error) {
		if list, ok := groupVersions[gv]; ok {
			return list, nil
		}
		list, err := r.discovery.ServerResourcesForGroupVersion(gv)
		if err != nil {
			return nil, err
		}
		groupVersions[gv] = list
		return list, nil
	}

	preferred, err := r.preferredVersions()
	if err != nil {
		return nil, fmt.Errorf("listing API groups: %w", err)
	}

	for _, p := range patterns {
		version := p.Version
		if version == "" {
			v, ok := preferred[p.Group]
			if !ok {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("watch %q: the API group %q is not served (yet)", p.Raw, groupName(p.Group)))
				continue
			}
			version = v
		}
		gv := schema.GroupVersion{Group: p.Group, Version: version}
		list, err := loadGroupVersion(gv.String())
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("watch %q: cannot discover %s: %v", p.Raw, gv, err))
			continue
		}

		statusCapable := statusSubresources(list)
		matched := false
		for i := range list.APIResources {
			res := list.APIResources[i]
			if strings.Contains(res.Name, "/") {
				continue
			}
			if !p.AnyKind && res.Kind != p.Kind {
				continue
			}
			matched = true
			if !supportsVerbs(res, "list", "watch") {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("watch %q: skipping %s/%s because it does not support list and watch",
						p.Raw, gv, res.Name))
				continue
			}
			if !statusCapable[res.Name] {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("watch %q: skipping %s/%s because it has no /status subresource",
						p.Raw, gv, res.Name))
				continue
			}
			resource := Resource{
				GVR:        gv.WithResource(res.Name),
				GVK:        gv.WithKind(res.Kind),
				Namespaced: res.Namespaced,
			}
			result.ByPattern[p.Raw] = append(result.ByPattern[p.Raw], resource)
			if !seen[resource.GVR] {
				seen[resource.GVR] = true
				result.Resources = append(result.Resources, resource)
			}
		}
		if !matched {
			what := "no resource"
			if !p.AnyKind {
				what = fmt.Sprintf("the kind %q", p.Kind)
			}
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("watch %q: %s found in %s (is the CRD installed?)", p.Raw, what, gv))
		}
	}

	sort.Slice(result.Resources, func(i, j int) bool {
		return result.Resources[i].GVR.String() < result.Resources[j].GVR.String()
	})
	return result, nil
}

// preferredVersions maps an API group to its preferred version.
func (r *Resolver) preferredVersions() (map[string]string, error) {
	groups, err := r.discovery.ServerGroups()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range groups.Groups {
		g := groups.Groups[i]
		version := g.PreferredVersion.Version
		if version == "" && len(g.Versions) > 0 {
			version = g.Versions[0].Version
		}
		if version != "" {
			out[g.Name] = version
		}
	}
	return out, nil
}

// statusSubresources returns the set of resource names that have a /status
// subresource in the given list.
func statusSubresources(list *metav1.APIResourceList) map[string]bool {
	out := map[string]bool{}
	if list == nil {
		return out
	}
	for i := range list.APIResources {
		name := list.APIResources[i].Name
		if parent, ok := strings.CutSuffix(name, "/status"); ok {
			out[parent] = true
		}
	}
	return out
}

func supportsVerbs(res metav1.APIResource, verbs ...string) bool {
	have := map[string]bool{}
	for _, v := range res.Verbs {
		have[v] = true
	}
	for _, v := range verbs {
		if !have[v] {
			return false
		}
	}
	return true
}

func groupName(group string) string {
	if group == "" {
		return "core"
	}
	return group
}
