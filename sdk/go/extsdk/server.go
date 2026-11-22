// Package extsdk provides a Go server helper for writing external
// auto-harness process extensions over stdio JSON-RPC.
package extsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/NanQiaoMi/auto-harness/ext"
)

// ToolHandler executes a registered extension tool.
type ToolHandler func(ctx context.Context, callID string, args json.RawMessage) ext.ToolResult

// CommandHandler executes a registered extension slash command.
type CommandHandler func(ctx context.Context, args string) (string, error)

// ConsultHandler answers an event.consult gate or rewrite request.
type ConsultHandler func(ctx context.Context, params ext.ConsultParams) ext.ConsultResult

type registeredTool struct {
	def     ext.ManifestTool
	handler ToolHandler
}

type registeredCommand struct {
	def     ext.ManifestCommand
	handler CommandHandler
}

// Server serves the auto-harness extension JSON-RPC protocol.
type Server struct {
	name        string
	description string

	mu       sync.RWMutex
	tools    map[string]registeredTool
	toolOrd  []string
	commands map[string]registeredCommand
	cmdOrd   []string
	consults map[string]ConsultHandler
	conOrd   []string

	outMu sync.Mutex
	out   io.Writer
}

// NewServer creates an extension server with the given name and summary.
func NewServer(name, description string) *Server {
	return &Server{
		name:        name,
		description: description,
		tools:       map[string]registeredTool{},
		commands:    map[string]registeredCommand{},
		consults:    map[string]ConsultHandler{},
	}
}

// RegisterTool registers a tool advertised in the manifest.
func (s *Server) RegisterTool(def ext.ManifestTool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[def.Name]; !exists {
		s.toolOrd = append(s.toolOrd, def.Name)
	}
	s.tools[def.Name] = registeredTool{def: def, handler: handler}
}

// RegisterCommand registers a slash command advertised in the manifest.
func (s *Server) RegisterCommand(def ext.ManifestCommand, handler CommandHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.commands[def.Name]; !exists {
		s.cmdOrd = append(s.cmdOrd, def.Name)
	}
	s.commands[def.Name] = registeredCommand{def: def, handler: handler}
}

// OnConsult registers a handler for an event.consult event type.
func (s *Server) OnConsult(eventType string, handler ConsultHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.consults[eventType]; !exists {
		s.conOrd = append(s.conOrd, eventType)
	}
	s.consults[eventType] = handler
}

// EmitEvent pushes an event notification to the harness.
func (s *Server) EmitEvent(event string, text string, data map[string]any) error {
	params, err := json.Marshal(ext.EventParams{
		Event: event,
		Text:  text,
		Data:  data,
	})
	if err != nil {
		return err
	}
	return s.writeRecord(ext.Protocol{
		SchemaVersion: ext.SchemaVersion,
		Method:        ext.MethodEvent,
		Params:        params,
	})
}

// ServeStdio runs the extension protocol over os.Stdin and os.Stdout.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// Serve processes JSON-RPC requests from in and writes responses to out
// until EOF, shutdown, or ctx cancellation.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.outMu.Lock()
	s.out = out
	s.outMu.Unlock()

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" {
			continue
		}
		var req ext.Protocol
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = s.writeError(0, -32700, "invalid JSON: "+err.Error())
			continue
		}
		if err := ext.ValidateSchemaVersion(req.SchemaVersion); err != nil {
			_ = s.writeError(req.ID, -32600, err.Error())
			continue
		}
		if req.Method == ext.MethodShutdown {
			return nil
		}
		resp := s.handle(ctx, req)
		if req.ID != 0 {
			if err := s.writeRecord(resp); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func (s *Server) handle(ctx context.Context, req ext.Protocol) ext.Protocol {
	switch req.Method {
	case ext.MethodManifest:
		return s.reply(req.ID, s.manifest())
	case ext.MethodToolExecute:
		var params ext.ExecParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errReply(req.ID, -32602, "invalid params: "+err.Error())
		}
		s.mu.RLock()
		t, ok := s.tools[params.Name]
		s.mu.RUnlock()
		if !ok || t.handler == nil {
			return s.reply(req.ID, ext.ToolResult{
				Content: fmt.Sprintf("unknown tool %q", params.Name),
				IsError: true,
			})
		}
		return s.reply(req.ID, t.handler(ctx, params.ToolCallID, params.Arguments))
	case ext.MethodCommandExecute:
		var params ext.CommandParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errReply(req.ID, -32602, "invalid params: "+err.Error())
		}
		s.mu.RLock()
		c, ok := s.commands[params.Name]
		s.mu.RUnlock()
		if !ok || c.handler == nil {
			return errReply(req.ID, -32601, fmt.Sprintf("unknown command %q", params.Name))
		}
		text, err := c.handler(ctx, params.Args)
		if err != nil {
			return errReply(req.ID, -32000, err.Error())
		}
		return s.reply(req.ID, map[string]string{"text": text})
	case ext.MethodConsult:
		var params ext.ConsultParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errReply(req.ID, -32602, "invalid params: "+err.Error())
		}
		s.mu.RLock()
		fn, ok := s.consults[params.Event]
		s.mu.RUnlock()
		if !ok || fn == nil {
			return s.reply(req.ID, ext.ConsultResult{})
		}
		return s.reply(req.ID, fn(ctx, params))
	default:
		return errReply(req.ID, -32601, fmt.Sprintf("unknown method %q", req.Method))
	}
}

func (s *Server) manifest() ext.Manifest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := ext.Manifest{
		SchemaVersion: ext.SchemaVersion,
		Name:          s.name,
		Description:   s.description,
	}
	for _, name := range s.toolOrd {
		m.Tools = append(m.Tools, s.tools[name].def)
	}
	for _, name := range s.cmdOrd {
		m.Commands = append(m.Commands, s.commands[name].def)
	}
	for _, ev := range s.conOrd {
		m.Consults = append(m.Consults, ev)
	}
	return m
}

func (s *Server) reply(id int, payload any) ext.Protocol {
	raw, err := json.Marshal(payload)
	if err != nil {
		return errReply(id, -32603, err.Error())
	}
	return ext.Protocol{
		SchemaVersion: ext.SchemaVersion,
		ID:            id,
		Result:        raw,
	}
}

func errReply(id, code int, msg string) ext.Protocol {
	return ext.Protocol{
		SchemaVersion: ext.SchemaVersion,
		ID:            id,
		Error:         &ext.RPCError{Code: code, Message: msg},
	}
}

func (s *Server) writeError(id, code int, msg string) error {
	return s.writeRecord(errReply(id, code, msg))
}

func (s *Server) writeRecord(rec ext.Protocol) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.out == nil {
		return fmt.Errorf("extsdk: output stream is not initialized")
	}
	if _, err := s.out.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}
