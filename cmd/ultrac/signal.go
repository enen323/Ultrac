package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// signalContext returns a context cancelled by SIGINT or SIGTERM.
//
// On Windows there is no SIGTERM from a console close, so os.Interrupt is the
// signal that matters; SIGTERM is included because `taskkill` sends it and a
// process that ignores it has to be killed outright, losing any unflushed
// transcript.
//
// Second Ctrl-C is left to the default handler. The usual pattern is "first
// cancels, second force-quits", and swallowing every signal makes a wedged
// process unkillable from the keyboard.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
