// Package rpcclient provides a strongly-typed Go client for driving an
// auto-harness subprocess over its JSONL RPC protocol.
package rpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/NanQiaoMi/auto-harness/rpc"
)

// PromptResult summarizes a completed prompt run up to agent_settled.
type PromptResult struct {
	Disposition string
	Text        string
	Aborted     bool
	Events      []rpc.Event
}

// Client drives an auto-harness RPC process over stdin/stdout JSONL.
type Client struct {
	in     io.WriteCloser
	reader *rpc.Reader
	cmd    *exec.Cmd

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpc.Response
	subs    map[int]chan rpc.Event
	nextSub int
	seq     atomic.Int64
	done    chan struct{}
	readErr error
	closed  bool
}

// Start spawns binPath with args (for example "-rpc") and connects a Client.
func Start(ctx context.Context, binPath string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, binPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("rpcclient: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rpcclient: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rpcclient: start %s: %w", binPath, err)
	}
	c := newClient(stdin, stdout, cmd)
	return c, nil
}

// New wraps an existing command/response pair (useful for in-memory pipes).
func New(in io.WriteCloser, out io.Reader) *Client {
	return newClient(in, out, nil)
}

func newClient(in io.WriteCloser, out io.Reader, cmd *exec.Cmd) *Client {
	c := &Client{
		in:      in,
		reader:  rpc.NewReader(out),
		cmd:     cmd,
		pending: map[string]chan rpc.Response{},
		subs:    map[int]chan rpc.Event{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		raw, err := c.reader.Read()
		if err != nil {
			c.mu.Lock()
			if err != io.EOF {
				c.readErr = err
			}
			pending := c.pending
			c.pending = map[string]chan rpc.Response{}
			subs := c.subs
			c.subs = map[int]chan rpc.Event{}
			c.mu.Unlock()
			for _, ch := range pending {
				close(ch)
			}
			for _, ch := range subs {
				close(ch)
			}
			return
		}

		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		if probe.Type == string(rpc.RecordResponse) {
			var resp rpc.Response
			if err := json.Unmarshal(raw, &resp); err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[resp.ID]
			if ok {
				delete(c.pending, resp.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- resp
				close(ch)
			}
			continue
		}

		var ev rpc.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		c.mu.Lock()
		for _, ch := range c.subs {
			select {
			case ch <- ev:
			default:
			}
		}
		c.mu.Unlock()
	}
}

// Call sends an arbitrary RPC command and waits for its correlated Response.
func (c *Client) Call(ctx context.Context, cmdType string, fields map[string]any) (rpc.Response, error) {
	id := fmt.Sprintf("req-%d", c.seq.Add(1))
	payload := map[string]any{
		"schema_version": rpc.SchemaVersion,
		"id":             id,
		"type":           cmdType,
	}
	for k, v := range fields {
		payload[k] = v
	}

	ch := make(chan rpc.Response, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return rpc.Response{}, fmt.Errorf("rpcclient: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	c.writeMu.Lock()
	err := rpc.WriteCommand(c.in, payload)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return rpc.Response{}, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return rpc.Response{}, fmt.Errorf("rpcclient: stream closed waiting for %s", cmdType)
		}
		if !resp.Success {
			return resp, fmt.Errorf("rpcclient: %s: %s", cmdType, resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return rpc.Response{}, ctx.Err()
	}
}

func (c *Client) subscribe() (int, chan rpc.Event) {
	ch := make(chan rpc.Event, 256)
	c.mu.Lock()
	c.nextSub++
	id := c.nextSub
	c.subs[id] = ch
	c.mu.Unlock()
	return id, ch
}

func (c *Client) unsubscribe(id int) {
	c.mu.Lock()
	ch, ok := c.subs[id]
	if ok {
		delete(c.subs, id)
		close(ch)
	}
	c.mu.Unlock()
}

// Prompt sends a prompt (with optional images) and blocks until agent_settled.
func (c *Client) Prompt(ctx context.Context, message string, images []rpc.ImageInput, onEvent func(rpc.Event)) (*PromptResult, error) {
	subID, evCh := c.subscribe()
	defer c.unsubscribe(subID)

	fields := map[string]any{"message": message}
	if len(images) > 0 {
		fields["images"] = images
	}
	resp, err := c.Call(ctx, rpc.CmdPrompt, fields)
	if err != nil {
		return nil, err
	}

	res := &PromptResult{}
	if data, ok := resp.Data.(map[string]any); ok {
		res.Disposition, _ = data["disposition"].(string)
	}

	var deltaText string
	var finalAssistant string
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				return res, fmt.Errorf("rpcclient: stream closed before agent_settled")
			}
			res.Events = append(res.Events, ev)
			if onEvent != nil {
				onEvent(ev)
			}
			switch ev.Type {
			case rpc.EventMessageUpdate:
				thinking, _ := ev.Data["thinking"].(bool)
				if !thinking {
					if t, _ := ev.Data["text"].(string); t != "" {
						deltaText += t
					}
				}
			case rpc.EventMessageEnd:
				if msg, ok := ev.Data["message"].(map[string]any); ok {
					if role, _ := msg["role"].(string); role == "assistant" {
						if content, _ := msg["content"].(string); content != "" {
							finalAssistant = content
						}
					}
				}
			case rpc.EventAgentSettled:
				if aborted, ok := ev.Data["aborted"].(bool); ok {
					res.Aborted = aborted
				}
				if finalAssistant != "" {
					res.Text = finalAssistant
				} else {
					res.Text = deltaText
				}
				return res, nil
			}
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
}

// GetState queries the current harness state.
func (c *Client) GetState(ctx context.Context) (rpc.State, error) {
	resp, err := c.Call(ctx, rpc.CmdGetState, nil)
	if err != nil {
		return rpc.State{}, err
	}
	var st rpc.State
	if err := decodeData(resp.Data, &st); err != nil {
		return rpc.State{}, err
	}
	return st, nil
}

// GetMessages queries the current conversation messages.
func (c *Client) GetMessages(ctx context.Context) ([]rpc.Message, error) {
	resp, err := c.Call(ctx, rpc.CmdGetMessages, nil)
	if err != nil {
		return nil, err
	}
	var payload rpc.MessagesPayload
	if err := decodeData(resp.Data, &payload); err != nil {
		return nil, err
	}
	return payload.Messages, nil
}

// Abort requests cancellation of the active run.
func (c *Client) Abort(ctx context.Context) error {
	_, err := c.Call(ctx, rpc.CmdAbort, nil)
	return err
}

// Close closes the stdin stream and waits for the reader and subprocess to exit.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	var err error
	if c.in != nil {
		err = c.in.Close()
	}
	<-c.done
	if c.cmd != nil {
		if waitErr := c.cmd.Wait(); waitErr != nil && err == nil {
			err = waitErr
		}
	}
	return err
}

func decodeData(data any, target any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
