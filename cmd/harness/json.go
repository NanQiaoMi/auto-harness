package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mimi/auto-harness/agent"
	"github.com/mimi/auto-harness/ext"
	"github.com/mimi/auto-harness/jsonout"
	"github.com/mimi/auto-harness/session"
)

// runJSONMode runs the harness one-shot and writes the run as a JSON
// event stream on stdout.
//
// The stream is the whole story of the run: the session header first,
// then every event as it happens, then the record that says the run
// settled. A consumer that wants the answer alone reads the last
// assistant message; a consumer that wants the whole thing reads
// everything.
//
// The exit code reports whether the run succeeded, so a script can
// check it without parsing the stream.
func runJSONMode(ctx context.Context, ag *agent.Agent, sess *session.Session, bus *ext.Bus, prompt, cwd string) {
	writer := jsonout.NewWriter(os.Stdout)

	// The header goes first, before the run starts: a consumer that
	// sees an event must already know which session it belongs to.
	if err := writer.WriteHeader(sess, cwd); err != nil {
		fatal(err)
	}

	unsubscribe := writer.Install(bus)
	defer unsubscribe()

	res, err := ag.Run(ctx, prompt)
	if err != nil {
		// The failure is reported on stderr: the stream is for the
		// run, and a run that died is not a run that settled.
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}

	// The settled record closes the stream. A consumer that reads
	// until it knows the run is over stops here.
	aborted := ctx.Err() != nil
	if err := writer.WriteSettled(aborted); err != nil {
		fatal(err)
	}

	// A run that ended badly is a failure, and the exit code says so.
	if res.Cancelled {
		os.Exit(1)
	}
}
