package agentprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/config"
)

func TestParseAndOverrideConfig(t *testing.T) {
	t.Setenv("PROFILE_URL", "https://example.test/v1")
	dir := t.TempDir()
	prompt := filepath.Join(dir, "system.md")
	if err := os.WriteFile(prompt, []byte("profile prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := `{
	  "schema_version": 1,
	  "name": "coding",
	  "model": "claude-sonnet-5-5",
	  "base_url": "${PROFILE_URL}",
	  "system_prompt_file": "system.md",
	  "max_turns": 9,
	  "max_tokens": 700,
	  "tools": ["bash", "read"],
	  "plugins": ["git-tools"],
	  "mcp_servers": [],
	  "context": {"compact_at": 0.6, "keep_recent_messages": 4},
	  "env": {"NODE_ENV": "test"}
	}`
	p, err := Parse([]byte(document), dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.SystemPrompt != "profile prompt" {
		t.Fatalf("system prompt = %q", p.SystemPrompt)
	}
	if p.MCPServers == nil || len(p.MCPServers) != 0 {
		t.Fatalf("empty mcp_servers must remain an explicit empty selection: %#v", p.MCPServers)
	}

	cfg := &config.Config{
		Provider: "openai",
		Model:    "old-model",
		Context:  &config.ContextConfig{Window: 32000, CompactAt: 0.8},
	}
	resolved := &Resolved{Profile: p}
	resolved.ApplyToConfig(cfg)
	if cfg.Model != "claude-sonnet-5-5" || cfg.Provider != "anthropic" {
		t.Fatalf("model/provider = %s/%s", cfg.Model, cfg.Provider)
	}
	if cfg.BaseURL != "https://example.test/v1" || cfg.SystemPrompt != "profile prompt" {
		t.Fatalf("base URL/system prompt = %q/%q", cfg.BaseURL, cfg.SystemPrompt)
	}
	if cfg.MaxTurns != 9 || cfg.MaxTokens != 700 {
		t.Fatalf("turn/token caps = %d/%d", cfg.MaxTurns, cfg.MaxTokens)
	}
	if cfg.Context.Window != 32000 || cfg.Context.CompactAt != 0.6 || cfg.Context.KeepRecentMessages != 4 {
		t.Fatalf("context = %#v", cfg.Context)
	}
}

func TestParseRejectsUnknownFieldsAndBadValues(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown", `{"schema_version":1,"name":"x","toolz":[]}`, "unknown field"},
		{"schema", `{"schema_version":2,"name":"x"}`, "schema_version"},
		{"name", `{"schema_version":1,"name":"Bad Name"}`, "name"},
		{"both prompts", `{"schema_version":1,"name":"x","system_prompt":"a","system_prompt_file":"b"}`, "either"},
		{"negative", `{"schema_version":1,"name":"x","max_turns":-1}`, "negative"},
		{"compact", `{"schema_version":1,"name":"x","context":{"compact_at":1.5}}`, "between 0 and 1"},
		{"duplicate", `{"schema_version":1,"name":"x","tools":["bash","bash"]}`, "more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestResolveUntrustedProjectProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	dir := ProjectDir(project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coding.json"), []byte(`{"schema_version":1,"name":"coding"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("coding", project, false); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("Resolve error = %v", err)
	}
	resolved, err := Resolve("coding", project, true)
	if err != nil {
		t.Fatalf("trusted Resolve: %v", err)
	}
	if resolved.Scope != ScopeProject {
		t.Fatalf("scope = %s", resolved.Scope)
	}
}

func TestProfileMissingEnvFails(t *testing.T) {
	_, err := Parse([]byte(`{"schema_version":1,"name":"x","env":{"TOKEN":"${MISSING_FOR_TEST}"}}`), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "MISSING_FOR_TEST") {
		t.Fatalf("error = %v", err)
	}
}
