package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

func netboxHarness(t *testing.T) *harness {
	t.Helper()
	return start(t, options{
		rulesFiles:       []string{exampleRules("netbox.rules.yaml")},
		watch:            []string{"netbox.dev/PrefixClaim", "netbox.dev/IpAddressClaim"},
		removeFinalizers: true,
	})
}

func prefixClaim(name string, mutate func(map[string]any)) map[string]any {
	obj := map[string]any{
		"apiVersion": "netbox.dev/v1",
		"kind":       "PrefixClaim",
		"metadata":   map[string]any{"name": name, "namespace": testNamespace},
		"spec": map[string]any{
			"prefixLength":         "/25",
			"parentPrefixSelector": map[string]any{"family": "IPv4", "environment": "test"},
		},
	}
	if mutate != nil {
		mutate(obj)
	}
	return obj
}

//  1. A PrefixClaim gets a masked /25 inside the default pool, Ready and
//     PrefixAssigned, and a matching observedGeneration. The value survives a
//     delete and recreate.
func TestPrefixClaimIsGreenlit(t *testing.T) {
	h := netboxHarness(t)
	h.create(prefixClaimGVR, prefixClaim("topology", nil))

	obj := h.await(prefixClaimGVR, "topology", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})

	prefix := statusString(obj, "prefix")
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil {
		t.Fatalf("status.prefix %q is not a CIDR: %v", prefix, err)
	}
	if parsed.Bits() != 25 {
		t.Fatalf("status.prefix %q is not a /25", prefix)
	}
	if parsed.Masked() != parsed {
		t.Fatalf("status.prefix %q is not masked", prefix)
	}
	if !netip.MustParsePrefix("10.0.0.0/8").Contains(parsed.Addr()) {
		t.Fatalf("status.prefix %q is outside the default pool", prefix)
	}
	if statusString(obj, "parentPrefix") != "10.0.0.0/8" {
		t.Fatalf("status.parentPrefix = %q", statusString(obj, "parentPrefix"))
	}
	if statusString(obj, "prefixName") != "topology" {
		t.Fatalf("status.prefixName = %q", statusString(obj, "prefixName"))
	}
	if !conditionIs(obj, "PrefixAssigned", "True") {
		t.Fatal("the PrefixAssigned condition is missing")
	}

	ready := condition(obj, "Ready")
	if ready["reason"] != "PrefixClaimResourceReady" {
		t.Fatalf("the ready reason is %q", ready["reason"])
	}
	if ready["observedGeneration"] != obj.GetGeneration() {
		t.Fatalf("observedGeneration = %v, want %d", ready["observedGeneration"], obj.GetGeneration())
	}
}

// Determinism: deleting and recreating an object must yield the same values,
// so that end-to-end tests can hard-code them.
func TestPrefixClaimAllocationIsDeterministic(t *testing.T) {
	h := netboxHarness(t)

	h.create(prefixClaimGVR, prefixClaim("stable", nil))
	first := h.await(prefixClaimGVR, "stable", func(obj *unstructured.Unstructured) bool {
		return statusString(obj, "prefix") != ""
	})
	prefix := statusString(first, "prefix")

	h.delete(prefixClaimGVR, "stable")
	waitGone(t, h, prefixClaimGVR, "stable")

	h.create(prefixClaimGVR, prefixClaim("stable", nil))
	second := h.await(prefixClaimGVR, "stable", func(obj *unstructured.Unstructured) bool {
		return statusString(obj, "prefix") != ""
	})
	if statusString(second, "prefix") != prefix {
		t.Fatalf("the allocation changed after a recreate: %q then %q", prefix, statusString(second, "prefix"))
	}
}

