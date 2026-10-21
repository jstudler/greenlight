// Package app wires the command line options, the Kubernetes clients, the
// rules and the controller together.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/jstudler/greenlight/internal/logging"
	"github.com/jstudler/greenlight/internal/metrics"
	"github.com/jstudler/greenlight/internal/options"
	"github.com/jstudler/greenlight/internal/patcher"
	"github.com/jstudler/greenlight/internal/rules"
	"github.com/jstudler/greenlight/internal/watch"
)

// Version is overridden at build time with -ldflags.
var Version = "dev"

// Run executes greenlight.
func Run(ctx context.Context, fs *pflag.FlagSet, args []string) error {
	opts := options.New()
	if err := opts.Parse(fs, args); err != nil {
		return err
	}
	if opts.ShowVersion {
		fmt.Println(Version)
		return nil
	}
	if err := opts.Validate(); err != nil {
		return err
	}

	logger, err := logging.New(opts.LogLevel, opts.LogFormat)
	if err != nil {
		return err
	}
	logger = logger.With("version", Version)

	ruleSet, err := loadRules(opts)
	if err != nil {
		return err
	}

	patterns, err := watchPatterns(opts, ruleSet)
	if err != nil {
		return err
	}

	restConfig, err := newRESTConfig(opts, logger)
	if err != nil {
		if opts.ValidateOnly {
			// Compiling the rules is still worth something without a cluster,
			// for example in CI.
			logger.Warn("no cluster is reachable, validating the rules only", "error", err)
			return validate(ruleSet, nil, patterns, os.Stdout)
		}
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building the dynamic client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building the discovery client: %w", err)
	}
	cachedDiscovery := memory.NewMemCacheClient(discoveryClient)
	resolver := watch.NewResolver(cachedDiscovery)

	if opts.ValidateOnly {
		return validate(ruleSet, resolver, patterns, os.Stdout)
	}

	registry := prometheus.NewRegistry()
	collectors := metrics.New(registry)

	controller := patcher.New(patcher.Config{
		Client:              dynamicClient,
		Resolver:            resolver,
		Patterns:            patterns,
		Rules:               ruleSet,
		Namespaces:          opts.Namespaces,
		LabelSelector:       opts.LabelSelector,
		FieldManager:        opts.FieldManager,
		RemoveFinalizers:    opts.RemoveFinalizers,
		Delay:               opts.Delay,
		ResyncPeriod:        opts.ResyncPeriod,
		RediscoveryInterval: opts.RediscoveryInterval,
		Workers:             opts.Workers,
		DryRun:              opts.DryRun,
		Logger:              logger,
		Metrics:             collectors,
		InvalidateDiscovery: cachedDiscovery.Invalidate,
	})

	servers := newServers(opts, logger, registry, controller.HasSynced)
	servers.start(ctx)
	defer servers.shutdown()

	logger.Info("starting greenlight",
		"watch", patternStrings(patterns),
		"rules", len(ruleSet.Rules()),
		"removeFinalizers", opts.RemoveFinalizers,
		"dryRun", opts.DryRun,
		"fieldManager", opts.FieldManager,
	)

	run := func(ctx context.Context) error { return controller.Run(ctx) }
	if opts.LeaderElect {
		return runWithLeaderElection(ctx, opts, restConfig, logger, run)
	}
	return run(ctx)
}

func loadRules(opts *options.Options) (*rules.Set, error) {
	ruleSet, err := rules.Load(opts.Rules, rules.Options{
		ConditionType: opts.ConditionType,
		Reason:        opts.Reason,
		FalseReason:   opts.FalseReason,
		Message:       opts.Message,
	})
	if err != nil {
		return nil, err
	}
	for _, param := range opts.Params {
		key, raw, found := strings.Cut(param, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("--param %q: expected key=value", param)
		}
		value, err := rules.ParseParamValue(raw)
		if err != nil {
			return nil, fmt.Errorf("--param %q: %w", param, err)
		}
		ruleSet.SetParam(key, value)
	}
	return ruleSet, nil
}

func watchPatterns(opts *options.Options, ruleSet *rules.Set) ([]watch.Pattern, error) {
	raw := opts.Watch
	if len(raw) == 0 {
		raw = ruleSet.WatchPatterns()
	}
	if len(raw) == 0 {
		return nil, errors.New("nothing to watch: pass --watch, or use rules with a match block")
	}
	return watch.ParsePatterns(raw)
}

func newRESTConfig(opts *options.Options, logger *slog.Logger) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		loadingRules.ExplicitPath = opts.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building the Kubernetes client configuration: %w", err)
	}
	config.QPS = opts.QPS
	config.Burst = opts.Burst
	config.UserAgent = "greenlight/" + Version
	// The API server reports pruned unknown status fields as warnings. They
	// must never fail a patch, so they are only logged.
	config.WarningHandlerWithContext = &warningLogger{logger: logger}
	return config, nil
}

type warningLogger struct {
	logger *slog.Logger
}

func (w *warningLogger) HandleWarningHeaderWithContext(_ context.Context, code int, agent, text string) {
	if code != 299 || text == "" {
		return
	}
	w.logger.Warn("the API server returned a warning", "agent", agent, "warning", text)
}

func patternStrings(patterns []watch.Pattern) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, p.Raw)
	}
	return out
}
