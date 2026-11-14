package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/NanQiaoMi/auto-harness/agent"
	"github.com/NanQiaoMi/auto-harness/ext"
	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/session"
	"github.com/NanQiaoMi/auto-harness/tools"
)

// fakeStream replays a scripted message.
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

// fakeProvider returns scripted responses and records the requests.
type fakeProvider struct {
	mu        sync.Mutex
	responses []llm.Message
	requests  []llm.ChatRequest
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return nil, errors.New("fake: no scripted responses left")
	}
	msg := p.responses[0]
	p.responses = p.responses[1:]
	return &fakeStream{events: []llm.StreamEvent{
		{Type: llm.EventTextDelta, Text: msg.Content},
		{Type: llm.EventDone, Message: &msg},
	}}, nil
}

// newTestServer builds a server over a fake provider.
func newTestServer(t *testing.T, provider llm.Provider) (*Server, *ext.Bus, *session.Session) {
	t.Helper()

	bus := ext.NewBus()
	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewReadTool()); err != nil {
		t.Fatal(err)
	}
	ag := agent.New(agent.Config{
		Provider: provider,
		Model:    "test-model",
		MaxTurns: 3,
	}, bus, reg)

	sess, err := session.New(t.TempDir(), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	commands := ext.NewCommandRegistry()
	commands.Register("hello", &ext.Command{Description: "say hello"})

	var out bytes.Buffer
	server := NewServer(ag, sess, commands, &out)
	server.Install(bus)
	return server, bus, sess
}

// records reads the records the server wrote.
func records(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var parsed []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(out.String()))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the record %q is not JSON: %v", line, err)
		}
		parsed = append(parsed, record)
	}
	return parsed
}

// responseFor returns the response to a command.
//
// The protocol says clients correlate by ID rather than by response
// order: a run's events and a command's response share the stream, so
// their order is not fixed.
func responseFor(t *testing.T, parsed []map[string]any, command string) map[string]any {
	t.Helper()
	for _, record := range parsed {
		if record["type"] == "response" && record["command"] == command {
			return record
		}
	}
	t.Fatalf("no response to %q in %v", command, parsed)
	return nil
}

// TestReaderSplitsOnLFOnly checks the framing.
//
// A record's strings may hold U+2028 and U+2029, which are ordinary
// characters in JSON. A reader that split on them would cut a record
// in half.
func TestReaderSplitsOnLFOnly(t *testing.T) {
	input := "{\"a\":\"one two three\"}\n{\"b\":2}\n"

	reader := NewReader(strings.NewReader(input))
	first, err := reader.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(first), "one two three") {
		t.Errorf("the record is %q", first)
	}
	second, err := reader.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(second) != `{"b":2}` {
		t.Errorf("the record is %q", second)
	}
	if _, err := reader.Read(); err != io.EOF {
		t.Errorf("the error is %v, want EOF", err)
	}
}

// TestReaderAcceptsCRLF checks that a CRLF client is accepted.
func TestReaderAcceptsCRLF(t *testing.T) {
	reader := NewReader(strings.NewReader("{\"a\":1}\r\n"))
	record, err := reader.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(record) != `{"a":1}` {
		t.Errorf("the record is %q", record)
	}
}

// TestReaderSkipsBlankLines checks that a stray newline does not
// produce an empty record.
func TestReaderSkipsBlankLines(t *testing.T) {
	reader := NewReader(strings.NewReader("\n\n{\"a\":1}\n"))
	record, err := reader.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(record) != `{"a":1}` {
		t.Errorf("the record is %q", record)
	}
}

// TestWriteRecordIsOneLine checks the output framing.
func TestWriteRecordIsOneLine(t *testing.T) {
	var out bytes.Buffer
	if err := WriteRecord(&out, map[string]any{"a": 1}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if out.String() != "{\"a\":1}\n" {
		t.Errorf("the output is %q", out.String())
	}
}

// TestServeAnswersAStateCommand checks the basic command round trip.
func TestServeAnswersAStateCommand(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"get_state"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	if len(parsed) == 0 {
		t.Fatal("no record was written")
	}
	last := responseFor(t, parsed, "get_state")
	if last["type"] != "response" {
		t.Errorf("the record is %v", last)
	}
	if last["success"] != true {
		t.Errorf("the response is %v", last)
	}
	if last["id"] != "r1" {
		t.Errorf("the id is %v, want r1", last["id"])
	}
}

// TestServeRejectsAnUnknownCommand checks that an unknown command is
// an error, not a silent success.
func TestServeRejectsAnUnknownCommand(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"nonsense"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "nonsense")
	if last["success"] != false {
		t.Errorf("the response is %v, want a failure", last)
	}
	if !strings.Contains(last["error"].(string), "Unknown command") {
		t.Errorf("the error is %v", last["error"])
	}
}

