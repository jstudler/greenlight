package options

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func parse(t *testing.T, env map[string]string, args ...string) *Options {
	t.Helper()
	opts := New()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	lookup := func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
	if err := ApplyEnv(fs, lookup); err != nil {
		t.Fatalf("applying the environment: %v", err)
	}
	return opts
}

func TestDefaults(t *testing.T) {
	opts := parse(t, nil)
	if opts.ConditionType != "Ready" || opts.Reason != "Greenlit" || opts.FalseReason != "NotGreenlit" {
		t.Fatalf("unexpected condition defaults: %+v", opts)
	}
	if !opts.RemoveFinalizers {
		t.Fatal("--remove-finalizers must default to true")
	}
	if opts.Workers != 4 || opts.ResyncPeriod != 10*time.Minute || opts.RediscoveryInterval != 30*time.Second {
		t.Fatalf("unexpected runtime defaults: %+v", opts)
	}
	if opts.MetricsBindAddress != ":8080" || opts.HealthProbeBindAddress != ":8081" {
		t.Fatalf("unexpected bind addresses: %+v", opts)
	}
}

func TestRepeatableAndCommaSeparatedFlags(t *testing.T) {
	opts := parse(t, nil,
		"--watch", "netbox.dev/PrefixClaim,netbox.dev/IpAddressClaim",
		"--watch", "example.com/*",
		"--rules", "a.yaml",
		"--rules", "b.yaml",
		"--param", "pool=10.0.0.0/8",
		"--param", "salt=x",
		"--namespace", "one,two",
	)

	if len(opts.Watch) != 3 {
		t.Fatalf("--watch = %v, want 3 entries", opts.Watch)
	}
	if len(opts.Rules) != 2 || opts.Rules[1] != "b.yaml" {
		t.Fatalf("--rules = %v", opts.Rules)
	}
	if len(opts.Params) != 2 || opts.Params[0] != "pool=10.0.0.0/8" {
		t.Fatalf("--param = %v", opts.Params)
	}
	if len(opts.Namespaces) != 2 {
		t.Fatalf("--namespace = %v", opts.Namespaces)
	}
}

func TestEnvironmentFallback(t *testing.T) {
	env := map[string]string{
		"GREENLIGHT_REMOVE_FINALIZERS": "false",
		"GREENLIGHT_WATCH":             "example.com/*",
		"GREENLIGHT_LOG_LEVEL":         "debug",
		"GREENLIGHT_WORKERS":           "8",
	}
	opts := parse(t, env)
	if opts.RemoveFinalizers {
		t.Fatal("GREENLIGHT_REMOVE_FINALIZERS was ignored")
	}
	if len(opts.Watch) != 1 || opts.Watch[0] != "example.com/*" {
		t.Fatalf("GREENLIGHT_WATCH was ignored: %v", opts.Watch)
	}
	if opts.LogLevel != "debug" || opts.Workers != 8 {
		t.Fatalf("the environment was not applied: %+v", opts)
	}
}

func TestTheFlagWinsOverTheEnvironment(t *testing.T) {
	env := map[string]string{"GREENLIGHT_LOG_LEVEL": "debug", "GREENLIGHT_WORKERS": "8"}
	opts := parse(t, env, "--log-level", "warn")
	if opts.LogLevel != "warn" {
		t.Fatalf("the command line must win: %q", opts.LogLevel)
	}
	if opts.Workers != 8 {
		t.Fatalf("flags that were not given must still come from the environment: %d", opts.Workers)
	}
}

func TestEnvName(t *testing.T) {
	tests := map[string]string{
		"watch":                     "GREENLIGHT_WATCH",
		"remove-finalizers":         "GREENLIGHT_REMOVE_FINALIZERS",
		"health-probe-bind-address": "GREENLIGHT_HEALTH_PROBE_BIND_ADDRESS",
	}
	for flag, want := range tests {
		if got := EnvName(flag); got != want {
			t.Fatalf("EnvName(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{name: "valid", mutate: func(o *Options) { o.Watch = []string{"example.com/*"} }},
		{name: "rules only", mutate: func(o *Options) { o.Rules = []string{"rules.yaml"} }},
		{name: "nothing to do", mutate: func(o *Options) {}, wantErr: "nothing to do"},
		{
			name:    "no workers",
			mutate:  func(o *Options) { o.Watch = []string{"a/B"}; o.Workers = 0 },
			wantErr: "--workers",
		},
		{
			name:    "negative delay",
			mutate:  func(o *Options) { o.Watch = []string{"a/B"}; o.Delay = -time.Second },
			wantErr: "--delay",
		},
		{
			name:    "empty field manager",
			mutate:  func(o *Options) { o.Watch = []string{"a/B"}; o.FieldManager = "" },
			wantErr: "--field-manager",
		},
		{
			name:    "empty namespace",
			mutate:  func(o *Options) { o.Watch = []string{"a/B"}; o.Namespaces = []string{" "} },
			wantErr: "--namespace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := New()
			tc.mutate(opts)
			err := opts.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDisabled(t *testing.T) {
	for _, address := range []string{"", "0", "off", " 0 "} {
		if !Disabled(address) {
			t.Fatalf("Disabled(%q) = false, want true", address)
		}
	}
	for _, address := range []string{":8080", "127.0.0.1:9090"} {
		if Disabled(address) {
			t.Fatalf("Disabled(%q) = true, want false", address)
		}
	}
}
