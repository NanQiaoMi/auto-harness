package main

import (
	"context"
	"fmt"
	"os"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/jsonout"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
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
func runJSONMode(ctx context.Context, ag *agent.Agent, sess *session.Session, bus *ext.Bus, prompt string, images []llm.Image, cwd string) int {
	writer := jsonout.NewWriter(os.Stdout)

	// The header goes first, before the run starts: a consumer that
	// sees an event must already know which session it belongs to.
	if err := writer.WriteHeader(sess, cwd); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		return 1
	}

	unsubscribe := writer.Install(bus)
	defer unsubscribe()

	res, err := ag.RunWithImages(ctx, prompt, images)
	if err != nil {
		// The failure is reported on stderr: the stream is for the
		// run, and a run that died is not a run that settled.
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		return 1
	}

	// The settled record comes from the bus: the agent emits it once the
	// queue and the settle handlers are done. Writing one here as well
	// would report idle twice, and the manual one could not know about
	// the work the agent still had.

	// A run that ended badly is a failure, and the exit code says so.
	if res.Cancelled {
		return 1
	}
	return 0
}
