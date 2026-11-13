// Command build-snapshot builds the static catalog snapshot from the public
// API. See docs/API_POLICY.md and docs/DEPLOYMENT.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/Cawlumm/open-exercise-db/internal/snapshot"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build-snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	api := fs.String("api", snapshot.DefaultAPI, "base URL of the public API")
	out := fs.String("out", "", "directory to write into (the checked-out catalog branch root)")
	confirm := fs.Bool("confirm-removals", false, "publish even if en shrank to under half the published count or a published locale is gone")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(stderr, "build-snapshot: -out is required")
		return 2
	}

	cfg := &snapshot.Config{API: *api, Out: *out, ConfirmRemovals: *confirm, Log: stderr}
	res, err := snapshot.Build(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "build-snapshot: refusing to publish:", err)
		return 1
	}
	for _, l := range res.Locales {
		fmt.Fprintf(stderr, "  %s: %d exercises, %d bytes, %s\n", l.Code, l.Total, res.Bytes[l.File], l.File)
	}
	fmt.Fprintf(stderr, "%d requests\n", cfg.Requests)
	if res.Changed {
		fmt.Fprintln(stdout, "changed", res.GeneratedAt)
	} else {
		fmt.Fprintln(stdout, "unchanged")
	}
	return 0
}