//  2. An IpAddressClaim gets a host address inside its parent prefix, and
//     status.ipAddress is a /32 host route.
func TestIPAddressClaimIsGreenlit(t *testing.T) {
	h := netboxHarness(t)
	const parent = "10.112.64.0/25"

	h.create(ipAddressClaimGVR, map[string]any{
		"apiVersion": "netbox.dev/v1",
		"kind":       "IpAddressClaim",
		"metadata":   map[string]any{"name": "virtual", "namespace": testNamespace},
		"spec":       map[string]any{"parentPrefix": parent},
	})

	obj := h.await(ipAddressClaimGVR, "virtual", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})

	dotted := statusString(obj, "ipAddressDotDecimal")
	addr, err := netip.ParseAddr(dotted)
	if err != nil {
		t.Fatalf("status.ipAddressDotDecimal %q is not a bare IP: %v", dotted, err)
	}
	if !netip.MustParsePrefix(parent).Contains(addr) {
		t.Fatalf("status.ipAddressDotDecimal %q is outside %s", dotted, parent)
	}
	if got := statusString(obj, "ipAddress"); got != dotted+"/32" {
		t.Fatalf("status.ipAddress = %q, want %q", got, dotted+"/32")
	}
	if !conditionIs(obj, "IPAssigned", "True") {
		t.Fatal("the IPAssigned condition is missing")
	}
}

//  3. Optional fields: a WidgetClaim with spec.generateId gets a numeric
//     generatedId, without it the field stays absent.
func TestOptionalStatusFields(t *testing.T) {
	h := genericHarness(t)

	h.create(widgetClaimGVR, widgetClaim("with-id", map[string]any{"generateId": true}))
	withID := h.await(widgetClaimGVR, "with-id", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})
	generated := statusString(withID, "generatedId")
	if generated == "" {
		t.Fatal("status.generatedId is missing")
	}
	if _, err := strconv.Atoi(generated); err != nil {
		t.Fatalf("status.generatedId %q is not numeric: %v", generated, err)
	}
	upstream, _, _ := unstructured.NestedStringSlice(withID.Object, "status", "upstreamIds")
	if len(upstream) == 0 {
		t.Fatal("status.upstreamIds must not be empty")
	}
	if statusString(withID, "lastOperationStatus") != "COMPLETED" {
		t.Fatalf("status.lastOperationStatus = %q", statusString(withID, "lastOperationStatus"))
	}

	h.create(widgetClaimGVR, widgetClaim("without-id", map[string]any{"generateId": false}))
	withoutID := h.await(widgetClaimGVR, "without-id", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})
	if _, found, _ := unstructured.NestedString(withoutID.Object, "status", "generatedId"); found {
		t.Fatal("status.generatedId must be absent when spec.generateId is false")
	}
}

//  4. A wildcard watch covers every kind of the group that has a status
//     subresource.
func TestWildcardWatchCoversTheWholeGroup(t *testing.T) {
	h := genericHarness(t)

	h.create(gadgetGVR, map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Gadget",
		"metadata":   map[string]any{"name": "wildcard", "namespace": testNamespace},
		"spec":       map[string]any{"size": "small"},
	})

	obj := h.await(gadgetGVR, "wildcard", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})
	ready := condition(obj, "Ready")
	if ready["message"] != "Reconciled by greenlight" {
		t.Fatalf("the catch-all rule did not apply: %#v", ready)
	}
}

// 5. A CRD installed after startup is picked up by the rediscovery loop.
func TestLateCRDIsPickedUp(t *testing.T) {
	h := start(t, options{
		rulesFiles:       []string{exampleRules("generic.rules.yaml")},
		watch:            []string{"example.com/*"},
		removeFinalizers: true,
		rediscovery:      time.Second,
	})

	installCRD(t, filepath.Join("..", "testdata", "crds-late", "example.com_sprockets.yaml"))

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		_, err := h.client.Resource(sprocketGVR).Namespace(testNamespace).
			Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "example.com/v1",
				"kind":       "Sprocket",
				"metadata":   map[string]any{"name": "late", "namespace": testNamespace},
			}}, metav1.CreateOptions{})
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(func() { h.delete(sprocketGVR, "late") })

	h.await(sprocketGVR, "late", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})
}

