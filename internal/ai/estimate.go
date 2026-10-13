package ai

import (
	"encoding/json"
	"math"
)

// Context size estimation.
//
// Ported from pi's packages/ai/src/utils/estimate.ts. It exists so
// clampMaxTokensToContext (simple-options.ts:18-22) can leave room for the
// answer inside the model's window without asking the provider first: the
// estimate is deliberately crude (3.5 chars/token) and only ever shrinks the
// requested max_tokens, so being wrong is safe in one direction.

const (
	charsPerToken       = 3.5
	estimatedImageChars = 4800
	contextSafetyTokens = 4096
	minMaxTokens        = 1
)

// EstimateContextTokens estimates the token count of a transcript.
//
// The shortcut matters: when the most recent assistant message carries
// usage, its reported totals describe the whole prefix exactly, and only the
// messages after it need estimating. Estimating everything from characters
// after a real count is available would throw that accuracy away.
func EstimateContextTokens(messages []Message) int {
	usageIdx := -1
	var usageTokens int
	latestPrefix := int64(math.MinInt64)
	for i := range messages {
		m := messages[i]
		if m.Role == RoleAssistant {
			applies := m.TimestampMS >= latestPrefix
			if applies && m.StopReason != StopAborted && m.StopReason != StopError && m.Usage != nil {
				if t := usageTotal(m.Usage); t > 0 {
					usageTokens = t
					usageIdx = i
				}
			}
		}
		if m.TimestampMS > latestPrefix {
			latestPrefix = m.TimestampMS
		}
	}
	if usageIdx >= 0 {
		trailing := 0
		for i := usageIdx + 1; i < len(messages); i++ {
			trailing += estimateMessageTokens(messages[i])
		}
		return usageTokens + trailing
	}
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

func usageTotal(u *Usage) int {
	if u.TotalTokens != 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

func estimateMessageTokens(m Message) int {
	switch m.Role {
	case RoleSystem:
		chars := len(SystemMessageText(m))
		for _, t := range m.ToolsAdded {
			chars += len(t.Name) + len(t.Description) + len(t.Parameters)
		}
		return estimateTextTokens(chars)
	case RoleUser, RoleToolResult:
		chars := 0
		for _, b := range m.Content {
			if b.Type == BlockImage {
				chars += estimatedImageChars
			} else {
				chars += len(b.Text)
			}
		}
		return estimateTextTokens(chars)
	default: // assistant
		chars := 0
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				chars += len(b.Text)
			case BlockThinking:
				chars += len(b.Thinking)
			default:
				chars += len(b.Name)
				if raw, err := json.Marshal(b.Arguments); err == nil {
					chars += len(raw)
				}
			}
		}
		return estimateTextTokens(chars)
	}
}

func estimateTextTokens(chars int) int {
	return int(math.Ceil(float64(chars) / charsPerToken))
}

// ClampMaxTokensToContext caps maxTokens so the request leaves room for the
// answer inside the model's context window.
//
// pi: simple-options.ts:18-22. A model with no recorded window (0) is not
// clamped: guessing a window would risk rejecting requests the model accepts.
func ClampMaxTokensToContext(model Model, messages []Message, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return max(minMaxTokens, maxTokens)
	}
	available := model.ContextWindow - EstimateContextTokens(messages) - contextSafetyTokens
	return min(maxTokens, max(minMaxTokens, available))
}
