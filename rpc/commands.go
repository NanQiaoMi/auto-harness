package rpc

import (
	"encoding/json"
	"fmt"

	"github.com/NanQiaoMi/auto-harness/contextmgr"
)

// The command types the RPC mode understands.
//
// The names match pi's, so a client written against pi's protocol
// works here.
const (
	// CmdPrompt sends a user message to the agent.
	CmdPrompt = "prompt"
	// CmdAbort stops the current run.
	CmdAbort = "abort"
	// CmdNewSession starts a fresh session.
	CmdNewSession = "new_session"
	// CmdGetState reports the session's state.
	CmdGetState = "get_state"
	// CmdGetMessages returns the conversation history.
	CmdGetMessages = "get_messages"
	// CmdSetModel switches the model.
	CmdSetModel = "set_model"
	// CmdGetAvailableModels lists the known models.
	CmdGetAvailableModels = "get_available_models"
	// CmdSetThinking turns the thinking mode on or off.
	CmdSetThinking = "set_thinking"
	// CmdCompact compresses the conversation history.
	CmdCompact = "compact"
	// CmdBash runs a shell command.
	CmdBash = "bash"
	// CmdGetSessionStats reports the session's token accounting.
	CmdGetSessionStats = "get_session_stats"
	// CmdGetContext reports the context budget and last compaction.
	CmdGetContext = "get_context"
	// CmdGetEntries returns the session tree's entries.
	CmdGetEntries = "get_entries"
	// CmdGetTree returns the session tree.
	CmdGetTree = "get_tree"
	// CmdGetLastAssistantText returns the last assistant message.
	CmdGetLastAssistantText = "get_last_assistant_text"
	// CmdSetSessionName names the session.
	CmdSetSessionName = "set_session_name"
	// CmdGetCommands lists the slash commands the harness knows.
	CmdGetCommands = "get_commands"
	// CmdGetProfile reports the agent environment profile in effect.
	CmdGetProfile = "get_profile"
	// CmdGetPlugins reports the discovered plugin catalog and state.
	CmdGetPlugins = "get_plugins"
	// CmdFork branches the session at an entry.
	CmdFork = "fork"
	// CmdClone copies the current branch into a new session.
	CmdClone = "clone"
)

// PromptCommand sends a message to the agent.
type PromptCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

// SetModelCommand switches the model.
type SetModelCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Model   string `json:"model"`
	Provide string `json:"provider,omitempty"`
}

// SetThinkingCommand turns the thinking mode on or off.
type SetThinkingCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

// CompactCommand compresses the conversation history.
type CompactCommand struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"`
	Custom string `json:"customInstructions,omitempty"`
}

// BashCommand runs a shell command.
type BashCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Command string `json:"command"`
	// ExcludeFromContext keeps the output out of the model's context.
	ExcludeFromContext bool `json:"excludeFromContext,omitempty"`
}

// SetSessionNameCommand names the session.
type SetSessionNameCommand struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Name string `json:"name"`
}

// ForkCommand branches the session at an entry.
type ForkCommand struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	EntryID string `json:"entryId"`
}

// The state the get_state command reports.
type State struct {
	// Model is the model in use.
	Model string `json:"model"`
	// Provider is the provider serving the model.
	Provider string `json:"provider"`
	// Thinking reports whether the thinking mode is on.
	Thinking bool `json:"thinking"`
	// IsStreaming reports whether a run is in progress.
	IsStreaming bool `json:"isStreaming"`
	// MessageCount is the number of messages in the history.
	MessageCount int `json:"messageCount"`
	// SessionID identifies the session.
	SessionID string `json:"sessionId,omitempty"`
	// SessionFile is where the session is written.
	SessionFile string `json:"sessionFile,omitempty"`
	// SessionName is the session's display name.
	SessionName string `json:"sessionName,omitempty"`
}

// The models the get_available_models command reports.
type ModelsPayload struct {
	Models []ModelInfo `json:"models"`
}

// ModelInfo describes one model.
type ModelInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Protocol string `json:"protocol,omitempty"`
}

// The messages the get_messages command reports.
type MessagesPayload struct {
	Messages []Message `json:"messages"`
}

