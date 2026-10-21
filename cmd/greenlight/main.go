// Command greenlight fakes downstream controllers: it watches a configurable
// set of Kubernetes kinds and writes their status subresource, so that
// operators can be tested without their real backends.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/jstudler/greenlight/internal/app"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fs := pflag.NewFlagSet("greenlight", pflag.ContinueOnError)
	err := app.Run(ctx, fs, os.Args[1:])
	switch {
	case err == nil, errors.Is(err, pflag.ErrHelp):
		return 0
	default:
		fmt.Fprintf(os.Stderr, "greenlight: %v\n", err)
		return 1
	}
}
