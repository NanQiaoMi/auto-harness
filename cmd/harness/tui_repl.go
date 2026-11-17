package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/config"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/perm"
	"github.com/NanQiaoMi/auto-harness/shellext"
	"github.com/NanQiaoMi/auto-harness/tui"
)

// runTUIREPL runs the interactive loop on the differential-rendering
// screen: model output updates a single pending line, so only the changed
// row is rewritten instead of scrolling the whole screen.
func runTUIREPL(ctx context.Context, ag *agent.Agent, bus *ext.Bus, commands *ext.CommandRegistry, cfg *config.Config, permSetup *permSetup, shellShortcut *shellext.Shortcut, promptSetup *promptSetup, skillSetup *skillSetup, quiet bool) {
	theme := tui.DefaultTheme()

	term := tui.NewProcessTerminal()
	screen := tui.NewScreen(term, theme)

	transcript := tui.NewTranscript()
	editor := tui.NewEditor("> ")
	editor.Style = theme.User

	root := &tui.Container{}
	root.Add(transcript)
	root.Add(editor)
	screen.SetRoot(root)

	// The screen owns the terminal input. The collector reassembles the
	// raw bytes into lines, which the REPL and the permission prompt
	// share.
	lines := tui.NewLineInput()
	screen.SetOnInput(func(data string) { lines.Feed(data) })
	lines.SetOnPartial(func(text string) {
		editor.Value = text
		screen.RequestRender()
	})
	permSetup.setLineReader(lines)

	// The permission prompt asks a question while the screen renders.
	// Writing it to the transcript keeps the two from interleaving.
	promptOut := tui.NewTranscriptWriter(transcript, screen.RequestRender)
	permSetup.setPromptOutput(promptOut)

	transcript.Appendf("auto-harness %s — model %s (%s). /help for commands.", version, cfg.Model, cfg.Provider)
	transcript.Append("")

	// The "!" shortcut writes into the transcript: the screen owns the
	// terminal, so stdout would interleave with the frame updates.
	if shellShortcut != nil {
		shellShortcut.Out = promptOut
	}

	// The prompt templates are slash commands like any other, and they
	// write into the transcript too: the screen owns the terminal, so
	// stdout would interleave with the frame updates.
	if promptSetup != nil {
		promptSetup.out = func(line string) {
			transcript.Append(line)
			screen.RequestRender()
		}
	}

	// The skills write into the transcript for the same reason.
	if skillSetup != nil {
		skillSetup.out = func(line string) {
			transcript.Append(line)
			screen.RequestRender()
		}
	}

	// The model streams into the transcript's pending line; the screen
	// diffs it against the previous frame.
	var streaming bool
	bus.On(ext.EventMessageUpdate, func(e *ext.Event) error {
		if e.Data != nil {
			if t, _ := e.Data["thinking"].(bool); t {
				return nil
			}
		}
		if !streaming {
			streaming = true
			transcript.SetPending("")
		}
		transcript.SetPending(transcript.Pending() + e.Text)
		screen.RequestRender()
		return nil
	})
	// Each model message is committed as its own line, so two turns do
	// not run together.
	bus.On(ext.EventMessageEnd, func(e *ext.Event) error {
		if pending := transcript.Pending(); pending != "" {
			transcript.Append(pending)
			transcript.SetPending("")
		}
		streaming = false
		screen.RequestRender()
		return nil
	})
	// The tool notices go through the renderers, so an extension that
	// registers one takes over the presentation of its tool.
	notice := &toolNotice{registry: ag.Tools, cwd: workdir()}
	notice.install(bus, quiet, func() bool { return false }, func(line string) {
		transcript.Append(line)
		screen.RequestRender()
	})

	if err := screen.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "harness: tui start: %v\n", err)
		return
	}
	defer screen.Stop()

	done := make(chan struct{})
	defer close(done)
	go screen.Run(33*time.Millisecond, done)

	scanner := perm.NewScanner(lines)
	for {
		if !scanner.Scan() {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		editor.Value = ""
		screen.RequestRender()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if err := handleCommand(ctx, commands, line); errors.Is(err, errExitREPL) {
				return
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		// The "!" shortcut runs a shell command without asking the
		// model.
		if shellShortcut != nil {
			if handled, err := shellShortcut.Handle(ctx, line); handled {
				if err != nil {
					transcript.Appendf("error: %v", err)
					screen.RequestRender()
				}
				continue
			}
		}
		transcript.Appendf("> %s", line)
		screen.RequestRender()

		res, err := ag.Run(ctx, line)
		if err != nil {
			transcript.Appendf("error: %v", err)
			screen.RequestRender()
			continue
		}
		// The streamed text is committed when the message ends; the
		// final answer may not have been streamed at all.
		if res.Final != "" {
			history := transcript.Lines()
			last := ""
			if len(history) > 0 {
				last = history[len(history)-1]
			}
			if res.Final != last {
				transcript.Append(res.Final)
			}
		}
		streaming = false
		screen.RequestRender()
	}
}
