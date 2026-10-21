// Package integration runs greenlight against a real API server provided by
// envtest. The tests are skipped when the control plane binaries are missing;
// run `make envtest-assets` (or setup-envtest) to install them.
package integration

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/jstudler/greenlight/internal/patcher"
	"github.com/jstudler/greenlight/internal/rules"
	"github.com/jstudler/greenlight/internal/watch"
)

const (
	testNamespace = "default"
	waitTimeout   = 30 * time.Second
)

var (
	prefixClaimGVR    = schema.GroupVersionResource{Group: "netbox.dev", Version: "v1", Resource: "prefixclaims"}
	ipAddressClaimGVR = schema.GroupVersionResource{Group: "netbox.dev", Version: "v1", Resource: "ipaddressclaims"}
	widgetClaimGVR    = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgetclaims"}
	gadgetGVR         = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "gadgets"}
	sprocketGVR       = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "sprockets"}
)

var restConfig *rest.Config

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if path := defaultAssetPath(); path != "" {
			_ = os.Setenv("KUBEBUILDER_ASSETS", path)
		}
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		//nolint:forbidigo // this message explains why the suite is skipped
		println("skipping the integration tests: KUBEBUILDER_ASSETS is not set, run `make envtest-assets`")
		os.Exit(0)
	}

	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "testdata", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		panic(err)
	}
	restConfig = config

	code := m.Run()
	if err := environment.Stop(); err != nil {
		panic(err)
	}
	os.Exit(code)
}

// defaultAssetPath looks for binaries installed by setup-envtest.
func defaultAssetPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	roots := []string{
		filepath.Join(home, "Library", "Application Support", "io.kubebuilder.envtest", "k8s"),
		filepath.Join(home, ".local", "share", "kubebuilder-envtest", "k8s"),
		"/usr/local/kubebuilder/bin",
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for i := len(entries) - 1; i >= 0; i-- {
			candidate := filepath.Join(root, entries[i].Name())
			if _, err := os.Stat(filepath.Join(candidate, "kube-apiserver")); err == nil {
				return candidate
			}
		}
		if _, err := os.Stat(filepath.Join(root, "kube-apiserver")); err == nil {
			return root
		}
	}
	return ""
}

// harness owns one running controller plus the clients the tests use.
type harness struct {
	t          *testing.T
	client     dynamic.Interface
	controller *patcher.Controller
	logs       *logCapture
	cancel     context.CancelFunc
}

type options struct {
	rulesFiles       []string
	watch            []string
	removeFinalizers bool
	params           map[string]any
	rediscovery      time.Duration
}

func start(t *testing.T, opts options) *harness {
	t.Helper()

	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("building the dynamic client: %v", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		t.Fatalf("building the discovery client: %v", err)
	}
	cachedDiscovery := memory.NewMemCacheClient(discoveryClient)

	ruleSet, err := rules.Load(opts.rulesFiles, rules.DefaultOptions())
	if err != nil {
		t.Fatalf("loading the rules: %v", err)
	}
	for key, value := range opts.params {
		ruleSet.SetParam(key, value)
	}

	raw := opts.watch
	if len(raw) == 0 {
		raw = ruleSet.WatchPatterns()
	}
	patterns, err := watch.ParsePatterns(raw)
	if err != nil {
		t.Fatalf("parsing the watch patterns: %v", err)
	}

	logs := newLogCapture()
	controller := patcher.New(patcher.Config{
		Client:              client,
		Resolver:            watch.NewResolver(cachedDiscovery),
		Patterns:            patterns,
		Rules:               ruleSet,
		FieldManager:        "greenlight",
		RemoveFinalizers:    opts.removeFinalizers,
		ResyncPeriod:        time.Hour,
		RediscoveryInterval: opts.rediscovery,
		Workers:             2,
		Logger:              slog.New(logs),
		InvalidateDiscovery: cachedDiscovery.Invalidate,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := controller.Run(ctx); err != nil {
			t.Errorf("the controller stopped with an error: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	syncCtx, syncCancel := context.WithTimeout(ctx, waitTimeout)
	defer syncCancel()
	if !controller.WaitForCacheSync(syncCtx) {
		t.Fatal("the informer caches never synced")
	}

	return &harness{t: t, client: client, controller: controller, logs: logs, cancel: cancel}
}

func (h *harness) create(gvr schema.GroupVersionResource, obj map[string]any) *unstructured.Unstructured {
	h.t.Helper()
	created, err := h.client.Resource(gvr).Namespace(testNamespace).
		Create(context.Background(), &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
	if err != nil {
		h.t.Fatalf("creating %s: %v", gvr.Resource, err)
	}
	h.t.Cleanup(func() { h.delete(gvr, created.GetName()) })
	return created
}

func (h *harness) get(gvr schema.GroupVersionResource, name string) (*unstructured.Unstructured, error) {
	return h.client.Resource(gvr).Namespace(testNamespace).
		Get(context.Background(), name, metav1.GetOptions{})
}

func (h *harness) delete(gvr schema.GroupVersionResource, name string) {
	_ = h.client.Resource(gvr).Namespace(testNamespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
}

// await polls the object until check returns true.
func (h *harness) await(gvr schema.GroupVersionResource, name string, check func(*unstructured.Unstructured) bool) *unstructured.Unstructured {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last *unstructured.Unstructured
	for time.Now().Before(deadline) {
		obj, err := h.get(gvr, name)
		if err == nil {
			last = obj
			if check(obj) {
				return obj
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last != nil {
		h.t.Fatalf("timed out waiting for %s/%s; last status: %#v", gvr.Resource, name, last.Object["status"])
	}
	h.t.Fatalf("timed out waiting for %s/%s, which never appeared", gvr.Resource, name)
	return nil
}

func conditionIs(obj *unstructured.Unstructured, conditionType, status string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == conditionType && cond["status"] == status {
			return true
		}
	}
	return false
}

func condition(obj *unstructured.Unstructured, conditionType string) map[string]any {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if ok && cond["type"] == conditionType {
			return cond
		}
	}
	return nil
}

func statusString(obj *unstructured.Unstructured, field string) string {
	value, _, _ := unstructured.NestedString(obj.Object, "status", field)
	return value
}

func exampleRules(name string) string {
	return filepath.Join("..", "..", "examples", name)
}
