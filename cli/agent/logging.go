package agent

import (
	"context"
	"fmt"
	"io"
	"os"
)

// kronkLoggerTo returns a kronk-style logger (matching both
// kronkprov.Logger and applog.Logger, which share the
// func(ctx, msg, args...) shape) that writes to w. kronk's bundled
// FmtLogger writes to stdout; in --json mode we redirect that chatter to
// stderr so stdout carries only the JSON result.
func kronkLoggerTo(w io.Writer) func(ctx context.Context, msg string, args ...any) {
	return func(_ context.Context, msg string, args ...any) {
		fmt.Fprintf(w, "%s:", msg)
		for i := 0; i+1 < len(args); i += 2 {
			fmt.Fprintf(w, " %v[%v]", args[i], args[i+1])
		}
		fmt.Fprintln(w)
	}
}

// stderrKronkLogger logs kronk progress to stderr.
var stderrKronkLogger = kronkLoggerTo(os.Stderr)
