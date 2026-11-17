// Package config loads harness configuration from defaults, an optional
// ~/.harness/config.json file, and environment variables (lowest to
// highest precedence for values the CLI also flags).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/NanQiaoMi/auto-harness/llm"
	"github.com/NanQiaoMi/auto-harness/mcp"
	"github.com/NanQiaoMi/auto-harness/perm"
)

// Config is the effective harness configuration.
type Config struct {
	// Provider: "openai" (OpenAI-compatible) or "anthropic".
	Provider string `json:"provider"`
	// Model ID passed to the provider.
	Model string `json:"model"`
	// BaseURL overrides the provider endpoint (gateways, local servers).
	BaseURL string `json:"base_url"`
	// APIKey overrides the environment-provided key when set.
	APIKey string `json:"api_key,omitempty"`
	// MaxTokens bounds one completion (provider default when 0).
	MaxTokens int `json:"max_tokens,omitempty"`
	// Context controls request-level context budgeting and automatic
	// compaction. Zero values use model metadata and safe defaults.
	Context *ContextConfig `json:"context,omitempty"`
	// MaxTurns caps agent loop iterations (default 25).
	MaxTurns int `json:"max_turns,omitempty"`
	// SystemPrompt replaces the built-in system prompt when non-empty.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// SessionDir stores session transcripts (default ~/.harness/sessions).
	SessionDir string `json:"session_dir,omitempty"`
	// Extensions lists extension names to activate (reserved; static
	// extensions are registered in code today).
	Extensions []string `json:"extensions,omitempty"`
	// ExtensionDirs are directories scanned for process extensions.
	// Each subdirectory may hold an executable that speaks the
	// extension RPC protocol over stdio (see ext.ProcessExtension).
	// Defaults to ~/.harness/extensions and ./.harness/extensions.
	ExtensionDirs []string `json:"extension_dirs,omitempty"`
	// ExtensionCommand is a single command started as a process
	// extension (e.g. "python3 my_ext.py").
	ExtensionCommand string `json:"extension_command,omitempty"`
	// MCPServers configures MCP servers, using the same shape as
	// mcp.json. Servers listed here are merged with the ones in
	// ~/.harness/mcp.json and ./.harness/mcp.json.
	MCPServers map[string]mcp.ServerConfig `json:"mcp_servers,omitempty"`
	// NoMCP disables MCP for this run.
	NoMCP bool `json:"no_mcp,omitempty"`
	// TrustProject trusts the project's local resources — its
	// extensions, MCP servers, permission rules, skills and prompt
	// templates — without asking. It is the "always trust" switch; the
	// per-project answers live in ~/.harness/trust.json.
	TrustProject bool `json:"trust_project,omitempty"`
	// Permissions configures the permission rules. The rules are
	// merged with the ones in ~/.harness/permissions.json and
	// ./.harness/permissions.json.
	Permissions *perm.Set `json:"permissions,omitempty"`
	// Sandbox enables the operating-system sandbox for shell commands.
	Sandbox bool `json:"sandbox,omitempty"`
	// SandboxWritePaths are the directories a sandboxed command may
	// write. Defaults to the working directory.
	SandboxWritePaths []string `json:"sandbox_write_paths,omitempty"`
	// SandboxAllowNetwork permits network access in the sandbox.
	SandboxAllowNetwork bool `json:"sandbox_allow_network,omitempty"`
	// NoPermissions disables the permission layer.
	NoPermissions bool `json:"no_permissions,omitempty"`
	// Providers configures additional provider endpoints, enabling
	// model switching across providers (/model).
	Providers []llm.ProviderConfig `json:"providers,omitempty"`
	// CatalogURL is an optional remote model catalog to sync at startup.
	CatalogURL string `json:"catalog_url,omitempty"`
	// Telemetry enables the telemetry collector for this run. It is off
	// by default: telemetry is local diagnostics, not something a run
	// starts on its own.
	Telemetry bool `json:"telemetry,omitempty"`
	// TelemetryPath is the JSONL file the spans are written to. It
	// defaults to ~/.harness/telemetry.jsonl.
	TelemetryPath string `json:"telemetry_path,omitempty"`
	// TelemetryFile turns the local JSONL file on. It is on whenever a
	// telemetry path is configured, and off otherwise.
	TelemetryFile *bool `json:"telemetry_file,omitempty"`
	// TelemetryEndpoint is an OTLP/HTTP collector to ship the
	// telemetry to, e.g. http://localhost:4318/v1/traces. The metrics
	// endpoint is derived from it.
	TelemetryEndpoint string `json:"telemetry_endpoint,omitempty"`
	// TelemetryHeaders are sent with every OTLP request, e.g. an
	// Authorization header.
	TelemetryHeaders map[string]string `json:"telemetry_headers,omitempty"`
	// TelemetryIntervalMs is how often the metrics go out, in
	// milliseconds. It defaults to 30000.
	TelemetryIntervalMs int `json:"telemetry_interval_ms,omitempty"`
	// TelemetryTimeoutMs bounds one OTLP request, in milliseconds. It
	// defaults to 10000.
	TelemetryTimeoutMs int `json:"telemetry_timeout_ms,omitempty"`
	// TelemetryPrometheusEndpoint is a Prometheus server's remote write
	// endpoint, e.g. http://localhost:9090/api/v1/write. The metrics go
	// there as Prometheus time series.
	TelemetryPrometheusEndpoint string `json:"telemetry_prometheus_endpoint,omitempty"`
	// TelemetryPrometheusHeaders are sent with every remote write
	// request, e.g. an Authorization header.
	TelemetryPrometheusHeaders map[string]string `json:"telemetry_prometheus_headers,omitempty"`
}

