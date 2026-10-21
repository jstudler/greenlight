package app

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/jstudler/greenlight/internal/rules"
	"github.com/jstudler/greenlight/internal/watch"
)

// validate implements --validate: it resolves every watch pattern against
// discovery, maps the resulting kinds to rules and prints the table. It fails
// when nothing could be resolved at all.
//
// A nil resolver means that no cluster is reachable. The rules have already
// been compiled at that point, so the patterns and rules are printed and the
// check succeeds.
func validate(ruleSet *rules.Set, resolver *watch.Resolver, patterns []watch.Pattern, out io.Writer) error {
	if resolver == nil {
		return printRulesOnly(ruleSet, patterns, out)
	}

	result, err := resolver.Resolve(patterns)
	if err != nil {
		return err
	}

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "PATTERN\tGROUP/VERSION/KIND\tRESOURCE\tSCOPE\tRULE")

	patternOf := map[string][]string{}
	for pattern, resources := range result.ByPattern {
		for _, resource := range resources {
			patternOf[resource.GVR.String()] = append(patternOf[resource.GVR.String()], pattern)
		}
	}

	usedRules := map[string]bool{}
	defaultOnly := []string{}
	for _, resource := range result.Resources {
		rule := ruleSet.Select(resource.GVK)
		usedRules[rule.Name] = true
		scope := "Cluster"
		if resource.Namespaced {
			scope = "Namespaced"
		}
		name := rule.Name
		if rule.IsDefault() {
			name = "<default: ready only>"
			defaultOnly = append(defaultOnly, resource.GVK.String())
		}
		sources := patternOf[resource.GVR.String()]
		sort.Strings(sources)
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			strings.Join(sources, ","), resource.GVK.String(), resource.GVR.Resource, scope, name)
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", warning)
	}
	for _, kind := range defaultOnly {
		_, _ = fmt.Fprintf(out, "warning: %s only gets the default rule (ready condition only)\n", kind)
	}
	for _, rule := range ruleSet.Rules() {
		if !usedRules[rule.Name] {
			_, _ = fmt.Fprintf(out, "warning: the rule %q does not match any watched resource\n", rule.Name)
		}
	}

	if len(result.Resources) == 0 {
		return fmt.Errorf("no watchable resource was resolved from %d pattern(s)", len(patterns))
	}
	return nil
}

// printRulesOnly reports the compiled rules and the watch patterns without
// consulting the API server.
func printRulesOnly(ruleSet *rules.Set, patterns []watch.Pattern, out io.Writer) error {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RULE\tMATCHES\tSTATUS FIELDS\tCONDITIONS")
	for _, rule := range ruleSet.Rules() {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%d\t%s\n",
			rule.Name, strings.Join(rule.WatchPatterns(), ","),
			rule.StatusFieldCount(), strings.Join(rule.ConditionTypes(), ","))
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, pattern := range patterns {
		_, _ = fmt.Fprintf(out, "watch: %s\n", pattern.Raw)
	}
	return nil
}
