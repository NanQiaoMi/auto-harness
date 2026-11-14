package shellext

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// fakeRunner runs a command and returns a fixed result.
type fakeRunner struct {
	result tools.Result
	err    error
	ran    string
}

func (f *fakeRunner) Run(ctx context.Context, command string) (tools.Result, error) {
	f.ran = command
	return f.result, f.err
}

// fakeConversation records the messages the shortcut adds.
type fakeConversation struct {
	messages []llm.Message
}

func (f *fakeConversation) AppendMessage(msg llm.Message) {
	f.messages = append(f.messages, msg)
}

func TestParseRecognizesTheShortcut(t *testing.T) {
	cmd, ok := Parse("!ls -la")
	if !ok {
		t.Fatal("!ls -la should be a shortcut")
	}
	if cmd.Text != "ls -la" {
		t.Errorf("text = %q, want %q", cmd.Text, "ls -la")
	}
	if cmd.Excluded {
		t.Error("a single ! keeps the output in the model's context")
	}
}

func TestParseRecognizesTheExcludedShortcut(t *testing.T) {
	cmd, ok := Parse("!!git status")
	if !ok {
		t.Fatal("!!git status should be a shortcut")
	}
	if cmd.Text != "git status" {
		t.Errorf("text = %q, want %q", cmd.Text, "git status")
	}
	if !cmd.Excluded {
		t.Error("!! keeps the output out of the model's context")
	}
}

func TestParseIgnoresOrdinaryLines(t *testing.T) {
	for _, line := range []string{"hello", "!", "!!", "  ", "not!a command"} {
		if _, ok := Parse(line); ok {
			t.Errorf("%q should not be a shortcut", line)
		}
	}
}

func TestHandleRunsTheCommand(t *testing.T) {
	runner := &fakeRunner{result: tools.Result{Content: "file.txt\n"}}
	conv := &fakeConversation{}
	var out bytes.Buffer

	s := &Shortcut{Runner: runner, Conversation: conv, Out: &out}
	handled, err := s.Handle(context.Background(), "!ls")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !handled {
		t.Fatal("the line should have been handled")
	}
	if runner.ran != "ls" {
		t.Errorf("ran = %q, want %q", runner.ran, "ls")
	}
	if !strings.Contains(out.String(), "file.txt") {
		t.Errorf("output = %q, want the command's output", out.String())
	}
	// The model sees the result too.
	if len(conv.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(conv.messages))
	}
	if !strings.Contains(conv.messages[0].Content, "file.txt") {
		t.Errorf("message = %q, want the output", conv.messages[0].Content)
	}
	if !strings.Contains(conv.messages[0].Content, "Ran `ls`") {
		t.Errorf("message = %q, want the command", conv.messages[0].Content)
	}
}

func TestHandleKeepsTheExcludedCommandOutOfTheContext(t *testing.T) {
	runner := &fakeRunner{result: tools.Result{Content: "secret\n"}}
	conv := &fakeConversation{}
	var out bytes.Buffer

	s := &Shortcut{Runner: runner, Conversation: conv, Out: &out}
	handled, err := s.Handle(context.Background(), "!!cat secret")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !handled {
		t.Fatal("the line should have been handled")
	}
	if len(conv.messages) != 0 {
		t.Errorf("messages = %v, want none for an excluded command", conv.messages)
	}
	// The user still sees the output.
	if !strings.Contains(out.String(), "secret") {
		t.Errorf("output = %q, want the command's output", out.String())
	}
}

func TestHandleLeavesOrdinaryLinesAlone(t *testing.T) {
	runner := &fakeRunner{}
	s := &Shortcut{Runner: runner}
	handled, err := s.Handle(context.Background(), "hello")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if handled {
		t.Error("an ordinary line should not be handled")
	}
	if runner.ran != "" {
		t.Errorf("ran = %q, want nothing", runner.ran)
	}
}

