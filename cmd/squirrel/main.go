package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// exitCodeError carries a specific process exit status out of a command's
// RunE. `squirrel status` uses it to report its scriptable green/amber/red
// contract (exit 0/1/2), which the generic "any error ⇒ exit 1" path below
// cannot express. errors.As recovers the code here; the command that
// returns it silences cobra's error printing so nothing but the code
// leaves the process.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := newRootCmd().ExecuteContext(ctx)
	var ec exitCodeError
	if errors.As(err, &ec) {
		os.Exit(ec.code)
	}
	if err != nil {
		os.Exit(1)
	}
}
