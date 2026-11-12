package ext

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Protocol is the wire protocol between the harness and an external
// extension process. It is newline-delimited JSON over the child's
// stdin/stdout:
//
//	harness -> extension: {"id":1,"method":"tool.execute","params":{...}}
//	extension -> harness: {"id":1,"result":{...}}
//	extension -> harness: {"method":"event","params":{"event":"tool_execution_start",...}}
//
// The extension may also implement a "manifest" method returning its name
// and the tools it provides.
type Protocol struct {
	// ID correlates a request with its response.
	ID int `json:"id,omitempty"`
	// Method names the operation for requests, or "event" for
	// notifications pushed by the extension.
	Method string `json:"method,omitempty"`
	// Params carries the method arguments or event payload.
	Params json.RawMessage `json:"params,omitempty"`
	// Result carries a successful response.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is set when the extension failed.
	Error *RPCError `json:"error,omitempty"`
}

// RPCError is the error payload of a failed response.
type RPCError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

// Method names used by the protocol.
const (
	// MethodManifest asks the extension to describe itself.
	MethodManifest = "manifest"
	// MethodToolExecute runs a tool the extension registered.
	MethodToolExecute = "tool.execute"
	// MethodShutdown asks the extension to exit cleanly.
	MethodShutdown = "shutdown"
	// MethodEvent is a notification from the extension.
	MethodEvent = "event"
)

// Manifest describes an extension process.
type Manifest struct {
	// Name identifies the extension.
	Name string `json:"name"`
	// Description is a human-readable summary.
	Description string `json:"description,omitempty"`
	// Tools lists the tools the extension provides.
	Tools []ManifestTool `json:"tools,omitempty"`
}

// ManifestTool describes one tool offered by an extension process.
type ManifestTool struct {
	Name        string         `json:"name"`
	Label       string         `json:"label,omitempty"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolResult is the outcome of a remote tool execution.
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// ExecParams are the arguments of tool.execute.
type ExecParams struct {
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

// EventParams is the payload of an event notification.
type EventParams struct {
	// Event is the ext.EventType name.
	Event string `json:"event"`
	// Text carries delta or notice content.
	Text string `json:"text,omitempty"`
	// Data carries arbitrary structured data.
	Data map[string]any `json:"data,omitempty"`
}

// rpcConn is a JSON-RPC connection over a child's stdio.
type rpcConn struct {
	mu      sync.Mutex
	enc     *json.Encoder
	dec     *json.Decoder
	closer  io.Closer
	nextID  int
	pending map[int]chan rpcResponse
	closed  bool
	// timeout bounds how long a call waits for its response.
	timeout time.Duration
}

type rpcResponse struct {
	Result json.RawMessage
	Error  *RPCError
}

// defaultRPCTimeout bounds a single request/response round trip.
const defaultRPCTimeout = 30 * time.Second

// newRPCConn wraps an extension process's streams.
func newRPCConn(in io.Reader, out io.Writer, closer io.Closer) *rpcConn {
	return &rpcConn{
		enc:     json.NewEncoder(out),
		dec:     json.NewDecoder(bufio.NewReaderSize(in, 64*1024)),
		closer:  closer,
		pending: map[int]chan rpcResponse{},
		timeout: defaultRPCTimeout,
	}
}

// call sends a request and waits for its response.
func (c *rpcConn) call(method string, params any, result any) error {
	c.mu.Lock()
	timeout := c.timeout
	c.mu.Unlock()
	return c.callTimeout(method, params, result, timeout)
}

// callTimeout sends a request and waits at most timeout for its
// response. A non-positive timeout waits forever.
func (c *rpcConn) callTimeout(method string, params any, result any, timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("ext: rpc connection closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload := Protocol{ID: id, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.forget(id)
			return err
		}
		payload.Params = b
	}
	c.mu.Lock()
	err := c.enc.Encode(payload)
	c.mu.Unlock()
	if err != nil {
		c.forget(id)
		return err
	}

	var resp rpcResponse
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case resp = <-ch:
		case <-timer.C:
			c.forget(id)
			return fmt.Errorf("ext: %s: timed out after %s", method, timeout)
		}
	} else {
		resp = <-ch
	}
	if resp.Error != nil {
		return fmt.Errorf("ext: %s: %s", method, resp.Error.Message)
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

// forget drops a pending call from the table.
func (c *rpcConn) forget(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// notify sends a notification (no response expected).
func (c *rpcConn) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("ext: rpc connection closed")
	}
	payload := Protocol{Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		payload.Params = b
	}
	return c.enc.Encode(payload)
}

// readLoop dispatches incoming messages until the stream ends.
func (c *rpcConn) readLoop(onNotification func(Protocol)) {
	for {
		var msg Protocol
		if err := c.dec.Decode(&msg); err != nil {
			c.failAll(err)
			return
		}
		if msg.ID == 0 {
			// A notification from the extension.
			if onNotification != nil {
				onNotification(msg)
			}
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ok {
			ch <- rpcResponse{Result: msg.Result, Error: msg.Error}
		}
	}
}

// failAll wakes every waiting call with the error.
func (c *rpcConn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- rpcResponse{Error: &RPCError{Message: err.Error()}}
		delete(c.pending, id)
	}
}

// close shuts the connection down.
func (c *rpcConn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	if c.closer != nil {
		c.closer.Close()
	}
}
