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
	"github.com/NanQiaoMi/auto-harness/contextmgr"
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

func TestNewSessionCanBeCancelled(t *testing.T) {
	server, bus, sess := newTestServer(t, &fakeProvider{})
	server.SessionDir = t.TempDir()
	id := sess.Meta.ID

	bus.On(ext.EventSessionBeforeSwitch, func(e *ext.Event) error {
		if e.Reason != "new" {
			t.Errorf("reason = %q, want new", e.Reason)
		}
		e.Cancel = true
		e.BlockReason = "not now"
		return nil
	})

	resp := server.newSession(&Command{Type: CmdNewSession})
	if resp.Success {
		t.Error("the vetoed new_session reported success")
	}
	if !strings.Contains(resp.Error, "not now") {
		t.Errorf("error = %q, want the extension's reason", resp.Error)
	}
	if server.Session.Meta.ID != id {
		t.Error("the session was replaced despite the veto")
	}
}

func TestNewSessionSwitchesCurrentSession(t *testing.T) {
	server, _, old := newTestServer(t, &fakeProvider{})
	server.SessionDir = t.TempDir()
	oldID := old.Meta.ID

	resp := server.newSession(&Command{Type: CmdNewSession})
	if !resp.Success {
		t.Fatalf("new_session = %#v", resp)
	}
	current := server.CurrentSession()
	if current == nil || current.Meta.ID == oldID {
		t.Fatalf("current session = %#v, old = %q", current, oldID)
	}
	if server.Agent.Session != current {
		t.Fatal("the agent kept the previous session")
	}
}

func TestNewSessionRejectedWhileStreaming(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})
	server.SessionDir = t.TempDir()
	server.setStreaming(true)
	resp := server.newSession(&Command{Type: CmdNewSession})
	if resp.Success || !strings.Contains(resp.Error, "run is in progress") {
		t.Fatalf("streaming new_session = %#v", resp)
	}
}

func TestGetContextReportsBudget(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})

	response := server.getContext(&Command{Type: CmdGetContext})
	if !response.Success {
		t.Fatalf("disabled context query failed: %s", response.Error)
	}
	payload, ok := response.Data.(ContextPayload)
	if !ok || payload.Enabled {
		t.Fatalf("payload = %#v, want a disabled context manager", response.Data)
	}

	server.Agent.SetContextManager(contextmgr.NewManager(contextmgr.Options{
		ContextWindow:   64000,
		MaxOutputTokens: 4096,
		ReserveTokens:   256,
	}, nil, nil, nil))
	response = server.getContext(&Command{Type: CmdGetContext})
	payload, ok = response.Data.(ContextPayload)
	if !response.Success || !ok || !payload.Enabled || payload.Status == nil {
		t.Fatalf("context response = %#v", response)
	}
	if payload.Status.ContextWindow != 64000 {
		t.Fatalf("context window = %d", payload.Status.ContextWindow)
	}
	if payload.Status.PromptBudget != 64000-4096-256 {
		t.Fatalf("prompt budget = %d", payload.Status.PromptBudget)
	}
}

func TestProfileAndPluginQueriesUseInjectedProviders(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})
	server.Profile = func() any {
		return ProfilePayload{
			Enabled:     true,
			Name:        "coding",
			Model:       "test-model",
			ActiveTools: []string{"read"},
			Tools:       []string{"read"},
		}
	}
	server.Plugins = func() any {
		return PluginsPayload{Plugins: []PluginInfo{{
			Name:         "pdf-tools",
			Version:      "0.1.0",
			Selected:     true,
			State:        "loaded",
			RestartMode:  "on_failure",
			RestartCount: 2,
			MaxRestarts:  3,
			Healthy:      true,
			LastExit:     "exit status 1",
		}}}
	}

	profile := server.getProfile(&Command{ID: "p", Type: CmdGetProfile})
	if !profile.Success {
		t.Fatalf("get_profile: %s", profile.Error)
	}
	payload, ok := profile.Data.(ProfilePayload)
	if !ok || !payload.Enabled || payload.Name != "coding" {
		t.Fatalf("profile payload = %#v", profile.Data)
	}

	plugins := server.getPlugins(&Command{ID: "q", Type: CmdGetPlugins})
	if !plugins.Success {
		t.Fatalf("get_plugins: %s", plugins.Error)
	}
	catalog, ok := plugins.Data.(PluginsPayload)
	if !ok || len(catalog.Plugins) != 1 || catalog.Plugins[0].Name != "pdf-tools" {
		t.Fatalf("plugins payload = %#v", plugins.Data)
	}
	if got := catalog.Plugins[0]; got.RestartMode != "on_failure" ||
		got.RestartCount != 2 || got.MaxRestarts != 3 ||
		!got.Healthy || got.LastExit != "exit status 1" {
		t.Fatalf("supervision payload = %#v", got)
	}
}

