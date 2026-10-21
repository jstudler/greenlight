package celfuncs_test

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"

	"github.com/jstudler/greenlight/internal/celfuncs"
)

func TestLibraryIsUsableFromCEL(t *testing.T) {
	env, err := cel.NewEnv(celfuncs.Library())
	if err != nil {
		t.Fatalf("building the environment: %v", err)
	}

	tests := []struct {
		expression string
		want       any
		wantErr    string
	}{
		{expression: `cidrSubnet('10.0.0.0/8', 25, 1)`, want: "10.0.0.128/25"},
		{expression: `cidrHost('10.0.0.0/24', 7)`, want: "10.0.0.7"},
		{expression: `hostCIDR('10.0.0.7')`, want: "10.0.0.7/32"},
		{expression: `pickSubnet('10.0.0.0/8', 25, 'ns/name') == pickSubnet('10.0.0.0/8', 25, 'ns/name')`, want: true},
		{expression: `pickHost('10.0.0.0/24', 'ns/name').startsWith('10.0.0.')`, want: true},
		{expression: `hash('ns/name') >= 0`, want: true},
		{expression: `cidrSubnet('bogus', 25, 1)`, wantErr: "not a valid CIDR prefix"},
	}

	for _, tc := range tests {
		t.Run(tc.expression, func(t *testing.T) {
			ast, issues := env.Compile(tc.expression)
			if issues != nil && issues.Err() != nil {
				t.Fatalf("compiling: %v", issues.Err())
			}
			program, err := env.Program(ast)
			if err != nil {
				t.Fatalf("building the program: %v", err)
			}
			out, _, err := program.Eval(cel.NoVars())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error containing %q, got %v (%v)", tc.wantErr, out, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluating: %v", err)
			}
			if out.Value() != tc.want {
				t.Fatalf("got %v, want %v", out.Value(), tc.want)
			}
		})
	}
}
