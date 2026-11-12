package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// client drives a server the way a real client does.
//
// The protocol is asynchronous: a prompt's response comes when the
// prompt is accepted, and the run's events follow. A client that wants
// the answer waits for the settled event, and a client that wants to
// read the history after a prompt waits for the same event.
//
// The client is here so the tests exercise the protocol the way a
// client does — by correlating responses and events, not by counting
// records.
type client struct {
	t       *testing.T
	writer  *io.PipeWriter
	reader  *bufio.Reader
	mu      sync.Mutex
	records []map[string]any
}

// newClient starts a server over a pipe and returns a client for it.
func newClient(t *testing.T, server *Server) *client {
	t.Helper()

	// A pipe stands in for the subprocess boundary: the client writes
	// commands on one end and reads records on the other.
	commandReader, commandWriter := io.Pipe()
	recordBuffer := newSyncBuffer()

	server.out = recordBuffer
	go func() {
		_ = server.Serve(context.Background(), commandReader)
		_ = commandWriter.Close()
		recordBuffer.close()
	}()

	return &client{
		t:      t,
		writer: commandWriter,
		reader: bufio.NewReader(recordBuffer),
	}
}

// syncBuffer is a buffer that is safe to write and read at the same
// time.
//
// A read on an empty buffer waits rather than reporting the end of the
// stream: the server writes the records as the run proceeds, and the
// client must not see the end of the stream before the run is done.
type syncBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newSyncBuffer() *syncBuffer {
	b := &syncBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	b.cond.Broadcast()
	return len(p), nil
}

func (b *syncBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// close ends the stream.
func (b *syncBuffer) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
}

// send writes a command.
func (c *client) send(command any) {
	c.t.Helper()
	if err := WriteCommand(c.writer, command); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// next reads the next record.
func (c *client) next() map[string]any {
	c.t.Helper()
	line, err := c.reader.ReadString('\n')
	if err != nil && line == "" {
		c.t.Fatalf("read: %v", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return c.next()
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		c.t.Fatalf("the record %q is not JSON: %v", line, err)
	}
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
	return record
}

// response reads records until the response to a command arrives.
func (c *client) response(command string) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record := c.next()
		if record["type"] == "response" && record["command"] == command {
			return record
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("no response to %q arrived", command)
		}
	}
}

// settled reads records until the run settles.
func (c *client) settled() map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record := c.next()
		if record["type"] == EventAgentSettled {
			return record
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the run did not settle")
		}
	}
}

// promptAndWait sends a prompt and waits for the run to settle.
//
// It is the sequence a real client follows: send, wait for the
// response, then wait for the settled event.
func (c *client) promptAndWait(message string) map[string]any {
	c.t.Helper()
	c.send(map[string]any{"id": "p1", "type": CmdPrompt, "message": message})
	response := c.response(CmdPrompt)
	if response["success"] != true {
		c.t.Fatalf("the prompt was rejected: %v", response)
	}
	return c.settled()
}

// close ends the client's side of the pipe.
func (c *client) close() {
	_ = c.writer.Close()
}
