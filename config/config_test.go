package config

import (
	"encoding/json"
	"testing"
)

func TestContextConfigJSONAndMerge(t *testing.T) {
	raw := []byte(`{
		"model": "test-model",
		"context": {
			"window": 64000,
			"reserve_tokens": 256,
			"compact_at": 0.6,
			"keep_recent_tokens": 8000,
			"keep_recent_messages": 4,
			"summary_max_tokens": 1200,
			"summary_chunk_tokens": 6000,
			"artifact_threshold": 4096,
			"artifact_preview": 512,
			"artifact_dir": "/tmp/artifacts"
		}
	}`)
	var src Config
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatal(err)
	}
	if src.Context == nil {
		t.Fatal("context config was not decoded")
	}

	var dst Config
	merge(&dst, src)
	if dst.Model != "test-model" || dst.Context == nil {
		t.Fatalf("merged config = %+v", dst)
	}
	got := dst.Context
	if got.Window != 64000 ||
		got.ReserveTokens != 256 ||
		got.CompactAt != 0.6 ||
		got.KeepRecentTokens != 8000 ||
		got.KeepRecentMessages != 4 ||
		got.SummaryMaxTokens != 1200 ||
		got.SummaryChunkTokens != 6000 ||
		got.ArtifactThreshold != 4096 ||
		got.ArtifactPreview != 512 ||
		got.ArtifactDir != "/tmp/artifacts" {
		t.Fatalf("context config = %+v", got)
	}
}

func TestProviderEnvironmentFollowsFinalProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("OPENAI_BASE_URL", "https://openai.example/v1")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example")

	cfg := &Config{Provider: "openai"}
	cfg.ResolveProviderEnvironment()
	if cfg.APIKey != "openai-key" || cfg.BaseURL != "https://openai.example/v1" {
		t.Fatalf("openai environment = %q/%q", cfg.APIKey, cfg.BaseURL)
	}

	cfg = &Config{Provider: "anthropic"}
	cfg.ResolveProviderEnvironment()
	if cfg.APIKey != "anthropic-key" || cfg.BaseURL != "https://anthropic.example" {
		t.Fatalf("anthropic environment = %q/%q", cfg.APIKey, cfg.BaseURL)
	}

	cfg = &Config{
		Provider: "anthropic",
		APIKey:   "explicit-key",
		BaseURL:  "https://explicit.example",
	}
	cfg.ResolveProviderEnvironment()
	if cfg.APIKey != "explicit-key" || cfg.BaseURL != "https://explicit.example" {
		t.Fatalf("explicit values were replaced: %q/%q", cfg.APIKey, cfg.BaseURL)
	}
}
