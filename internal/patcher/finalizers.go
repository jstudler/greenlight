package patcher

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/jstudler/greenlight/internal/metrics"
)

// removeFinalizers strips every finalizer from the object.
//
// Downstream operators are not running in a greenlight environment, so their
// finalizers would never be removed and the objects would hang in Terminating
// forever. The patch is optimistically locked on the resourceVersion, so a
// concurrent change simply causes a requeue instead of clobbering someone
// else's update.
//
// Caveat: if another controller keeps adding finalizers to a watched kind, the
// two controllers will keep undoing each other. Use --remove-finalizers=false
// for such scenarios.
func (c *Controller) removeFinalizers(
	ctx context.Context,
	key queueKey,
	entry *informerEntry,
	obj *unstructured.Unstructured,
) (bool, error) {
	finalizers := obj.GetFinalizers()
	if len(finalizers) == 0 {
		return false, nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"finalizers":      nil,
			"resourceVersion": obj.GetResourceVersion(),
		},
	})
	if err != nil {
		return false, err
	}

	options := metav1.PatchOptions{}
	if c.cfg.DryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}

	_, err = c.cfg.Client.Resource(entry.gvr).
		Namespace(obj.GetNamespace()).
		Patch(ctx, obj.GetName(), types.MergePatchType, patch, options)
	switch {
	case apierrors.IsNotFound(err):
		// The object was already gone, which is the outcome we wanted.
		return true, nil
	case err != nil:
		return false, fmt.Errorf("removing the finalizers of %s: %w", key.cacheKey(), err)
	}

	group, kind := metrics.Labels(entry.gvk)
	c.cfg.Metrics.FinalizersRemoved.WithLabelValues(group, kind).Inc()
	c.cfg.Logger.Info("finalizers removed",
		"gvk", entry.gvk.String(), "object", key.cacheKey(),
		"finalizers", finalizers, "dryRun", c.cfg.DryRun)
	return true, nil
}