func TestProfileAndPluginQueriesHaveEmptyDefaults(t *testing.T) {
	server, _, _ := newTestServer(t, &fakeProvider{})
	profile := server.getProfile(&Command{Type: CmdGetProfile})
	if !profile.Success {
		t.Fatalf("get_profile: %s", profile.Error)
	}
	plugins := server.getPlugins(&Command{Type: CmdGetPlugins})
	if !plugins.Success {
		t.Fatalf("get_plugins: %s", plugins.Error)
	}
	payload, ok := plugins.Data.(PluginsPayload)
	if !ok || payload.Plugins == nil {
		t.Fatalf("plugins payload = %#v", plugins.Data)
	}
}

func TestForkCanBeCancelled(t *testing.T) {
	server, bus, sess := newTestServer(t, &fakeProvider{})
	server.SessionDir = t.TempDir()
	if err := sess.Append(llm.Message{Role: llm.RoleUser, Content: "hi"}); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(ForkCommand{Type: CmdFork})
	if err != nil {
		t.Fatal(err)
	}

	unsubscribe := bus.On(ext.EventSessionBeforeFork, func(e *ext.Event) error {
		if e.Reason != "fork" {
			t.Errorf("reason = %q, want fork", e.Reason)
		}
		if e.EntryID == "" {
			t.Error("the event carries no entry to fork from")
		}
		e.Cancel = true
		return nil
	})

	resp := server.fork(&Command{Type: CmdFork, raw: raw}, "fork")
	if resp.Success {
		t.Error("the vetoed fork reported success")
	}
	if list, err := session.List(server.SessionDir); err != nil {
		t.Fatal(err)
	} else if len(list) != 0 {
		t.Errorf("a cancelled fork wrote %d sessions", len(list))
	}

	// The same command with the veto lifted writes the fork into the
	// configured session directory.
	unsubscribe()
	resp = server.fork(&Command{Type: CmdFork, raw: raw}, "fork")
	if !resp.Success {
		t.Fatalf("the approved fork failed: %s", resp.Error)
	}
	if list, err := session.List(server.SessionDir); err != nil {
		t.Fatal(err)
	} else if len(list) != 1 {
		t.Errorf("the approved fork wrote %d sessions, want 1", len(list))
	}
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
	response := responseFor(t, parsed, "set_model")
	modelInfo, _ := response["data"].(map[string]any)
	if provider, _ := modelInfo["provider"].(string); provider == "" {
		t.Fatalf("set_model did not report the active provider: %v", response)
	}
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

// TestClientKeepsTheRecordsItSkips checks that a client does not lose
// a record it read ahead of the one it was looking for.
//
// The order between a response and the run's events is not fixed: the
// settled event can arrive before the response that ended the run. A
// client that throws away what it read early loses the settled event,
// and then waits for a second copy that never comes — the abort test
// hung that way under the race detector.
//
// The test drives the client with the records in the awkward order on
// purpose, so it fails on the old client every time rather than once
// in a while.
func TestClientKeepsTheRecordsItSkips(t *testing.T) {
	// The test never sends a command, so the pipe's reading end is
	// closed right away: only the records matter here.
	commandReader, commandWriter := io.Pipe()
	commandReader.Close()
	recordBuffer := newSyncBuffer()
	c := &client{
		t:      t,
		writer: commandWriter,
		source: recordBuffer,
		lines:  make(chan string, 256),
	}
	go c.pump()
	defer commandWriter.Close()

	// The settled event comes first, and the response to the abort
	// comes second.
	recordBuffer.Write([]byte(`{"type":"agent_settled","data":{"aborted":true}}` + "\n"))
	recordBuffer.Write([]byte(`{"type":"response","command":"abort","success":true}` + "\n"))

	// The client looks for the response first: it has to skip past
	// the settled event to find it.
	abort := c.response(CmdAbort)
	if abort["success"] != true {
		t.Fatalf("the abort response is %v", abort)
	}

	// And the settled event it skipped is still there.
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
