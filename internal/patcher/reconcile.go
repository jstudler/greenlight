package patcher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/jstudler/greenlight/internal/metrics"
	"github.com/jstudler/greenlight/internal/rules"
)

func meta(obj any) (metav1.Object, error) {
	if tombstone, ok := obj.(interface{ GetObj() any }); ok {
		obj = tombstone.GetObj()
	}
	return apimeta.Accessor(obj)
}

// reconcile brings a single object to its desired status.
func (c *Controller) reconcile(ctx context.Context, key queueKey) error {
	entry := c.entry(informerKey{gvr: key.gvr, namespace: key.watchNamespace})
	if entry == nil {
		// The informer was stopped in the meantime, for example because the CRD
		// was removed.
		return nil
	}
	group, kind := metrics.Labels(entry.gvk)
	timer := c.cfg.Clock.Now()
	defer func() {
		c.cfg.Metrics.ReconcileDuration.WithLabelValues(group, kind).
			Observe(c.cfg.Clock.Since(timer).Seconds())
	}()

	item, exists, err := entry.informer.Informer().GetIndexer().GetByKey(key.cacheKey())
	if err != nil {
		return fmt.Errorf("reading the cache: %w", err)
	}
	if !exists {
		c.forgetDelay(key)
		return nil
	}
	obj, ok := item.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("the cache holds a %T instead of an unstructured object", item)
	}
	obj = obj.DeepCopy()

	log := c.cfg.Logger.With("gvk", entry.gvk.String(), "object", key.cacheKey())

	if c.cfg.RemoveFinalizers && len(obj.GetFinalizers()) > 0 {
		removed, err := c.removeFinalizers(ctx, key, entry, obj)
		if err != nil {
			return err
		}
		if removed && obj.GetDeletionTimestamp() != nil {
			// The object disappears once the last finalizer is gone.
			c.forgetDelay(key)
			return nil
		}
	}

	if obj.GetDeletionTimestamp() != nil {
		// Never patch the status of a terminating object.
		c.forgetDelay(key)
		return nil
	}

	rule := c.cfg.Rules.Select(entry.gvk)
	matched, evalErrors := rule.MatchConditions(ctx, c.cfg.Rules, obj)
	c.logEvalErrors(log, obj, rule.Name, evalErrors)
	if !matched {
		log.Debug("skipped by matchConditions", "rule", rule.Name)
		c.forgetDelay(key)
		return nil
	}

	evaluation := rule.Evaluate(ctx, c.cfg.Rules, obj)
	c.logEvalErrors(log, obj, rule.Name, evaluation.Errors)

	generation := obj.GetGeneration()
	fields := evaluation.Fields
	for field := range c.knownUnknownFields(key.gvr) {
		if _, present := fields[field]; present {
			delete(fields, field)
			log.Debug("skipping a status field that the CRD schema does not declare",
				"rule", rule.Name, "field", field)
		}
	}
	desired := Desired{Fields: fields}
	for _, cond := range evaluation.Conditions {
		desired.Conditions = append(desired.Conditions, Managed{
			Type:               cond.Type,
			Status:             cond.Status,
			Reason:             cond.Reason,
			Message:            cond.Message,
			ObservedGeneration: generation,
		})
	}

	currentStatus, _, _ := unstructured.NestedMap(obj.Object, "status")
	if currentStatus == nil {
		currentStatus = map[string]any{}
	}
	owned := OwnedStatusFields(obj, c.cfg.FieldManager)

	needed, reason := NeedsPatch(currentStatus, desired, owned)
	if !needed {
		c.cfg.Metrics.Patches.WithLabelValues(group, kind, metrics.ResultUnchanged).Inc()
		log.Debug("status is already up to date", "rule", rule.Name)
		c.forgetDelay(key)
		return nil
	}

	if wait, ok := c.waitForDelay(key, rule); ok {
		log.Debug("delaying the patch", "rule", rule.Name, "delay", wait.String())
		return nil
	}

	if err := c.apply(ctx, entry, obj, desired, currentStatus); err != nil {
		if apierrors.IsNotFound(err) {
			c.forgetDelay(key)
			return nil
		}
		c.cfg.Metrics.Patches.WithLabelValues(group, kind, metrics.ResultError).Inc()
		return fmt.Errorf("applying the status: %w", err)
	}

	result := metrics.ResultApplied
	if c.cfg.DryRun {
		result = metrics.ResultDryRun
	}
	c.cfg.Metrics.Patches.WithLabelValues(group, kind, result).Inc()
	c.cfg.Metrics.ReadyStatus.WithLabelValues(group, kind, string(evaluation.Ready())).Inc()

	log.Info("status applied",
		"rule", rule.Name,
		"ready", string(evaluation.Ready()),
		"fields", evaluation.FieldNames(),
		"reason", reason,
		"dryRun", c.cfg.DryRun,
	)
	return nil
}

// waitForDelay implements the artificial delay. The worker is never blocked:
// the item is simply requeued until the delay has elapsed.
func (c *Controller) waitForDelay(key queueKey, rule *rules.Rule) (time.Duration, bool) {
	delay := c.cfg.Delay
	if rule.Delay() > delay {
		delay = rule.Delay()
	}
	if delay <= 0 {
		return 0, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Clock.Now()
	started, ok := c.delayed[key]
	if !ok {
		c.delayed[key] = now
		c.queue.AddAfter(key, delay)
		return delay, true
	}
	if remaining := delay - now.Sub(started); remaining > 0 {
		c.queue.AddAfter(key, remaining)
		return remaining, true
	}
	delete(c.delayed, key)
	return 0, false
}

func (c *Controller) forgetDelay(key queueKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.delayed, key)
}

func (c *Controller) logEvalErrors(log *slog.Logger, obj *unstructured.Unstructured, rule string, errs []rules.EvalError) {
	for _, err := range errs {
		c.cfg.Metrics.EvaluationErrors.WithLabelValues(rule, err.Path).Inc()
		if c.errorLog.shouldLog(string(obj.GetUID()), err.Path, err.Err.Error()) {
			log.Error("expression evaluation failed",
				"rule", rule, "path", err.Path, "error", err.Err.Error())
		}
	}
}
