package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
)

// printSignalContext is a context cancelled by SIGINT or SIGTERM, plus the
// identity of the signal that fired. The print mode maps that identity to an
// exit code (pi: print-mode.ts:50-66 — 143 for SIGTERM, 129 for SIGHUP), and
// "which signal" is not recoverable from a bare context cancellation.
//
// On Windows there is no SIGTERM from a console close, so os.Interrupt is the
// signal that matters; SIGTERM is included because `taskkill` sends it and a
// process that ignores it has to be killed outright, losing any unflushed
// transcript. A second Ctrl-C is left to the default handler: the usual
// pattern is "first cancels, second force-quits", and swallowing every signal
// makes a wedged process unkillable from the keyboard.
//
// The watcher goroutine exits on the first signal or when stop is called, so
// a run that finishes without a signal leaves nothing behind.
func printSignalContext() (ctx context.Context, stop func(), fired func() string) {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	// SIGHUP exists on Windows as a constant but is never delivered there;
	// registering it is harmless, but the guard keeps the set honest about
	// what this platform can actually send.
	if runtime.GOOS != "windows" {
		sigs = append(sigs, syscall.SIGHUP)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, sigs...)

	ctx, cancel := context.WithCancel(context.Background())
	var name atomic.Value // string
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			name.Store(sig.String())
			cancel()
		case <-done:
		}
	}()
	stop = func() {
		signal.Stop(sigCh)
		close(done)
	}
	fired = func() string {
		if v, ok := name.Load().(string); ok {
			return v
		}
		return ""
	}
	return ctx, stop, fired
}

// signalExitCode maps a signal name to the conventional exit status: 128 +
// the signal number, using the shell's numbering (SIGINT 2 → 130, SIGTERM 15
// → 143, SIGHUP 1 → 129). pi hardcodes 143 and 129 for the two signals it
// registers; the arithmetic here covers the third for free.
func signalExitCode(name string) int {
	switch name {
	case "interrupt":
		return 130
	case "hangup":
		return 129
	default:
		return 143
	}
}
