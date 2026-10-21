package patcher

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/jstudler/greenlight/internal/rules"
	"github.com/jstudler/greenlight/internal/watch"
)

var widgetGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgetclaims"}

const testRules = `apiVersion: statuspatcher.dev/v1alpha1
kind: GreenlightConfig
rules:
  - name: widget
    match: {apiGroup: example.com, kinds: [WidgetClaim]}
    matchConditions:
      - name: not-opted-out
        expression: "object.metadata.?annotations[?'statuspatcher.dev/skip'].orValue('') != 'true'"
    status:
      endpoint: "'10.0.0.1'"
    readyCondition:
      statusExpression: "object.metadata.?annotations[?'statuspatcher.dev/ready'].orValue('true') != 'false'"
      reason: Greenlit
      falseReason: NotGreenlit
      message: ok
    conditions:
      - {type: EndpointAssigned, reason: EndpointCreated, falseReason: EndpointNotCreated, message: ""}
`

type capture struct {
	mu          sync.Mutex
	applies     []map[string]any
	mergePatche []map[string]any
}

func (c *capture) addApply(payload map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applies = append(c.applies, payload)
}

func (c *capture) addMerge(payload map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergePatche = append(c.mergePatche, payload)
}

func (c *capture) appliesCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.applies)
}

func (c *capture) lastApply() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applies) == 0 {
		return nil
	}
	return c.applies[len(c.applies)-1]
}

func (c *capture) merges() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.mergePatche...)
}

func widget(name string, mutate func(obj map[string]any)) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "WidgetClaim",
		"metadata": map[string]any{
			"name":       name,
			"namespace":  "default",
			"generation": int64(1),
			"uid":        "uid-" + name,
		},
		"spec": map[string]any{},
	}
	if mutate != nil {
		mutate(obj)
	}
	return &unstructured.Unstructured{Object: obj}
}

func newTestController(t *testing.T, cfgMutate func(*Config), objects ...runtime.Object) (*Controller, *capture, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{widgetGVR: "WidgetClaimList"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)

	captured := &capture{}
	client.PrependReactor("patch", "widgetclaims", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok {
			return false, nil, nil
		}
		payload := map[string]any{}
		if err := json.Unmarshal(patch.GetPatch(), &payload); err != nil {
			return true, nil, err
		}
		switch patch.GetPatchType() {
		case types.ApplyPatchType:
			captured.addApply(payload)
		case types.MergePatchType:
			captured.addMerge(payload)
		}
		return true, &unstructured.Unstructured{Object: payload}, nil
	})

	discovery := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{
		Resources: []*metav1.APIResourceList{{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgetclaims", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"list", "watch", "get", "patch"}},
				{Name: "widgetclaims/status", Kind: "WidgetClaim", Namespaced: true, Verbs: metav1.Verbs{"get", "patch"}},
			},
		}},
	}}

	ruleSet, err := rules.LoadBytes([]byte(testRules), rules.DefaultOptions())
	if err != nil {
		t.Fatalf("loading the test rules: %v", err)
	}
	patterns, err := watch.ParsePatterns([]string{"example.com/*"})
	if err != nil {
		t.Fatalf("parsing the patterns: %v", err)
	}

	cfg := Config{
		Client:              client,
		Resolver:            watch.NewResolver(discovery),
		Patterns:            patterns,
		Rules:               ruleSet,
		FieldManager:        "greenlight",
		RemoveFinalizers:    true,
		ResyncPeriod:        time.Hour,
		RediscoveryInterval: 0,
		Workers:             1,
	}
	if cfgMutate != nil {
		cfgMutate(&cfg)
	}
	return New(cfg), captured, client
}

func runController(t *testing.T, controller *Controller) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := controller.Run(ctx); err != nil {
			t.Errorf("Run returned an error: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if !controller.WaitForCacheSync(ctxWithTimeout(t, 10*time.Second)) {
		t.Fatal("the informer caches never synced")
	}
	return cancel
}

func ctxWithTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestControllerAppliesStatusInOnePayload(t *testing.T) {
	controller, captured, _ := newTestController(t, nil, widget("one", nil))
	runController(t, controller)

	if !eventually(t, 10*time.Second, func() bool { return captured.appliesCount() > 0 }) {
		t.Fatal("no status was applied")
	}

	payload := captured.lastApply()
	if payload["apiVersion"] != "example.com/v1" || payload["kind"] != "WidgetClaim" {
		t.Fatalf("the apply payload is missing its type: %#v", payload)
	}
	metadata, _ := payload["metadata"].(map[string]any)
	if metadata["name"] != "one" || metadata["namespace"] != "default" {
		t.Fatalf("the apply payload is missing its identity: %#v", metadata)
	}
	if _, present := payload["spec"]; present {
		t.Fatalf("the apply payload must only carry the status: %#v", payload)
	}

	status, _ := payload["status"].(map[string]any)
	if status["endpoint"] != "10.0.0.1" {
		t.Fatalf("status.endpoint = %#v", status["endpoint"])
	}

	conditions, _ := status["conditions"].([]any)
	if len(conditions) != 2 {
		t.Fatalf("expected 2 conditions in the same payload, got %#v", status["conditions"])
	}
	ready, _ := conditions[0].(map[string]any)
	if ready["type"] != "Ready" || ready["status"] != "True" || ready["reason"] != "Greenlit" {
		t.Fatalf("unexpected ready condition: %#v", ready)
	}
	if ready["observedGeneration"] != float64(1) {
		t.Fatalf("observedGeneration = %#v, want 1", ready["observedGeneration"])
	}
	if ready["lastTransitionTime"] == "" {
		t.Fatal("lastTransitionTime was not set")
	}
}

func TestControllerRemovesFinalizers(t *testing.T) {
	object := widget("final", func(obj map[string]any) {
		metadata := obj["metadata"].(map[string]any)
		metadata["finalizers"] = []any{"example.com/protect", "other.io/keep"}
		metadata["resourceVersion"] = "42"
	})
	controller, captured, _ := newTestController(t, nil, object)
	runController(t, controller)

	if !eventually(t, 10*time.Second, func() bool { return len(captured.merges()) > 0 }) {
		t.Fatal("the finalizers were never removed")
	}

	patch := captured.merges()[0]
	metadata, _ := patch["metadata"].(map[string]any)
	if value, present := metadata["finalizers"]; !present || value != nil {
		t.Fatalf("the merge patch must set finalizers to null: %#v", metadata)
	}
	if metadata["resourceVersion"] != "42" {
		t.Fatalf("the merge patch must be optimistically locked: %#v", metadata)
	}
}

func TestControllerKeepsFinalizersWhenDisabled(t *testing.T) {
	object := widget("keep", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["finalizers"] = []any{"example.com/protect"}
	})
	controller, captured, _ := newTestController(t, func(cfg *Config) {
		cfg.RemoveFinalizers = false
	}, object)
	runController(t, controller)

	if !eventually(t, 10*time.Second, func() bool { return captured.appliesCount() > 0 }) {
		t.Fatal("no status was applied")
	}
	if merges := captured.merges(); len(merges) != 0 {
		t.Fatalf("--remove-finalizers=false must not patch finalizers: %#v", merges)
	}
}

func TestControllerSkipsOptedOutObjects(t *testing.T) {
	object := widget("skipped", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["annotations"] = map[string]any{"statuspatcher.dev/skip": "true"}
	})
	controller, captured, _ := newTestController(t, nil, object)
	runController(t, controller)

	// Give the worker a chance to do the wrong thing.
	time.Sleep(300 * time.Millisecond)
	if captured.appliesCount() != 0 {
		t.Fatalf("an opted-out object must not be patched: %#v", captured.lastApply())
	}
}