//  6. A spec change bumps the generation, which must trigger exactly one more
//     apply. There must be no patch loop.
func TestGenerationBumpRepatchesWithoutLooping(t *testing.T) {
	h := genericHarness(t)
	h.create(widgetClaimGVR, widgetClaim("bumped", map[string]any{"size": "small"}))

	first := h.await(widgetClaimGVR, "bumped", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})
	firstGeneration := condition(first, "Ready")["observedGeneration"]

	// Let any spurious re-reconcile happen before counting.
	time.Sleep(time.Second)
	appliesBefore := h.logs.countWhere(slog.LevelInfo, "status applied", "object", "bumped")

	patch := []byte(`{"spec":{"size":"large"}}`)
	if _, err := h.client.Resource(widgetClaimGVR).Namespace(testNamespace).
		Patch(context.Background(), "bumped", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("patching the spec: %v", err)
	}

	h.await(widgetClaimGVR, "bumped", func(obj *unstructured.Unstructured) bool {
		return condition(obj, "Ready")["observedGeneration"] != firstGeneration
	})

	time.Sleep(2 * time.Second)
	appliesAfter := h.logs.countWhere(slog.LevelInfo, "status applied", "object", "bumped")
	if delta := appliesAfter - appliesBefore; delta != 1 {
		t.Fatalf("expected exactly one more apply after the generation bump, got %d", delta)
	}
}

// 7. The ready and skip annotations drive failure injection.
func TestReadyAndSkipAnnotations(t *testing.T) {
	h := netboxHarness(t)

	h.create(prefixClaimGVR, prefixClaim("annotated", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["annotations"] = map[string]any{"statuspatcher.dev/ready": "false"}
	}))
	notReady := h.await(prefixClaimGVR, "annotated", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "False")
	})
	ready := condition(notReady, "Ready")
	if ready["reason"] != "PrefixClaimResourceNotReady" {
		t.Fatalf("expected the falseReason, got %q", ready["reason"])
	}
	if !strings.Contains(fmt.Sprint(ready["message"]), "annotation") {
		t.Fatalf("expected the computed message, got %q", ready["message"])
	}

	// Removing the annotation flips Ready back to True without a generation
	// bump, which proves that the diff check looks at the computed status.
	patch := []byte(`[{"op":"remove","path":"/metadata/annotations/statuspatcher.dev~1ready"}]`)
	if _, err := h.client.Resource(prefixClaimGVR).Namespace(testNamespace).
		Patch(context.Background(), "annotated", types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("removing the annotation: %v", err)
	}
	h.await(prefixClaimGVR, "annotated", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})

	h.create(prefixClaimGVR, prefixClaim("skipped", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["annotations"] = map[string]any{"statuspatcher.dev/skip": "true"}
	}))
	time.Sleep(2 * time.Second)
	obj, err := h.get(prefixClaimGVR, "skipped")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions"); found {
		t.Fatalf("an object annotated with skip must stay untouched: %#v", obj.Object["status"])
	}
}

//  8. A failing status expression omits only that field, keeps Ready=True and
//     logs the error at most once per resync period.
func TestFailingExpressionOmitsOnlyThatField(t *testing.T) {
	rulesFile := writeRules(t, `apiVersion: statuspatcher.dev/v1alpha1
kind: GreenlightConfig
rules:
  - name: widget
    match: {apiGroup: example.com, kinds: [WidgetClaim]}
    status:
      widgetId: "'fine'"
      endpoint: "object.spec.doesNotExist.nested"
`)
	h := start(t, options{
		rulesFiles:       []string{rulesFile},
		watch:            []string{"example.com/v1/WidgetClaim"},
		removeFinalizers: true,
	})

	h.create(widgetClaimGVR, widgetClaim("broken", map[string]any{}))
	obj := h.await(widgetClaimGVR, "broken", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})

	if statusString(obj, "widgetId") != "fine" {
		t.Fatalf("the healthy field was dropped: %#v", obj.Object["status"])
	}
	if _, found, _ := unstructured.NestedString(obj.Object, "status", "endpoint"); found {
		t.Fatal("the failing field should have been omitted")
	}

	time.Sleep(time.Second)
	if count := h.logs.countWhere(slog.LevelError, "expression evaluation failed", "path", "status.endpoint"); count != 1 {
		t.Fatalf("expected the evaluation error to be logged exactly once, got %d", count)
	}
}