// Message is one conversation message.
//
// The harness's own message shape is kept: a client that reads the
// history does not have to know a second message format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// The stats the get_session_stats command reports.
type StatsPayload struct {
	Messages    int `json:"messages"`
	Turns       int `json:"turns"`
	InputTokens int `json:"inputTokens"`
	OutputToken int `json:"outputTokens"`
}

// ContextPayload reports the active context policy. Enabled is false
// when no context manager is installed.
type ContextPayload struct {
	Enabled bool               `json:"enabled"`
	Status  *contextmgr.Status `json:"status,omitempty"`
}

// The entries the get_entries command reports.
type EntriesPayload struct {
	Entries []Entry `json:"entries"`
	LeafID  string  `json:"leafId,omitempty"`
}

// Entry is one node of the session tree.
type Entry struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	Type     string `json:"type"`
	// Timestamp is the entry's creation time in milliseconds.
	Timestamp int64 `json:"timestamp,omitempty"`
}

// The tree the get_tree command reports.
type TreePayload struct {
	Entries []Entry `json:"entries"`
	LeafID  string  `json:"leafId,omitempty"`
}

// The text the get_last_assistant_text command reports.
type TextPayload struct {
	Text string `json:"text"`
}

// The commands the get_commands command reports.
type CommandsPayload struct {
	Commands []CommandInfo `json:"commands"`
}

// CommandInfo describes one slash command.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Source says where the command comes from: "builtin",
	// "extension", "prompt" or "skill".
	Source string `json:"source"`
}

// ProfilePayload reports the active agent environment. Environment
// values are represented by their keys only.
type ProfilePayload struct {
	Enabled        bool     `json:"enabled"`
	Name           string   `json:"name,omitempty"`
	Description    string   `json:"description,omitempty"`
	Source         string   `json:"source,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	Model          string   `json:"model"`
	Provider       string   `json:"provider,omitempty"`
	MaxTurns       int      `json:"maxTurns,omitempty"`
	MaxTokens      int      `json:"maxTokens,omitempty"`
	ActiveTools    []string `json:"activeTools"`
	AllTools       int      `json:"allTools"`
	Tools          []string `json:"tools"`
	ToolsAll       bool     `json:"toolsAll"`
	Plugins        []string `json:"plugins"`
	PluginsAll     bool     `json:"pluginsAll"`
	MCPServers     []string `json:"mcpServers"`
	MCPServersAll  bool     `json:"mcpServersAll"`
	Skills         []string `json:"skills"`
	SkillsAll      bool     `json:"skillsAll"`
	Prompts        []string `json:"prompts"`
	PromptsAll     bool     `json:"promptsAll"`
	PluginDirs     []string `json:"pluginDirs,omitempty"`
	SkillDirs      []string `json:"skillDirs,omitempty"`
	PromptDirs     []string `json:"promptDirs,omitempty"`
	EnvKeys        []string `json:"envKeys,omitempty"`
	PluginWarnings []string `json:"pluginWarnings,omitempty"`
}

// PluginsPayload reports every discovered process plugin.
type PluginsPayload struct {
	Plugins []PluginInfo `json:"plugins"`
}

// PluginInfo describes one manifest or legacy plugin.
type PluginInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Path        string `json:"path"`
	Selected    bool   `json:"selected"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
	Error       string `json:"error,omitempty"`
}

// The result the bash command reports.
type BashPayload struct {
	// Output is the command's combined output.
	Output string `json:"output"`
	// ExitCode is the command's exit status.
	ExitCode int `json:"exitCode"`
	// IsError reports a non-zero exit.
	IsError bool `json:"isError"`
}

// The result the fork command reports.
type ForkPayload struct {
	// SessionID identifies the new session.
	SessionID string `json:"sessionId,omitempty"`
	// SessionFile is where the new session is written.
	SessionFile string `json:"sessionFile,omitempty"`
	// Text is the message that was forked from.
	Text string `json:"text,omitempty"`
}

// The result the new_session command reports.
type NewSessionPayload struct {
	SessionID   string `json:"sessionId,omitempty"`
	SessionFile string `json:"sessionFile,omitempty"`
}

// decodeArgs decodes a command's arguments.
//
// A command whose arguments do not fit the expected shape is an error
// rather than a silently-empty struct: the client and the harness
// disagree, and that must be visible.
func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("rpc: the command arguments are wrong: %w", err)
	}
	return nil
}
