package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/jstudler/greenlight/internal/options"
)

const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
)

// runWithLeaderElection acquires a Lease and only then runs the controller, so
// that a Deployment with more than one replica stays safe.
func runWithLeaderElection(
	ctx context.Context,
	opts *options.Options,
	config *rest.Config,
	logger *slog.Logger,
	run func(context.Context) error,
) error {
	namespace := opts.LeaderElectionNamespace
	if namespace == "" {
		namespace = strings.TrimSpace(podNamespace())
	}
	if namespace == "" {
		return fmt.Errorf("--leader-elect needs a namespace: set --leader-election-namespace or POD_NAMESPACE")
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("building the Kubernetes client for leader election: %w", err)
	}

	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = fmt.Sprintf("greenlight-%d", time.Now().UnixNano())
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Namespace: namespace, Name: opts.LeaderElectionID},
		Client:    client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	runErr := make(chan error, 1)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logger.Info("acquired the leader lease", "identity", identity, "lease", opts.LeaderElectionID)
				runErr <- run(ctx)
			},
			OnStoppedLeading: func() {
				logger.Info("lost the leader lease", "identity", identity)
			},
		},
	})
	if err != nil {
		return fmt.Errorf("configuring leader election: %w", err)
	}

	elector.Run(ctx)
	select {
	case err := <-runErr:
		return err
	default:
		return nil
	}
}
