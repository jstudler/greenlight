// Package patcher watches the resolved resources and keeps their status
// green: it evaluates the matching rule, compares the result with the live
// object and writes the difference with server-side apply.
package patcher

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

// maxUnknownFieldRetries bounds how often one apply is retried while dropping
// fields the CRD schema does not declare.
const maxUnknownFieldRetries = 5

// unknownFieldPattern matches the field paths the API server reports when a
// server-side apply payload contains a field that is not in the schema.
var unknownFieldPattern = regexp.MustCompile(`\.status\.([A-Za-z0-9_-]+)[^:]*: field not declared in schema`)

// apply writes the desired status with server-side apply.
//
// Status fields and the managed conditions go into a single apply, so a
// consumer can never observe Ready=True without the fields it needs.
//
// Unlike a plain update, server-side apply rejects a payload that contains a
// field the CRD schema does not declare: it cannot merge what it cannot type.
// A typo in a rule would therefore block readiness, which is exactly what
// greenlight promises not to do. Such fields are consequently dropped, logged
// at warn level and remembered, so that later reconciles skip them right away.
func (c *Controller) apply(
	ctx context.Context,
	entry *informerEntry,
	obj *unstructured.Unstructured,
	desired Desired,
	currentStatus map[string]any,
) error {
	status := make(map[string]any, len(desired.Fields)+1)
	for field, value := range desired.Fields {
		status[field] = value
	}
	now := metav1.NewTime(c.cfg.Clock.Now())
	status["conditions"] = UpsertConditions(Conditions(currentStatus), desired.Conditions, now)

	options := metav1.ApplyOptions{FieldManager: c.cfg.FieldManager, Force: true}
	if c.cfg.DryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}

	for attempt := 0; ; attempt++ {
		payload := applyPayload(entry, obj, status)
		_, err := c.cfg.Client.Resource(entry.gvr).
			Namespace(obj.GetNamespace()).
			ApplyStatus(ctx, obj.GetName(), payload, options)
		if err == nil {
			return nil
		}

		unknown := unknownStatusFields(err)
		dropped := sets.New[string]()
		for field := range unknown {
			if _, present := status[field]; present {
				delete(status, field)
				dropped.Insert(field)
			}
		}
		if dropped.Len() == 0 || attempt >= maxUnknownFieldRetries {
			return fmt.Errorf("server-side apply: %w", err)
		}

		c.rememberUnknownFields(entry.gvr, dropped)
		c.cfg.Logger.Warn("dropping status fields that the CRD schema does not declare",
			"gvk", entry.gvk.String(), "object", objectKey(obj), "fields", sets.List(dropped))
	}
}

func applyPayload(entry *informerEntry, obj *unstructured.Unstructured, status map[string]any) *unstructured.Unstructured {
	metadata := map[string]any{"name": obj.GetName()}
	if entry.namespaced {
		metadata["namespace"] = obj.GetNamespace()
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": entry.gvk.GroupVersion().String(),
		"kind":       entry.gvk.Kind,
		"metadata":   metadata,
		"status":     status,
	}}
}

// unknownStatusFields extracts the top-level status fields that the API server
// rejected as not declared in the schema.
func unknownStatusFields(err error) sets.Set[string] {
	out := sets.New[string]()
	if err == nil {
		return out
	}
	for _, match := range unknownFieldPattern.FindAllStringSubmatch(err.Error(), -1) {
		if field := strings.TrimSpace(match[1]); field != "" {
			out.Insert(field)
		}
	}
	return out
}

func (c *Controller) rememberUnknownFields(gvr schema.GroupVersionResource, fields sets.Set[string]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	known, ok := c.unknownFields[gvr]
	if !ok {
		known = sets.New[string]()
		c.unknownFields[gvr] = known
	}
	known.Insert(sets.List(fields)...)
}

// knownUnknownFields returns the status fields that this resource's schema has
// already rejected.
func (c *Controller) knownUnknownFields(gvr schema.GroupVersionResource) sets.Set[string] {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.unknownFields[gvr]
}

func objectKey(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}
