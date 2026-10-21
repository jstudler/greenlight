package patcher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"

	"github.com/jstudler/greenlight/internal/metrics"
	"github.com/jstudler/greenlight/internal/rules"
	"github.com/jstudler/greenlight/internal/watch"
)

// Config configures the controller.
type Config struct {
	Client   dynamic.Interface
	Resolver *watch.Resolver
	Patterns []watch.Pattern
	Rules    *rules.Set

	Namespaces    []string
	LabelSelector string

	FieldManager        string
	RemoveFinalizers    bool
	Delay               time.Duration
	ResyncPeriod        time.Duration
	RediscoveryInterval time.Duration
	Workers             int
	DryRun              bool

	Logger  *slog.Logger
	Metrics *metrics.Metrics

	// InvalidateDiscovery is called before every rediscovery so that a cached
	// discovery client notices CRDs that were installed in the meantime.
	InvalidateDiscovery func()

	// Clock is used for condition timestamps and for the artificial delay. It
	// exists for tests.
	Clock clock.Clock
}

type queueKey struct {
	gvr            schema.GroupVersionResource
	watchNamespace string
	namespace      string
	name           string
}

func (k queueKey) cacheKey() string {
	if k.namespace == "" {
		return k.name
	}
	return k.namespace + "/" + k.name
}

func (k queueKey) String() string {
	return k.gvr.String() + " " + k.cacheKey()
}

type informerKey struct {
	gvr       schema.GroupVersionResource
	namespace string
}

type informerEntry struct {
	gvr        schema.GroupVersionResource
	gvk        schema.GroupVersionKind
	namespaced bool
	informer   informers.GenericInformer
	cancel     context.CancelFunc
}

// Controller watches the resolved resources and keeps their status green.
type Controller struct {
	cfg   Config
	queue workqueue.TypedRateLimitingInterface[queueKey]

	mu        sync.RWMutex
	informers map[informerKey]*informerEntry
	delayed   map[queueKey]time.Time
	warned    map[string]bool
	// unknownFields remembers, per resource, the status fields that the CRD
	// schema rejected, so that they are skipped instead of retried forever.
	unknownFields map[schema.GroupVersionResource]sets.Set[string]
	// refreshed is set once the watch patterns have been resolved for the
	// first time, so that the readiness probe does not report success before
	// anything is actually being watched.
	refreshed bool

	errorLog *errorLog

	ctx context.Context
}

// New creates a controller. The configuration is validated by the caller.
func New(cfg Config) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New(nil)
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	return &Controller{
		cfg: cfg,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[queueKey](),
			workqueue.TypedRateLimitingQueueConfig[queueKey]{Name: "greenlight"},
		),
		informers:     map[informerKey]*informerEntry{},
		delayed:       map[queueKey]time.Time{},
		warned:        map[string]bool{},
		unknownFields: map[schema.GroupVersionResource]sets.Set[string]{},
		errorLog:      newErrorLog(cfg.ResyncPeriod, cfg.Clock),
	}
}

// Run starts the informers and the workers and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	c.ctx = ctx
	defer c.queue.ShutDown()

	if err := c.Refresh(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c.processNext(ctx) {
			}
		}()
	}

	if c.cfg.RediscoveryInterval > 0 {
		go c.rediscoveryLoop(ctx)
	}

	<-ctx.Done()
	c.queue.ShutDown()
	wg.Wait()
	c.stopAll()
	return nil
}

func (c *Controller) rediscoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.RediscoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.cfg.InvalidateDiscovery != nil {
				c.cfg.InvalidateDiscovery()
			}
			if err := c.Refresh(); err != nil {
				c.cfg.Logger.Warn("rediscovery failed", "error", err)
			}
		}
	}
}

