package rpcclient

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/rpc"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

type fakeStream struct {
	events []llm.StreamEvent
	pos    int
}

func (s *fakeStream) Recv() (llm.StreamEvent, error) {
	if s.pos >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *fakeStream) Close() error { return nil }

type fakeProvider struct {
	reply string
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	// Include U+2028 line separator inside content to verify LF-only framing.
	msg := llm.Message{Role: llm.RoleAssistant, Content: p.reply + "\u2028line2"}
	return &fakeStream{events: []llm.StreamEvent{
		{Type: llm.EventTextDelta, Text: msg.Content},
		{Type: llm.EventDone, Message: &msg},
	}}, nil
}

func TestClientRoundTripAndPromptSettle(t *testing.T) {
	bus := ext.NewBus()
	reg := tools.NewRegistry()
	ag := agent.New(agent.Config{
		Provider: &fakeProvider{reply: "hello from sdk"},
		Model:    "claude-sonnet-5-5",
		MaxTurns: 2,
	}, bus, reg)
	sess, err := session.New(t.TempDir(), "claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	srv := rpc.NewServer(ag, sess, ext.NewCommandRegistry(), nil)
	unsub := srv.Install(bus)
	defer unsub()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv = rpc.NewServer(ag, sess, ext.NewCommandRegistry(), outW)
	unsub()
	unsub = srv.Install(bus)
	defer unsub()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srvDone := make(chan error, 1)
	go func() {
		defer outW.Close()
		srvDone <- srv.Serve(ctx, inR)
	}()

	cli := New(inW, outR)
	defer cli.Close()

	st, err := cli.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st.Model != "claude-sonnet-5-5" || st.SchemaVersion != rpc.SchemaVersion {
		t.Errorf("state = %+v", st)
	}

	b64PNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	var seen []string
	res, err := cli.Prompt(ctx, "inspect", []rpc.ImageInput{{Data: b64PNG}}, func(ev rpc.Event) {
		seen = append(seen, ev.Type)
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Disposition != "started" {
		t.Errorf("Disposition = %q", res.Disposition)
	}
	if !strings.Contains(res.Text, "hello from sdk\u2028line2") {
		t.Errorf("Text = %q", res.Text)
	}
	if len(seen) == 0 || seen[len(seen)-1] != rpc.EventAgentSettled {
		t.Errorf("events did not end with agent_settled: %v", seen)
	}

	msgs, err := cli.GetMessages(ctx)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("messages = %d, want 2 (%s)", len(msgs), mustJSON(msgs))
	}

	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-srvDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