// ContextConfig configures the context window and compression pipeline.
type ContextConfig struct {
	// Window overrides the model catalog's context window.
	Window int `json:"window,omitempty"`
	// ReserveTokens is the safety margin for tokenizer differences.
	ReserveTokens int `json:"reserve_tokens,omitempty"`
	// CompactAt is the fraction of the prompt budget at which
	// automatic compaction starts. Default 0.75.
	CompactAt float64 `json:"compact_at,omitempty"`
	// KeepRecentTokens is the approximate recent transcript budget.
	KeepRecentTokens int `json:"keep_recent_tokens,omitempty"`
	// KeepRecentMessages is the minimum number of recent units kept.
	KeepRecentMessages int `json:"keep_recent_messages,omitempty"`
	// SummaryMaxTokens bounds one context-capsule completion.
	SummaryMaxTokens int `json:"summary_max_tokens,omitempty"`
	// SummaryChunkTokens bounds one summarizer input chunk.
	SummaryChunkTokens int `json:"summary_chunk_tokens,omitempty"`
	// ArtifactThreshold is the byte size at which tool output moves to
	// the artifact store.
	ArtifactThreshold int `json:"artifact_threshold,omitempty"`
	// ArtifactPreview is the number of bytes kept in the prompt.
	ArtifactPreview int `json:"artifact_preview,omitempty"`
	// ArtifactDir stores oversized tool output. Empty uses
	// <session_dir>/artifacts/<session-id>.
	ArtifactDir string `json:"artifact_dir,omitempty"`
}

// TelemetryInterval returns how often the metrics go out.
func (c *Config) TelemetryInterval() time.Duration {
	if c.TelemetryIntervalMs > 0 {
		return time.Duration(c.TelemetryIntervalMs) * time.Millisecond
	}
	return 30 * time.Second
}

// TelemetryTimeout returns the bound on one OTLP request.
func (c *Config) TelemetryTimeout() time.Duration {
	if c.TelemetryTimeoutMs > 0 {
		return time.Duration(c.TelemetryTimeoutMs) * time.Millisecond
	}
	return 10 * time.Second
}

// fileConfig mirrors Config for the on-disk JSON file.
type fileConfig = Config

// Dir returns the harness config directory (~/.harness).
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".harness"
	}
	return filepath.Join(home, ".harness")
}