// TestServeReportsAMalformedRecord checks that a bad line is reported
// and the stream continues.
//
// One bad line must not kill a session: the client and the harness
// disagree about one record, not about the whole protocol.
func TestServeReportsAMalformedRecord(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := "not json\n" + `{"id":"r1","type":"get_state"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	if len(parsed) < 2 {
		t.Fatalf("the records are %v", parsed)
	}
	parse := responseFor(t, parsed, "parse")
	if parse["success"] != false {
		t.Errorf("the parse response is %v", parse)
	}
	state := responseFor(t, parsed, "get_state")
	if state["success"] != true {
		t.Errorf("the state response is %v", state)
	}
}

// TestPromptStreamsTheEvents checks the whole run.
//
// The response goes out when the prompt is accepted, and the events
// follow. A client that waits for the run's end before reading would
// deadlock on a full pipe, so the response must come first.
func TestPromptStreamsTheEvents(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{Role: llm.RoleAssistant, Content: "RPC_OK"},
	}}
	server, _, _ := newTestServer(t, provider)

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"prompt","message":"hello"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	if len(parsed) < 3 {
		t.Fatalf("the records are %v", parsed)
	}

	// The response comes first.
	if parsed[0]["command"] != "prompt" {
		t.Errorf("the first record is %v", parsed[0])
	}
	if parsed[0]["success"] != true {
		t.Errorf("the response is %v", parsed[0])
	}

	// The events follow, and the last one says the run is settled.
	var types []string
	for _, record := range parsed {
		types = append(types, record["type"].(string))
	}
	if types[len(types)-1] != EventAgentSettled {
		t.Errorf("the last event is %q", types[len(types)-1])
	}

	found := false
	for _, record := range parsed {
		if record["type"] != EventMessageEnd {
			continue
		}
		data, _ := record["data"].(map[string]any)
		message, _ := data["message"].(map[string]any)
		if message["content"] == "RPC_OK" {
			found = true
		}
	}
	if !found {
		t.Errorf("the answer is not in the events: %v", parsed)
	}
}

// TestPromptRejectsAnEmptyMessage checks that an empty prompt is an
// error.
func TestPromptRejectsAnEmptyMessage(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"prompt","message":"   "}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "prompt")
	if last["success"] != false {
		t.Errorf("the response is %v", last)
	}
}

// TestGetMessagesReturnsTheHistory checks the history command.
//
// The history is read after the run settles: the prompt runs
// asynchronously, so a client that reads the history before the run
// ends sees an empty one. That is the protocol working as designed,
// not a bug.
func TestGetMessagesReturnsTheHistory(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{Role: llm.RoleAssistant, Content: "the answer"},
	}}
	server, _, _ := newTestServer(t, provider)
	c := newClient(t, server)
	defer c.close()

	c.promptAndWait("hello")

	c.send(map[string]any{"id": "r2", "type": CmdGetMessages})
	response := c.response(CmdGetMessages)
	data, _ := response["data"].(map[string]any)
	messages, _ := data["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("the messages are %v", messages)
	}
	assistant, _ := messages[1].(map[string]any)
	if assistant["content"] != "the answer" {
		t.Errorf("the assistant message is %v", assistant)
	}
}

// TestSetModelSwitchesTheModel checks the model command.
func TestSetModelSwitchesTheModel(t *testing.T) {
	server, _, sess := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"set_model","model":"other-model"}` + "\n" +
		`{"id":"r2","type":"get_state"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "get_state")
	data, _ := last["data"].(map[string]any)
	if data["model"] != "other-model" {
		t.Errorf("the model is %v", data["model"])
	}
	if sess.Meta.Model != "other-model" {
		t.Errorf("the session's model is %q", sess.Meta.Model)
	}
}

// TestGetCommandsListsTheCommands checks the command list.
func TestGetCommandsListsTheCommands(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"get_commands"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "get_commands")
	data, _ := last["data"].(map[string]any)
	commands, _ := data["commands"].([]any)
	if len(commands) == 0 {
		t.Fatal("no command was reported")
	}
	first, _ := commands[0].(map[string]any)
	if first["name"] != "hello" {
		t.Errorf("the command is %v", first)
	}
}

// TestBashReportsTheExitCode checks the bash command.
func TestBashReportsTheExitCode(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})
	server.Bash = func(ctx context.Context, command string) (string, int, error) {
		if command == "false" {
			return "", 1, errors.New("failed")
		}
		return "ok\n", 0, nil
	}

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"bash","command":"false"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "bash")
	data, _ := last["data"].(map[string]any)
	if data["exitCode"].(float64) != 1 {
		t.Errorf("the exit code is %v", data["exitCode"])
	}
	if data["isError"] != true {
		t.Errorf("the result is %v", data)
	}
}

// TestBashIsGated checks that a vetoed command does not run.
//
// The command is the client's own, but it still runs on the user's
// machine, so the same extension that can veto a "!" command can veto
// this one.
func TestBashIsGated(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	ran := false
	server.Bash = func(ctx context.Context, command string) (string, int, error) {
		ran = true
		return "ok\n", 0, nil
	}
	server.Gate = func(command string) error {
		return errors.New("blocked by an extension")
	}

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"bash","command":"rm -rf /"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "bash")
	if last["success"] != false {
		t.Errorf("the response is %v, want a failure", last)
	}
	if !strings.Contains(last["error"].(string), "blocked by an extension") {
		t.Errorf("the error is %v", last["error"])
	}
	if ran {
		t.Error("the command ran even though it was gated")
	}
}

// TestSetSessionNameNamesTheSession checks the session name command.
func TestSetSessionNameNamesTheSession(t *testing.T) {
	server, _, sess := newTestServer(t, &fakeProvider{})

	var out bytes.Buffer
	server.out = &out

	input := `{"id":"r1","type":"set_session_name","name":"the name"}` + "\n" +
		`{"id":"r2","type":"get_state"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	parsed := records(t, &out)
	last := responseFor(t, parsed, "get_state")
	data, _ := last["data"].(map[string]any)
	if data["sessionName"] != "the name" {
		t.Errorf("the session name is %v", data["sessionName"])
	}
	if sess.Meta.Name != "the name" {
		t.Errorf("the session's name is %q", sess.Meta.Name)
	}
}

// TestGetEntriesReportsTheTree checks the session tree command.
func TestGetEntriesReportsTheTree(t *testing.T) {
	provider := &fakeProvider{responses: []llm.Message{
		{Role: llm.RoleAssistant, Content: "the answer"},
	}}
	server, _, _ := newTestServer(t, provider)
	c := newClient(t, server)
	defer c.close()

	c.promptAndWait("hello")

	c.send(map[string]any{"id": "r2", "type": CmdGetEntries})
	response := c.response(CmdGetEntries)
	data, _ := response["data"].(map[string]any)
	entries, _ := data["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("no entry was reported")
	}
}

// TestAbortStopsTheRun checks the abort command.
//
// The run is cancelled, so the settled event says the run was aborted.
func TestAbortStopsTheRun(t *testing.T) {
	// A provider that never answers: the run only ends when it is
	// aborted.
	provider := &blockingProvider{}
	server, _, _ := newTestServer(t, provider)
	c := newClient(t, server)
	defer c.close()

	c.send(map[string]any{"id": "r1", "type": CmdPrompt, "message": "hello"})
	response := c.response(CmdPrompt)
	if response["success"] != true {
		t.Fatalf("the prompt was rejected: %v", response)
	}

	c.send(map[string]any{"id": "r2", "type": CmdAbort})
	abort := c.response(CmdAbort)
	if abort["success"] != true {
		t.Fatalf("the abort was rejected: %v", abort)
	}

	settled := c.settled()
	data, _ := settled["data"].(map[string]any)
	if data["aborted"] != true {
		t.Errorf("the settled event is %v, want an aborted one", settled)
	}
}

// blockingProvider never answers, so a run only ends when it is
// aborted.
type blockingProvider struct{}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

