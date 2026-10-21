// Package metrics holds the Prometheus instrumentation of greenlight.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Label names shared by most collectors.
const (
	labelGroup = "group"
	labelKind  = "kind"
)

// Result classifies the outcome of one reconcile.
const (
	ResultApplied   = "applied"
	ResultUnchanged = "unchanged"
	ResultError     = "error"
	ResultDryRun    = "dryrun"
)

// Metrics bundles every collector greenlight exposes.
type Metrics struct {
	Patches           *prometheus.CounterVec
	ReadyStatus       *prometheus.CounterVec
	EvaluationErrors  *prometheus.CounterVec
	FinalizersRemoved *prometheus.CounterVec
	WatchedResources  prometheus.Gauge
	ReconcileDuration *prometheus.HistogramVec
}

// New creates the collectors and registers them with the given registerer.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Patches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "greenlight_patches_total",
			Help: "Number of status patches by outcome.",
		}, []string{labelGroup, labelKind, "result"}),
		ReadyStatus: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "greenlight_ready_status_total",
			Help: "Number of applied ready conditions by status.",
		}, []string{labelGroup, labelKind, "status"}),
		EvaluationErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "greenlight_evaluation_errors_total",
			Help: "Number of CEL evaluation errors by rule and expression path.",
		}, []string{"rule", "path"}),
		FinalizersRemoved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "greenlight_finalizers_removed_total",
			Help: "Number of objects whose finalizers were removed.",
		}, []string{labelGroup, labelKind}),
		WatchedResources: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "greenlight_watched_resources",
			Help: "Number of API resources currently watched.",
		}),
		ReconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "greenlight_reconcile_duration_seconds",
			Help:    "Duration of a single reconcile.",
			Buckets: prometheus.DefBuckets,
		}, []string{labelGroup, labelKind}),
	}
	if reg != nil {
		reg.MustRegister(
			m.Patches,
			m.ReadyStatus,
			m.EvaluationErrors,
			m.FinalizersRemoved,
			m.WatchedResources,
			m.ReconcileDuration,
		)
	}
	return m
}

// Labels returns the group and kind label values for a GVK.
func Labels(gvk schema.GroupVersionKind) (group, kind string) {
	group = gvk.Group
	if group == "" {
		group = "core"
	}
	return group, gvk.Kind
}
