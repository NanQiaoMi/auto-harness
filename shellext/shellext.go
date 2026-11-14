// Package shellext implements the "!" shell shortcut: a line that
// starts with "!" runs a shell command directly, without asking the
// model.
//
// "!!" runs the same command but keeps its output out of the model's
// context, the way pi's shortcut does: the user runs the command for
// themselves, and the model never learns what it printed.
//
// The shortcut is the user's own command: it does not go through the
// model's permission gate, because the user typed it. The sandbox
// still applies, since it is about what a command may touch rather
// than about who asked for it.
package shellext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// Command is a parsed shell shortcut.
type Command struct {
	// Text is the command to run.
	Text string
	// Excluded reports that the output must not reach the model.
	Excluded bool
}

// Parse parses a REPL line as a shell shortcut.
//
// A line starting with "!" is a command; a line starting with "!!" is
// a command whose output stays out of the model's context. A line
// whose command is empty is not a shortcut: it falls through to the
// normal input path, as it does in pi.
func Parse(line string) (Command, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "!") {
		return Command{}, false
	}
	excluded := strings.HasPrefix(trimmed, "!!")
	text := trimmed[1:]
	if excluded {
		text = trimmed[2:]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Command{}, false
	}
	return Command{Text: text, Excluded: excluded}, true
}

// Format renders a command's result as the text the model sees.
//
// The format follows pi's: the command, then its output in a fenced
// block. A command that failed says so.
func Format(command string, result tools.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ran `%s`\n", command)
	output := strings.TrimRight(result.Content, "\n")
	if output == "" {
		b.WriteString("(no output)")
	} else {
		b.WriteString("```\n")
		b.WriteString(output)
		b.WriteString("\n```")
	}
	if result.IsError {
		b.WriteString("\n\n(command failed)")
	}
	return b.String()
}

// Runner runs a shell command.
type Runner interface {
	// Run executes the command and returns its result.
	Run(ctx context.Context, command string) (tools.Result, error)
}

// Conversation is what the shortcut needs from the agent: a way to
// put the command's result in front of the model.
type Conversation interface {
	// AppendMessage adds a message to the conversation.
	AppendMessage(msg llm.Message)
}

// Shortcut runs the "!" shortcut.
type Shortcut struct {
	// Runner runs the command. It is required.
	Runner Runner
	// Conversation receives the result, so the model sees it in its
	// next turn. It may be nil, in which case the output only goes
	// to the user.
	Conversation Conversation
	// Bus is where the user_bash event is emitted. It may be nil.
	Bus *ext.Bus
	// Out receives the command's output. It may be nil.
	Out io.Writer
}

// Handle runs a line that is a shell shortcut.
//
// It reports whether it handled the line: a line that is not a
// shortcut is left to the caller. A blocked command is reported as an
// error, and nothing is run.
func (s *Shortcut) Handle(ctx context.Context, line string) (bool, error) {
	cmd, ok := Parse(line)
	if !ok {
		return false, nil
	}
	if s.Runner == nil {
		return true, fmt.Errorf("shellext: no runner")
	}

	// The user_bash event lets an extension veto the command, the way
	// pi's does.
	if s.Bus != nil {
		ev := &ext.Event{
			Type:               ext.EventUserBash,
			BashCommand:        cmd.Text,
			ExcludeFromContext: cmd.Excluded,
		}
		if err := s.Bus.Emit(ev); err != nil {
			return true, err
		}
		if ev.Block {
			reason := ev.BlockReason
			if reason == "" {
				reason = "blocked by an extension"
			}
			return true, fmt.Errorf("shellext: %s", reason)
		}
	}

	result, err := s.Runner.Run(ctx, cmd.Text)
	if err != nil {
		return true, err
	}

	// The user sees the output.
	if s.Out != nil {
		fmt.Fprint(s.Out, result.Content)
		if !strings.HasSuffix(result.Content, "\n") && result.Content != "" {
			fmt.Fprintln(s.Out)
		}
	}

	// The model sees it too, unless the command was excluded.
	if !cmd.Excluded && s.Conversation != nil {
		s.Conversation.AppendMessage(llm.Message{
			Role:    llm.RoleUser,
			Content: Format(cmd.Text, result),
		})
	}
	return true, nil
}

// ToolRunner runs shell commands through a bash tool, so the sandbox
// applies to them exactly as it applies to the model's commands.
type ToolRunner struct {
	// Tool is the bash tool the commands run through.
	Tool *tools.Tool
}

// Run executes the command through the bash tool.
func (r *ToolRunner) Run(ctx context.Context, command string) (tools.Result, error) {
	if r == nil || r.Tool == nil {
		return tools.Result{Content: "no shell tool"}, fmt.Errorf("shellext: no shell tool")
	}
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return tools.Result{}, err
	}
	return r.Tool.Execute(ctx, "shell", args, nil)
}
