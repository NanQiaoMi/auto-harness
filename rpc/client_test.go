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
	t      *testing.T
	writer *io.PipeWriter
	// source is where the records come from.
	source io.Reader
	// lines carries the records as they are read.
	//
	// The reading happens in one goroutine, so a lookup that gives up
	// does not leave a half-finished read behind to trip over the
	// next one.
	lines chan string
	mu    sync.Mutex
	// records holds every record read so far.
	//
	// The records are kept because the order between a response and
	// the run's events is not fixed: the settled event can arrive
	// before the response that ended the run. A client that throws
	// away what it read early loses it, and then waits for a second
	// copy that never comes.
	records []map[string]any
	// responses and settles are the cursors of the two kinds of
	// lookup.
	//
	// They are separate because a lookup that does not match must
	// not consume what another one is looking for: the settled event
	// can arrive before the response that ended the run, and a client
	// that read it early still has to find it later.
	responses int
	settles   int
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

	c := &client{
		t:      t,
		writer: commandWriter,
		source: recordBuffer,
		lines:  make(chan string, 256),
	}
	go c.pump()
	return c
}

// pump moves the records from the pipe into the channel.
//
// It is the only reader: the pipe read blocks, so the lookups take
// from the channel instead, and a lookup that gives up leaves the
// reader alone rather than abandoning a read half way.
func (c *client) pump() {
	reader := bufio.NewReader(c.source)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			c.lines <- line
		}
		if err != nil {
			close(c.lines)
			return
		}
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

// readOne waits for the next record.
//
// The wait has a deadline: a record that never comes must fail the
// test rather than hang it. The reader goroutine keeps reading either
// way, so giving up here does not disturb the next lookup.
func (c *client) readOne(within time.Duration) map[string]any {
	c.t.Helper()
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				c.t.Fatalf("the stream ended before the record arrived")
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				c.t.Fatalf("the record %q is not JSON: %v", line, err)
			}
			c.mu.Lock()
			c.records = append(c.records, record)
			c.mu.Unlock()
			return record
		case <-time.After(within):
			c.t.Fatalf("no record arrived within %s", within)
		}
	}
}

// take returns the first record that matches, reading more when the
// ones already read do not match.
//
// The records that do not match are kept: they were read ahead of the
// one being looked for, and a later lookup may well be the one they
// are. That is the whole point — the order between a response and the
// run's events is not fixed, so the settled event can arrive before
// the response that ended the run.
//
// Each kind of lookup has its own cursor, so looking for a response
// does not consume the settled event that a later lookup wants.
func (c *client) take(cursor *int, matches func(map[string]any) bool) map[string]any {
	c.t.Helper()
	for {
		c.mu.Lock()
		for *cursor < len(c.records) {
			record := c.records[*cursor]
			*cursor++
			if matches(record) {
				c.mu.Unlock()
				return record
			}
		}
		c.mu.Unlock()
		c.readOne(5 * time.Second)
	}
}

// response reads records until the response to a command arrives.
func (c *client) response(command string) map[string]any {
	c.t.Helper()
	return c.take(&c.responses, func(r map[string]any) bool {
		return r["type"] == "response" && r["command"] == command
	})
}

// settled reads records until the run settles.
func (c *client) settled() map[string]any {
	c.t.Helper()
	return c.take(&c.settles, func(r map[string]any) bool {
		return r["type"] == EventAgentSettled
	})
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
