// Package rpc implements the harness's RPC mode: a long-lived
// subprocess driven by JSON records on stdin and stdout.
//
// The protocol is the same shape as pi's: commands go in, responses and
// events come out, and every record is one JSON object on its own line.
//
//	{"id":"req-1","type":"prompt","message":"Hello"}
//	{"id":"req-1","type":"response","command":"prompt","success":true,"data":{...}}
//
// The framing is strict JSONL: records are separated by LF and by
// nothing else. A record's strings may hold U+2028 and U+2029, which
// are ordinary characters in JSON, so the reader must not treat them as
// line breaks.
//
// The mode exists so the harness can be driven from another program —
// an IDE, a script in any language, a test harness — without going
// through the REPL's human interface.
package rpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SchemaVersion is the wire protocol schema version emitted by the
// RPC server and accepted from clients. Clients that omit
// schema_version are treated as v1 ("1.0") for backward compatibility.
const SchemaVersion = "1.0"

// ValidateSchemaVersion checks that v is empty or belongs to major
// version 1 (for example "1" or "1.0").
func ValidateSchemaVersion(v string) error {
	v = strings.TrimSpace(v)
	if v == "" || v == "1" || strings.HasPrefix(v, "1.") {
		return nil
	}
	return fmt.Errorf("unsupported schema_version %q (supported major version: 1)", v)
}

// RecordType names a record's kind.
//
// The three kinds share one stream: a client reads them all and tells
// them apart by the type field.
type RecordType string

const (
	// RecordResponse answers one command.
	RecordResponse RecordType = "response"
	// RecordEvent reports session activity.
	RecordEvent RecordType = "event"
)

// Command is one request from the client.
//
// The Type field selects the handler; the rest of the fields are that
// command's arguments. The whole struct is decoded at once, so a
// command's arguments are fields on one type rather than a nested
// object — the same shape pi's protocol uses.
type Command struct {
	// SchemaVersion identifies the client's protocol revision (optional;
	// omitted defaults to "1.0").
	SchemaVersion string `json:"schema_version,omitempty"`
	// ID correlates a response with its command. It is optional: a
	// client that sends one command at a time does not need it.
	ID string `json:"id,omitempty"`
	// Type selects the handler.
	Type string `json:"type"`

	// raw is the command's original JSON. Each handler decodes the
	// arguments it needs, so the dispatcher does not have to know
	// every command's fields.
	raw json.RawMessage
}

// Response is the answer to one command.
//
// A response always carries the command's type, so a client can tell
// which command failed even when the failure happened before the
// command was understood.
type Response struct {
	// SchemaVersion identifies the wire protocol revision.
	SchemaVersion string `json:"schema_version,omitempty"`
	// ID repeats the command's ID.
	ID string `json:"id,omitempty"`
	// Type is always "response".
	Type string `json:"type"`
	// Command is the command's type.
	Command string `json:"command"`
	// Success reports whether the command was handled.
	Success bool `json:"success"`
	// Data is the command's result, when there is one.
	Data any `json:"data,omitempty"`
	// Error explains a failure.
	Error string `json:"error,omitempty"`
}

// Event is one session event.
//
// The harness's event bus already has a vocabulary; the RPC events
// reuse its names where the concepts match, so a client that knows the
// REPL's events knows the RPC ones.
type Event struct {
	// SchemaVersion identifies the wire protocol revision.
	SchemaVersion string `json:"schema_version,omitempty"`
	// Type names the event.
	Type string `json:"type"`
	// Data carries the event's payload.
	Data map[string]any `json:"data,omitempty"`
}

// The event types the RPC mode emits.
//
// They mirror the harness's own event names where the concept matches,
// and add the ones that only make sense in a headless run.
const (
	// EventAgentStart fires when a run starts.
	EventAgentStart = "agent_start"
	// EventAgentEnd fires when a run ends.
	EventAgentEnd = "agent_end"
	// EventAgentSettled fires when the harness has no more automatic
	// work for the current run. A client that needs to know the
	// harness is done waits for this one, not for agent_end: a run may
	// still be followed by a retry.
	EventAgentSettled = "agent_settled"
	// EventTurnStart fires when one turn starts.
	EventTurnStart = "turn_start"
	// EventTurnEnd fires when one turn ends.
	EventTurnEnd = "turn_end"
	// EventMessageStart fires when a message starts.
	EventMessageStart = "message_start"
	// EventMessageUpdate fires for each streamed delta.
	EventMessageUpdate = "message_update"
	// EventMessageEnd fires when a message completes.
	EventMessageEnd = "message_end"
	// EventToolExecutionStart fires when a tool call starts.
	EventToolExecutionStart = "tool_execution_start"
	// EventToolExecutionEnd fires when a tool call ends.
	EventToolExecutionEnd = "tool_execution_end"
	// EventRequestStart fires when a model request begins.
	EventRequestStart = "request_start"
	// EventSessionBeforeCompact fires before the history is compacted.
	EventSessionBeforeCompact = "session_before_compact"
	// EventSessionCompact fires after the history is compacted.
	EventSessionCompact = "session_compact"
)

// Success builds a successful response.
//
// The data is left out when it is nil, so a command with no result
// produces a response with no "data" field rather than a null one.
func Success(id, command string, data any) *Response {
	return &Response{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Type:          string(RecordResponse),
		Command:       command,
		Success:       true,
		Data:          data,
	}
}

// Failure builds a failed response.
func Failure(id, command, message string) *Response {
	return &Response{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Type:          string(RecordResponse),
		Command:       command,
		Success:       false,
		Error:         message,
	}
}

// WriteRecord writes one record as a JSON line.
//
// The writer is buffered, so a caller writes many records cheaply and
// flushes when it needs the bytes out.
func WriteRecord(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("rpc: marshal: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("rpc: write: %w", err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return fmt.Errorf("rpc: write: %w", err)
	}
	return nil
}

// Reader reads JSON records from a stream.
//
// The framing is LF-only. A generic line reader that also splits on
// U+2028 or U+2029 would cut records in half, because those
// characters are legal inside a JSON string.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader creates a reader over r.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	// A record can be large — a tool result or a long message — so the
	// default 64 KiB limit is raised. The limit still exists: a
	// stream that never ends must not grow without bound.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &Reader{scanner: scanner}
}

// Read returns the next record's raw JSON.
//
// A blank line is skipped: a client that writes a trailing newline
// should not produce an empty record. The error is io.EOF at the end
// of the stream.
func (r *Reader) Read() ([]byte, error) {
	for {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return nil, fmt.Errorf("rpc: read: %w", err)
			}
			return nil, io.EOF
		}
		line := r.scanner.Bytes()
		// A CRLF client is accepted: the carriage return is not part
		// of the record.
		trimmed := strings.TrimSuffix(string(line), "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		return []byte(trimmed), nil
	}
}

// WriteCommand writes a command as a JSON line.
//
// It is the client's half of the protocol, and it is exported so a Go
// program can drive the harness without reimplementing the framing.
func WriteCommand(w io.Writer, command any) error {
	return WriteRecord(w, command)
}
