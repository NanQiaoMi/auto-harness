package contextmgr

import (
	"encoding/json"
	"math"
	"unicode/utf8"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// Estimator estimates how many tokens a request will occupy.
//
// The default implementation is deliberately conservative: ASCII text is
// counted at roughly four characters per token, while non-ASCII runes
// are counted as one token each. A safety multiplier covers the fact
// that providers do not share one tokenizer.
type Estimator interface {
	EstimateText(string) int
	EstimateMessage(llm.Message) int
	EstimateMessages([]llm.Message) int
	EstimateTools([]llm.ToolDefinition) int
}

// HeuristicEstimator is a dependency-free token estimate.
type HeuristicEstimator struct {
	// Safety scales the estimate. Values below one are clamped to one;
	// zero means the default 1.10.
	Safety float64
}

func (e HeuristicEstimator) safety() float64 {
	if e.Safety <= 0 {
		return 1.10
	}
	if e.Safety < 1 {
		return 1
	}
	return e.Safety
}

// EstimateText estimates a string's token count.
func (e HeuristicEstimator) EstimateText(s string) int {
	if s == "" {
		return 0
	}
	var units float64
	for _, r := range s {
		if r < utf8.RuneSelf {
			units += 0.25
		} else {
			units++
		}
	}
	return int(math.Ceil(units * e.safety()))
}

// EstimateMessage estimates one message, including its envelope and any
// tool-call arguments.
func (e HeuristicEstimator) EstimateMessage(m llm.Message) int {
	total := 6 + e.EstimateText(m.Content)
	for _, call := range m.ToolCalls {
		total += 8 + e.EstimateText(call.Name) + e.EstimateText(string(call.Arguments))
	}
	return total
}

// EstimateMessages estimates a message sequence.
func (e HeuristicEstimator) EstimateMessages(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += e.EstimateMessage(m)
	}
	return total
}

// EstimateTools estimates the JSON-schema cost of the tool definitions.
func (e HeuristicEstimator) EstimateTools(tools []llm.ToolDefinition) int {
	total := 0
	for _, tool := range tools {
		data, err := json.Marshal(tool)
		if err != nil {
			total += 32
			continue
		}
		total += 8 + e.EstimateText(string(data))
	}
	return total
}
