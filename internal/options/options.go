// Package options defines the command line interface of greenlight.
package options

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// EnvPrefix is prepended to the upper-snake-case flag name to build the
// environment variable that provides a default for that flag.
const EnvPrefix = "GREENLIGHT_"

// Options holds every command line flag.
type Options struct {
	Kubeconfig string
	Context    string

	Watch      []string
	Rules      []string
	Params     []string
	Namespaces []string

	LabelSelector string

	ConditionType string
	Reason        string
	FalseReason   string
	Message       string

	RemoveFinalizers bool
	FieldManager     string
	Workers          int
	Delay            time.Duration

	ResyncPeriod        time.Duration
	RediscoveryInterval time.Duration

	DryRun       bool
	ValidateOnly bool

	QPS   float32
	Burst int

	MetricsBindAddress     string
	HealthProbeBindAddress string

	LeaderElect             bool
	LeaderElectionNamespace string
	LeaderElectionID        string

	LogLevel  string
	LogFormat string

	ShowVersion bool
}

// New returns the options with their default values.
func New() *Options {
	return &Options{
		Kubeconfig:             os.Getenv("KUBECONFIG"),
		ConditionType:          "Ready",
		Reason:                 "Greenlit",
		FalseReason:            "NotGreenlit",
		Message:                "Status set by greenlight",
		RemoveFinalizers:       true,
		FieldManager:           "greenlight",
		Workers:                4,
		ResyncPeriod:           10 * time.Minute,
		RediscoveryInterval:    30 * time.Second,
		QPS:                    50,
		Burst:                  100,
		MetricsBindAddress:     ":8080",
		HealthProbeBindAddress: ":8081",
		LeaderElectionID:       "greenlight",
		LogLevel:               "info",
		LogFormat:              "text",
	}
}

// AddFlags registers every flag on the given flag set.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig,
		"Path to a kubeconfig file. Defaults to $KUBECONFIG, then to the in-cluster configuration.")
	fs.StringVar(&o.Context, "context", o.Context,
		"Name of the kubeconfig context to use. Defaults to the current context.")

	fs.StringSliceVar(&o.Watch, "watch", o.Watch,
		"API groups and kinds to watch. Repeatable and comma-separated. "+
			"Patterns: group/Kind, group/*, group/version/Kind. Use core/Kind for the core group. "+
			"If omitted, the watch set is derived from the match blocks of the rules.")
	fs.StringSliceVar(&o.Rules, "rules", o.Rules,
		"Path to a rules file. Repeatable; the rules are concatenated in order.")
	fs.StringArrayVar(&o.Params, "param", o.Params,
		"Additional key=value parameter exposed to CEL as params.key. Repeatable. "+
			"The value is parsed as a YAML scalar and overrides the rules file.")
	fs.StringSliceVar(&o.Namespaces, "namespace", o.Namespaces,
		"Restrict namespaced kinds to these namespaces. Repeatable. Defaults to all namespaces.")

	fs.StringVar(&o.LabelSelector, "label-selector", o.LabelSelector,
		"Label selector applied to every informer.")

	fs.StringVar(&o.ConditionType, "condition-type", o.ConditionType,
		"Type of the managed ready condition.")
	fs.StringVar(&o.Reason, "reason", o.Reason,
		"Default condition reason when the ready condition is True.")
	fs.StringVar(&o.FalseReason, "false-reason", o.FalseReason,
		"Default condition reason when a rule sets the ready condition to False.")
	fs.StringVar(&o.Message, "message", o.Message,
		"Default ready condition message.")

	fs.BoolVar(&o.RemoveFinalizers, "remove-finalizers", o.RemoveFinalizers,
		"Strip all metadata.finalizers from every watched object.")
	fs.StringVar(&o.FieldManager, "field-manager", o.FieldManager,
		"Field manager used for server-side apply.")
	fs.IntVar(&o.Workers, "workers", o.Workers,
		"Number of workqueue workers.")
	fs.DurationVar(&o.Delay, "delay", o.Delay,
		"Artificial delay before patching, to simulate a slow backend.")

	fs.DurationVar(&o.ResyncPeriod, "resync-period", o.ResyncPeriod,
		"Informer resync period.")
	fs.DurationVar(&o.RediscoveryInterval, "rediscovery-interval", o.RediscoveryInterval,
		"How often the watch patterns are re-resolved, so that CRDs installed later get watched. 0 disables it.")

	fs.BoolVar(&o.DryRun, "dry-run", o.DryRun,
		"Evaluate and log the patches without persisting them (server-side dryRun=All).")
	fs.BoolVar(&o.ValidateOnly, "validate", o.ValidateOnly,
		"Load and compile the rules, resolve the watch patterns, print the result and exit.")

	fs.Float32Var(&o.QPS, "qps", o.QPS, "Client-side QPS limit.")
	fs.IntVar(&o.Burst, "burst", o.Burst, "Client-side burst limit.")

	fs.StringVar(&o.MetricsBindAddress, "metrics-bind-address", o.MetricsBindAddress,
		"Address the Prometheus metrics endpoint binds to. Use 0 to disable it.")
	fs.StringVar(&o.HealthProbeBindAddress, "health-probe-bind-address", o.HealthProbeBindAddress,
		"Address the /healthz and /readyz endpoints bind to. Use 0 to disable them.")

	fs.BoolVar(&o.LeaderElect, "leader-elect", o.LeaderElect,
		"Enable leader election, so that only one replica is active.")
	fs.StringVar(&o.LeaderElectionNamespace, "leader-election-namespace", o.LeaderElectionNamespace,
		"Namespace holding the leader election Lease. Defaults to the pod namespace.")
	fs.StringVar(&o.LeaderElectionID, "leader-election-id", o.LeaderElectionID,
		"Name of the leader election Lease.")

	fs.StringVar(&o.LogLevel, "log-level", o.LogLevel, "Log level: debug, info, warn or error.")
	fs.StringVar(&o.LogFormat, "log-format", o.LogFormat, "Log format: text or json.")

	fs.BoolVar(&o.ShowVersion, "version", o.ShowVersion, "Print the version and exit.")
}

