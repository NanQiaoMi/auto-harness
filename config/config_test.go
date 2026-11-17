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
