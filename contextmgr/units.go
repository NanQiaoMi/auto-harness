package contextmgr

import (
	"strings"

	"github.com/NanQiaoMi/auto-harness/llm"
)

const (
	summaryPrefix        = "【会话压缩摘要 — 此前对话已压缩，请据此继续协助】\n"
	automaticSummaryHead = "【上下文压缩摘要】\n"
)

type unit struct {
	msgs []llm.Message
}

func groupUnits(messages []llm.Message) []unit {
	var units []unit
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			group := []llm.Message{m}
			i++
			for i < len(messages) && messages[i].Role == llm.RoleTool {
				group = append(group, messages[i])
				i++
			}
			units = append(units, unit{msgs: group})
			continue
		}
		units = append(units, unit{msgs: []llm.Message{m}})
		i++
	}
	return units
}

func flattenUnits(units []unit) []llm.Message {
	var out []llm.Message
	for _, u := range units {
		out = append(out, u.msgs...)
	}
	return out
}

func lastUserUnit(units []unit) int {
	for i := len(units) - 1; i >= 0; i-- {
		for _, m := range units[i].msgs {
			if m.Role == llm.RoleUser {
				return i
			}
		}
	}
	return -1
}

func isSummaryMessage(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	return strings.HasPrefix(m.Content, summaryPrefix) ||
		strings.HasPrefix(m.Content, automaticSummaryHead)
}

func summaryMessage(text string) llm.Message {
	return SummaryMessage(text)
}

// SummaryMessage wraps a capsule in the prefix every entry point uses
// when it puts a compaction summary back into model context.
func SummaryMessage(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: SummaryPrefix + strings.TrimSpace(text)}
}

// SummaryPrefix identifies a compaction summary at the start of a
// user-role message.
const SummaryPrefix = summaryPrefix

// StripSummaryPrefix removes either known summary prefix.
func StripSummaryPrefix(s string) string {
	for _, prefix := range []string{summaryPrefix, automaticSummaryHead} {
		if strings.HasPrefix(s, prefix) {
			return strings.TrimPrefix(s, prefix)
		}
	}
	return s
}