// Refresh re-resolves the watch patterns and starts or stops informers so that
// the running set matches. CRDs that are installed later are picked up here.
func (c *Controller) Refresh() error {
	result, err := c.cfg.Resolver.Resolve(c.cfg.Patterns)
	if err != nil {
		return fmt.Errorf("resolving the watch patterns: %w", err)
	}
	c.logWarnings(result.Warnings)

	desired := map[informerKey]watch.Resource{}
	for _, resource := range result.Resources {
		for _, namespace := range c.namespacesFor(resource) {
			desired[informerKey{gvr: resource.GVR, namespace: namespace}] = resource
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for key, entry := range c.informers {
		if _, keep := desired[key]; keep {
			continue
		}
		c.cfg.Logger.Info("stopping watch", "gvr", key.gvr.String(), "namespace", key.namespace)
		entry.cancel()
		delete(c.informers, key)
	}

	for key, resource := range desired {
		if _, running := c.informers[key]; running {
			continue
		}
		c.startInformer(key, resource)
	}

	gvrs := map[schema.GroupVersionResource]bool{}
	for key := range c.informers {
		gvrs[key.gvr] = true
	}
	c.cfg.Metrics.WatchedResources.Set(float64(len(gvrs)))
	c.refreshed = true
	return nil
}

// InformerCount returns the number of running informers.
func (c *Controller) InformerCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.informers)
}

func (c *Controller) namespacesFor(resource watch.Resource) []string {
	if !resource.Namespaced || len(c.cfg.Namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return c.cfg.Namespaces
}

// startInformer must be called with the lock held.
func (c *Controller) startInformer(key informerKey, resource watch.Resource) {
	tweak := func(options *metav1.ListOptions) {
		if c.cfg.LabelSelector != "" {
			options.LabelSelector = c.cfg.LabelSelector
		}
	}
	informer := dynamicinformer.NewFilteredDynamicInformer(
		c.cfg.Client, key.gvr, key.namespace, c.cfg.ResyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, tweak,
	)

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueue(key, obj) },
		UpdateFunc: func(_, obj any) { c.enqueue(key, obj) },
	}
	if _, err := informer.Informer().AddEventHandler(handler); err != nil {
		c.cfg.Logger.Error("cannot register the event handler", "gvr", key.gvr.String(), "error", err)
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	c.informers[key] = &informerEntry{
		gvr:        key.gvr,
		gvk:        resource.GVK,
		namespaced: resource.Namespaced,
		informer:   informer,
		cancel:     cancel,
	}
	c.cfg.Logger.Info("watching", "gvk", resource.GVK.String(), "resource", key.gvr.Resource,
		"namespace", namespaceLabel(key.namespace))
	go informer.Informer().Run(ctx.Done())
}

func (c *Controller) stopAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.informers {
		entry.cancel()
		delete(c.informers, key)
	}
}

// HasSynced reports whether every running informer has completed its initial
// list. It backs the readiness probe.
func (c *Controller) HasSynced() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.refreshed {
		return false
	}
	for _, entry := range c.informers {
		if !entry.informer.Informer().HasSynced() {
			return false
		}
	}
	return true
}

// WaitForCacheSync blocks until every informer has synced or ctx is done.
func (c *Controller) WaitForCacheSync(ctx context.Context) bool {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c.HasSynced() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (c *Controller) enqueue(key informerKey, obj any) {
	accessor, err := meta(obj)
	if err != nil {
		c.cfg.Logger.Warn("cannot read the object metadata", "gvr", key.gvr.String(), "error", err)
		return
	}
	c.queue.Add(queueKey{
		gvr:            key.gvr,
		watchNamespace: key.namespace,
		namespace:      accessor.GetNamespace(),
		name:           accessor.GetName(),
	})
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		c.cfg.Logger.Error("reconcile failed", "gvr", key.gvr.String(),
			"object", key.cacheKey(), "error", err)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

func (c *Controller) entry(key informerKey) *informerEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.informers[key]
}

func namespaceLabel(namespace string) string {
	if namespace == "" {
		return "<all>"
	}
	return namespace
}

// logWarnings logs discovery warnings, but only when they change, so that the
// rediscovery loop does not spam the log every interval.
func (c *Controller) logWarnings(warnings []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := map[string]bool{}
	for _, warning := range warnings {
		current[warning] = true
		if !c.warned[warning] {
			c.cfg.Logger.Warn(warning)
		}
	}
	c.warned = current
}