// Load builds the effective config. Precedence: built-in defaults <
// config file < environment variables. CLI flags are applied by the
// caller on top of the returned value. When Provider is not set
// explicitly, it is inferred from the model ID ("claude*" -> anthropic,
// anything else -> openai-compatible).
func Load() (*Config, error) {
	cfg := &Config{
		Model: "claude-sonnet-5-5",
	}

	path := filepath.Join(Dir(), "config.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var fc fileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, fmt.Errorf("config: %s: %w", path, err)
		}
		merge(cfg, fc)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: %w", err)
	}

	if v := os.Getenv("HARNESS_PROVIDER"); v != "" {
		cfg.Provider = v
	}
	if v := os.Getenv("HARNESS_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("HARNESS_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("HARNESS_SESSION_DIR"); v != "" {
		cfg.SessionDir = v
	}

	if cfg.Provider == "" {
		cfg.Provider = llm.GuessProvider(cfg.Model)
	}
	if cfg.BaseURL == "" {
		if cfg.Provider == "anthropic" {
			cfg.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
		} else {
			cfg.BaseURL = os.Getenv("OPENAI_BASE_URL")
		}
	}
	if cfg.APIKey == "" {
		if cfg.Provider == "anthropic" {
			cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		} else {
			cfg.APIKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	return cfg, nil
}

func merge(dst *Config, src Config) {
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.BaseURL != "" {
		dst.BaseURL = src.BaseURL
	}
	if src.APIKey != "" {
		dst.APIKey = src.APIKey
	}
	if src.MaxTokens > 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if src.Context != nil {
		dst.Context = src.Context
	}
	if src.MaxTurns > 0 {
		dst.MaxTurns = src.MaxTurns
	}
	if src.SystemPrompt != "" {
		dst.SystemPrompt = src.SystemPrompt
	}
	if src.SessionDir != "" {
		dst.SessionDir = src.SessionDir
	}
	if len(src.Extensions) > 0 {
		dst.Extensions = src.Extensions
	}
	if len(src.ExtensionDirs) > 0 {
		dst.ExtensionDirs = src.ExtensionDirs
	}
	if src.ExtensionCommand != "" {
		dst.ExtensionCommand = src.ExtensionCommand
	}
	if len(src.MCPServers) > 0 {
		dst.MCPServers = src.MCPServers
	}
	if src.NoMCP {
		dst.NoMCP = true
	}
	if src.TrustProject {
		dst.TrustProject = true
	}
	if src.Permissions != nil {
		dst.Permissions = src.Permissions
	}
	if src.Sandbox {
		dst.Sandbox = true
	}
	if len(src.SandboxWritePaths) > 0 {
		dst.SandboxWritePaths = src.SandboxWritePaths
	}
	if src.SandboxAllowNetwork {
		dst.SandboxAllowNetwork = true
	}
	if src.NoPermissions {
		dst.NoPermissions = true
	}
	if len(src.Providers) > 0 {
		dst.Providers = src.Providers
	}
	if src.CatalogURL != "" {
		dst.CatalogURL = src.CatalogURL
	}
	if src.Telemetry {
		dst.Telemetry = true
	}
	if src.TelemetryPath != "" {
		dst.TelemetryPath = src.TelemetryPath
	}
	if src.TelemetryFile != nil {
		dst.TelemetryFile = src.TelemetryFile
	}
	if src.TelemetryEndpoint != "" {
		dst.TelemetryEndpoint = src.TelemetryEndpoint
	}
	if len(src.TelemetryHeaders) > 0 {
		dst.TelemetryHeaders = src.TelemetryHeaders
	}
	if src.TelemetryIntervalMs > 0 {
		dst.TelemetryIntervalMs = src.TelemetryIntervalMs
	}
	if src.TelemetryTimeoutMs > 0 {
		dst.TelemetryTimeoutMs = src.TelemetryTimeoutMs
	}
	if src.TelemetryPrometheusEndpoint != "" {
		dst.TelemetryPrometheusEndpoint = src.TelemetryPrometheusEndpoint
	}
	if len(src.TelemetryPrometheusHeaders) > 0 {
		dst.TelemetryPrometheusHeaders = src.TelemetryPrometheusHeaders
	}
}