func TestHandleEmitsTheUserBashEvent(t *testing.T) {
	bus := ext.NewBus()
	var gotCommand string
	var gotExcluded bool
	bus.On(ext.EventUserBash, func(e *ext.Event) error {
		gotCommand = e.BashCommand
		gotExcluded = e.ExcludeFromContext
		return nil
	})

	runner := &fakeRunner{result: tools.Result{Content: "ok"}}
	s := &Shortcut{Runner: runner, Bus: bus}
	if _, err := s.Handle(context.Background(), "!!pwd"); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if gotCommand != "pwd" {
		t.Errorf("command = %q, want %q", gotCommand, "pwd")
	}
	if !gotExcluded {
		t.Error("the event should carry the excluded flag")
	}
}

func TestHandleRespectsABlock(t *testing.T) {
	bus := ext.NewBus()
	bus.On(ext.EventUserBash, func(e *ext.Event) error {
		e.Block = true
		e.BlockReason = "not allowed"
		return nil
	})

	runner := &fakeRunner{}
	s := &Shortcut{Runner: runner, Bus: bus}
	handled, err := s.Handle(context.Background(), "!rm -rf /")
	if err == nil {
		t.Fatal("expected the block to be reported")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %v, want the reason", err)
	}
	if !handled {
		t.Error("the line should have been handled")
	}
	if runner.ran != "" {
		t.Errorf("ran = %q, want nothing", runner.ran)
	}
}

func TestToolRunnerRunsThroughTheBashTool(t *testing.T) {
	tool := tools.NewBashTool()
	runner := &ToolRunner{Tool: tool}
	result, err := runner.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.IsError {
		t.Errorf("result = %v, want a successful run", result)
	}
	if !strings.Contains(result.Content, "hello") {
		t.Errorf("content = %q, want the command's output", result.Content)
	}
}

func TestFormatReportsAFailure(t *testing.T) {
	text := Format("false", tools.Result{Content: "", IsError: true})
	if !strings.Contains(text, "(command failed)") {
		t.Errorf("text = %q, want the failure marker", text)
	}
	if !strings.Contains(text, "(no output)") {
		t.Errorf("text = %q, want the no-output marker", text)
	}
}

func TestFormatQuotesTheOutput(t *testing.T) {
	text := Format("ls", tools.Result{Content: "a\nb\n"})
	if !strings.Contains(text, "```\na\nb\n```") {
		t.Errorf("text = %q, want the fenced output", text)
	}
}

// The shortcut must not need the model: a command that runs before
// the agent exists still works.
func TestHandleWorksWithoutAConversation(t *testing.T) {
	runner := &fakeRunner{result: tools.Result{Content: "ok"}}
	s := &Shortcut{Runner: runner}
	handled, err := s.Handle(context.Background(), "!true")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !handled {
		t.Fatal("the line should have been handled")
	}
}

// The event carries the command the user typed, so an extension can
// see it even when the shortcut has no runner.
func TestUserBashEventCarriesTheCommand(t *testing.T) {
	bus := ext.NewBus()
	var seen *ext.Event
	bus.On(ext.EventUserBash, func(e *ext.Event) error {
		seen = e
		return nil
	})
	if err := bus.Emit(&ext.Event{Type: ext.EventUserBash, BashCommand: "ls"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if seen == nil || seen.BashCommand != "ls" {
		t.Errorf("event = %v, want the command", seen)
	}
}

// The command is passed to the bash tool as its raw JSON argument.
func TestToolRunnerPassesTheCommandAsJSON(t *testing.T) {
	var got json.RawMessage
	tool := &tools.Tool{
		Name: "bash",
		Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
			got = args
			return tools.Result{Content: "ok"}, nil
		},
	}
	runner := &ToolRunner{Tool: tool}
	if _, err := runner.Run(context.Background(), "echo hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	parsed := tools.DecodeArgs(got)
	if parsed["command"] != "echo hi" {
		t.Errorf("command = %v, want %q", parsed["command"], "echo hi")
	}
}
