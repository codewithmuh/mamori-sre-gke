// Command gameday runs one fault from the mamori fault library (chaos/faults) against the lab
// cluster: it applies the fault's objects in namespace boutique, holds them for a bounded time,
// removes them, and writes a timeline to gameday-runs/<run-id>.json.
//
// Usage: gameday list | start <id> [--duration 10m] [--manual] | stop <id> | stop --all | status
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Ctrl-C, SIGTERM and SIGHUP (the terminal was closed) end a running fault early; the
	// cleanup still runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := newApp(os.Stdout, os.Stderr, os.Stdin).run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