//  9. An unknown status field must not block the apply: it is dropped, logged
//     at warn level, and the ready condition is still written.
func TestUnknownStatusFieldDoesNotFailTheApply(t *testing.T) {
	rulesFile := writeRules(t, `apiVersion: statuspatcher.dev/v1alpha1
kind: GreenlightConfig
rules:
  - name: gadget
    match: {apiGroup: example.com, kinds: [Gadget]}
    status:
      totallyUnknownField: "'value'"
`)
	h := start(t, options{
		rulesFiles:       []string{rulesFile},
		watch:            []string{"example.com/v1/Gadget"},
		removeFinalizers: true,
	})

	h.create(gadgetGVR, map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Gadget",
		"metadata":   map[string]any{"name": "unknown-field", "namespace": testNamespace},
	})

	obj := h.await(gadgetGVR, "unknown-field", func(obj *unstructured.Unstructured) bool {
		return conditionIs(obj, "Ready", "True")
	})
	if _, found, _ := unstructured.NestedString(obj.Object, "status", "totallyUnknownField"); found {
		t.Fatal("the unknown field should never have reached the API server")
	}
	if count := h.logs.countWhere(slog.LevelWarn, "does not declare", "fields", "totallyUnknownField"); count == 0 {
		t.Fatal("the dropped field was not logged at warn level")
	}
}

//  10. Finalizers are removed by default, a terminating object disappears, and
//     --remove-finalizers=false leaves them alone.
func TestFinalizerHandling(t *testing.T) {
	t.Run("removed by default", func(t *testing.T) {
		h := genericHarness(t)
		h.create(widgetClaimGVR, widgetClaim("finalized", nil, func(obj map[string]any) {
			obj["metadata"].(map[string]any)["finalizers"] = []any{"example.com/protect"}
		}))

		h.await(widgetClaimGVR, "finalized", func(obj *unstructured.Unstructured) bool {
			return len(obj.GetFinalizers()) == 0
		})
	})

	t.Run("a terminating object is released", func(t *testing.T) {
		h := start(t, options{
			rulesFiles:       []string{exampleRules("generic.rules.yaml")},
			watch:            []string{"example.com/*"},
			removeFinalizers: false,
		})
		h.create(widgetClaimGVR, widgetClaim("terminating", nil, func(obj map[string]any) {
			obj["metadata"].(map[string]any)["finalizers"] = []any{"example.com/protect"}
		}))
		h.await(widgetClaimGVR, "terminating", func(obj *unstructured.Unstructured) bool {
			return conditionIs(obj, "Ready", "True")
		})
		h.delete(widgetClaimGVR, "terminating")

		// With --remove-finalizers=false the object stays in Terminating.
		time.Sleep(2 * time.Second)
		obj, err := h.get(widgetClaimGVR, "terminating")
		if err != nil {
			t.Fatalf("the object disappeared even though the finalizer was kept: %v", err)
		}
		if obj.GetDeletionTimestamp() == nil {
			t.Fatal("the object should be terminating")
		}
		if len(obj.GetFinalizers()) != 1 {
			t.Fatalf("--remove-finalizers=false must keep the finalizers, got %v", obj.GetFinalizers())
		}

		// A second greenlight with the default setting releases it.
		releaser := genericHarness(t)
		waitGone(t, releaser, widgetClaimGVR, "terminating")
	})
}

func genericHarness(t *testing.T) *harness {
	t.Helper()
	return start(t, options{
		rulesFiles:       []string{exampleRules("generic.rules.yaml")},
		watch:            []string{"example.com/*"},
		removeFinalizers: true,
	})
}

func widgetClaim(name string, spec map[string]any, mutate ...func(map[string]any)) map[string]any {
	if spec == nil {
		spec = map[string]any{}
	}
	obj := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "WidgetClaim",
		"metadata":   map[string]any{"name": name, "namespace": testNamespace},
		"spec":       spec,
	}
	for _, fn := range mutate {
		fn(obj)
	}
	return obj
}

func waitGone(t *testing.T, h *harness, gvr schema.GroupVersionResource, name string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		_, err := h.get(gvr, name)
		if apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s/%s did not disappear", gvr.Resource, name)
}

func writeRules(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func installCRD(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatal(err)
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	gvr := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(crd)
	if err != nil {
		t.Fatal(err)
	}
	// The CRD is deliberately left installed: removing it again would make the
	// informers of later tests fail to list a resource that discovery still
	// advertises.
	if _, err := client.Resource(gvr).Create(context.Background(),
		&unstructured.Unstructured{Object: raw}, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("installing the CRD: %v", err)
	}
}