// Parse parses the arguments and then fills every flag that was not given
// explicitly from its environment variable.
func (o *Options) Parse(fs *pflag.FlagSet, args []string) error {
	o.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return ApplyEnv(fs, os.LookupEnv)
}

// ApplyEnv sets every flag that was not changed on the command line from
// GREENLIGHT_<FLAG_UPPER_SNAKE>. The command line always wins.
func ApplyEnv(fs *pflag.FlagSet, lookup func(string) (string, bool)) error {
	var err error
	fs.VisitAll(func(flag *pflag.Flag) {
		if err != nil || flag.Changed {
			return
		}
		value, ok := lookup(EnvName(flag.Name))
		if !ok {
			return
		}
		if setErr := fs.Set(flag.Name, value); setErr != nil {
			err = fmt.Errorf("%s: %w", EnvName(flag.Name), setErr)
		}
	})
	return err
}

// EnvName returns the environment variable that backs a flag.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Validate checks the values that the flag package cannot check itself.
func (o *Options) Validate() error {
	if o.Workers <= 0 {
		return fmt.Errorf("--workers must be greater than 0")
	}
	if o.Delay < 0 {
		return fmt.Errorf("--delay must not be negative")
	}
	if o.ResyncPeriod < 0 {
		return fmt.Errorf("--resync-period must not be negative")
	}
	if o.RediscoveryInterval < 0 {
		return fmt.Errorf("--rediscovery-interval must not be negative")
	}
	if o.FieldManager == "" {
		return fmt.Errorf("--field-manager must not be empty")
	}
	if len(o.Watch) == 0 && len(o.Rules) == 0 {
		return fmt.Errorf("nothing to do: pass --watch, --rules, or both")
	}
	for _, namespace := range o.Namespaces {
		if strings.TrimSpace(namespace) == "" {
			return fmt.Errorf("--namespace must not be empty")
		}
	}
	return nil
}

// Disabled reports whether a bind address switches the server off.
func Disabled(address string) bool {
	switch strings.TrimSpace(address) {
	case "", "0", "off":
		return true
	}
	return false
}