func TestControllerHonoursTheReadyAnnotation(t *testing.T) {
	object := widget("notready", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["annotations"] = map[string]any{"statuspatcher.dev/ready": "false"}
	})
	controller, captured, _ := newTestController(t, nil, object)
	runController(t, controller)

	if !eventually(t, 10*time.Second, func() bool { return captured.appliesCount() > 0 }) {
		t.Fatal("no status was applied")
	}
	status := captured.lastApply()["status"].(map[string]any)
	conditions := status["conditions"].([]any)
	ready := conditions[0].(map[string]any)
	if ready["status"] != "False" || ready["reason"] != "NotGreenlit" {
		t.Fatalf("expected Ready=False with the falseReason: %#v", ready)
	}
}

func TestControllerDoesNotPatchTerminatingObjects(t *testing.T) {
	object := widget("terminating", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["deletionTimestamp"] = "2025-01-01T00:00:00Z"
	})
	controller, captured, _ := newTestController(t, nil, object)
	runController(t, controller)

	time.Sleep(300 * time.Millisecond)
	if captured.appliesCount() != 0 {
		t.Fatalf("a terminating object must not be patched: %#v", captured.lastApply())
	}
}

func TestControllerDoesNotLoopOnItsOwnUpdates(t *testing.T) {
	// The object already carries exactly the status the rule computes, so the
	// diff check must suppress the patch entirely.
	object := widget("steady", func(obj map[string]any) {
		obj["metadata"].(map[string]any)["managedFields"] = []any{map[string]any{
			"manager":     "greenlight",
			"operation":   "Apply",
			"subresource": "status",
			"fieldsV1": map[string]any{
				"f:status": map[string]any{"f:endpoint": map[string]any{}, "f:conditions": map[string]any{}},
			},
		}}
		obj["status"] = map[string]any{
			"endpoint": "10.0.0.1",
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "True", "reason": "Greenlit", "message": "ok",
					"observedGeneration": int64(1), "lastTransitionTime": "2025-01-01T00:00:00Z",
				},
				map[string]any{
					"type": "EndpointAssigned", "status": "True", "reason": "EndpointCreated", "message": "",
					"observedGeneration": int64(1), "lastTransitionTime": "2025-01-01T00:00:00Z",
				},
			},
		}
	})
	controller, captured, _ := newTestController(t, nil, object)
	runController(t, controller)

	time.Sleep(300 * time.Millisecond)
	if captured.appliesCount() != 0 {
		t.Fatalf("an up-to-date object must not be patched again: %#v", captured.lastApply())
	}
}

func TestControllerDryRunUsesServerSideDryRun(t *testing.T) {
	controller, captured, client := newTestController(t, func(cfg *Config) {
		cfg.DryRun = true
	}, widget("dry", nil))

	var options []string
	client.PrependReactor("patch", "widgetclaims", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if patch, ok := action.(clienttesting.PatchActionImpl); ok {
			options = append(options, patch.PatchOptions.DryRun...)
		}
		return false, nil, nil
	})

	runController(t, controller)
	if !eventually(t, 10*time.Second, func() bool { return captured.appliesCount() > 0 }) {
		t.Fatal("no status was applied")
	}
	if len(options) == 0 || options[0] != metav1.DryRunAll {
		t.Fatalf("--dry-run must set dryRun=All, got %v", options)
	}
}

func TestControllerStopsWatchingWhenTheResourceDisappears(t *testing.T) {
	controller, _, _ := newTestController(t, nil, widget("gone", nil))
	runController(t, controller)

	if !eventually(t, 10*time.Second, func() bool { return controller.InformerCount() == 1 }) {
		t.Fatalf("expected 1 informer, got %d", controller.InformerCount())
	}

	controller.cfg.Resolver = watch.NewResolver(&discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}})
	if err := controller.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if count := controller.InformerCount(); count != 0 {
		t.Fatalf("expected the informer to be stopped, got %d", count)
	}
}
