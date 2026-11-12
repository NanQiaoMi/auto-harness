package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Transport carries JSON-RPC messages between the client and a server.
type Transport interface {
	// Call sends a request and waits for the matching response.
	Call(ctx context.Context, method string, params any, result any) error
	// Notify sends a notification (no response expected).
	Notify(ctx context.Context, method string, params any) error
	// Close releases the transport.
	Close() error
}

// StdioTransport speaks JSON-RPC over a child process's stdin/stdout.
//
// The wire format is newline-delimited JSON: one JSON object per line.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu      sync.Mutex
	enc     *json.Encoder
	dec     *json.Decoder
	nextID  int
	pending map[int]chan *Response
	closed  bool
	err     error
}

// NewStdioTransport starts a command and speaks MCP to it over stdio.
func NewStdioTransport(ctx context.Context, command string, args []string, env []string, dir string) (*StdioTransport, error) {
	if command == "" {
		return nil, fmt.Errorf("mcp: no command")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %s: %w", command, err)
	}
	t := &StdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		enc:     json.NewEncoder(stdin),
		dec:     json.NewDecoder(bufio.NewReaderSize(stdout, 64*1024)),
		pending: map[int]chan *Response{},
	}
	go t.readLoop()
	return t, nil
}

// readLoop dispatches incoming messages until the stream ends.
func (t *StdioTransport) readLoop() {
	for {
		var resp Response
		if err := t.dec.Decode(&resp); err != nil {
			t.failAll(err)
			return
		}
		t.mu.Lock()
		ch, ok := t.pending[resp.ID]
		delete(t.pending, resp.ID)
		t.mu.Unlock()
		if ok {
			ch <- &resp
		}
	}
}

// failAll wakes every waiting call with the error.
func (t *StdioTransport) failAll(err error) {
	t.mu.Lock()
	t.err = err
	pending := t.pending
	t.pending = map[int]chan *Response{}
	t.mu.Unlock()
	for _, ch := range pending {
		ch <- &Response{Error: &RPCError{Code: -1, Message: err.Error()}}
	}
}

// Call sends a request and waits for its response.
func (t *StdioTransport) Call(ctx context.Context, method string, params any, result any) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("mcp: transport closed")
	}
	t.nextID++
	id := t.nextID
	ch := make(chan *Response, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	req := Request{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	t.mu.Lock()
	err := t.enc.Encode(req)
	t.mu.Unlock()
	if err != nil {
		t.forget(id)
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		t.forget(id)
		return ctx.Err()
	}
}

// forget drops a pending call.
func (t *StdioTransport) forget(id int) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

// Notify sends a notification.
func (t *StdioTransport) Notify(ctx context.Context, method string, params any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return fmt.Errorf("mcp: transport closed")
	}
	n := Notification{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		n.Params = b
	}
	return t.enc.Encode(n)
}

// Close shuts the child process down: close stdin so it sees EOF,
// then terminate the process group if it does not exit.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	_ = t.stdin.Close()
	if t.cmd != nil && t.cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = t.cmd.Process.Kill()
			<-done
		}
	}
	return nil
}

// HTTPTransport speaks MCP over streamable HTTP.
//
// A POST carries a JSON-RPC message; the server answers with either a
// JSON body or an SSE stream. Session identifiers come back in the
// Mcp-Session-Id header and are echoed on later requests.
type HTTPTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu        sync.Mutex
	sessionID string
	nextID    int
}

// NewHTTPTransport creates a transport for a streamable HTTP server.
func NewHTTPTransport(url string, headers map[string]string) *HTTPTransport {
	return &HTTPTransport{
		url:     url,
		headers: headers,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// Call sends a request and waits for its response.
func (t *HTTPTransport) Call(ctx context.Context, method string, params any, result any) error {
	req := Request{JSONRPC: "2.0", ID: t.nextIDValue(), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	resp, err := t.send(ctx, req)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

// nextIDValue returns the next request id.
func (t *HTTPTransport) nextIDValue() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	return t.nextID
}

// Notify sends a notification.
func (t *HTTPTransport) Notify(ctx context.Context, method string, params any) error {
	n := Notification{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		n.Params = b
	}
	_, err := t.send(ctx, n)
	return err
}

// send posts a message and reads the response.
func (t *HTTPTransport) send(ctx context.Context, msg any) (*Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()

	httpResp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", t.url, err)
	}
	defer httpResp.Body.Close()

	// The server may hand out a session id on the first response.
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("mcp: %s: HTTP %d: %s", t.url, httpResp.StatusCode, strings.TrimSpace(string(data)))
	}

	// The response may be a plain JSON body or an SSE stream.
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream") {
		return parseSSE(data)
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mcp: bad response: %w", err)
	}
	return &resp, nil
}

// parseSSE extracts the first JSON-RPC response from an SSE payload.
func parseSSE(data []byte) (*Response, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var resp Response
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			continue
		}
		return &resp, nil
	}
	return nil, fmt.Errorf("mcp: no response in SSE stream")
}

// Close releases the transport.
func (t *HTTPTransport) Close() error { return nil }
