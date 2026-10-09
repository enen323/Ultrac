package builtin

import (
	"context"
	"strings"
	"sync"

	"ultrac/internal/contract/tool"
)

// AskOption is one choice offered to the user.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// AskQuestion is one question.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

// AskInput mirrors AskUserQuestionTool's schema.
type AskInput struct {
	Questions []AskQuestion `json:"questions"`
}

// AskAnswer is the user's reply to one question.
type AskAnswer struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// AskHandler collects answers. It is installed by a frontend that has a user
// to ask; nil means there is none.
//
// A package-level injection point rather than a field on UseContext: only this
// one tool needs it, and widening the context every tool receives for a single
// consumer would make every tool's signature lie a little.
type AskHandler func(ctx context.Context, questions []AskQuestion) ([]AskAnswer, error)

var (
	askMu      sync.RWMutex
	askHandler AskHandler
)

// SetAskHandler installs the interaction channel. Passing nil restores the
// unattended behaviour.
func SetAskHandler(h AskHandler) {
	askMu.Lock()
	defer askMu.Unlock()
	askHandler = h
}

func currentAskHandler() AskHandler {
	askMu.RLock()
	defer askMu.RUnlock()
	return askHandler
}

const askSchema = `{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "minItems": 1,
      "maxItems": 4,
      "description": "Questions to ask the user (1-4 questions)",
      "items": {
        "type": "object",
        "properties": {
          "question": {
            "type": "string",
            "description": "The complete question to ask the user. Should be clear, specific, and end with a question mark. Example: \"Which library should we use for date formatting?\" If multiSelect is true, phrase it accordingly, e.g. \"Which features do you want to enable?\""
          },
          "header": {
            "type": "string",
            "description": "Very short label displayed as a chip/tag (max 12 chars). Examples: \"Auth method\", \"Library\", \"Approach\"."
          },
          "options": {
            "type": "array",
            "minItems": 2,
            "maxItems": 4,
            "description": "The available choices for this question. Must have 2-4 options. Each option should be a distinct, mutually exclusive choice (unless multiSelect is enabled). There should be no 'Other' option, that will be provided automatically.",
            "items": {
              "type": "object",
              "properties": {
                "label": {
                  "type": "string",
                  "description": "The display text for this option that the user will see and select. Should be concise (1-5 words) and clearly describe the choice."
                },
                "description": {
                  "type": "string",
                  "description": "Explanation of what this option means or what will happen if chosen. Useful for providing context about trade-offs or implications."
                },
                "preview": {
                  "type": "string",
                  "description": "Optional preview content rendered when this option is focused. Use for mockups, code snippets, or visual comparisons that help users compare options."
                }
              },
              "required": ["label", "description"],
              "additionalProperties": false
            }
          },
          "multiSelect": {
            "type": "boolean",
            "default": false,
            "description": "Set to true to allow the user to select multiple options instead of just one. Use when choices are not mutually exclusive."
          }
        },
        "required": ["question", "header", "options"],
        "additionalProperties": false
      }
    }
  },
  "required": ["questions"],
  "additionalProperties": false
}`

const askDescription = `Use this tool when you need to ask the user questions during execution. This allows you to:
1. Gather user preferences or requirements
2. Clarify ambiguous instructions
3. Get decisions on implementation choices as you work
4. Offer choices to the user about what direction to take.

Usage notes:
- Users will always be able to select "Other" to provide custom text input
- Use multiSelect: true to allow multiple answers to be selected for a question
- If you recommend a specific option, make that the first option in the list and add "(Recommended)" at the end of the label

Plan mode note: In plan mode, use this tool to clarify requirements or choose between approaches BEFORE finalizing your plan. Do NOT use this tool to ask "Is my plan ready?" or "Should I proceed?" - use ExitPlanMode for plan approval.

IMPORTANT: this engine can only ask when a frontend is attached. A headless run
(--print) has nobody to answer, so the tool returns an error telling you to ask
the question in your response instead. Do not treat that as the user refusing.`

type askTool struct {
	spec[AskInput]
}

func (askTool) Call(ctx context.Context, in AskInput, _ *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	if len(in.Questions) == 0 {
		return errResult("ask: at least one question is required"), nil
	}
	if len(in.Questions) > 4 {
		return errResult("ask: at most 4 questions may be asked at once; got %d", len(in.Questions)), nil
	}
	seen := map[string]bool{}
	for i, q := range in.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return errResult("ask: questions[%d].question cannot be empty", i), nil
		}
		if seen[q.Question] {
			return errResult("ask: questions[%d] repeats a question that was already asked", i), nil
		}
		seen[q.Question] = true
		if len(q.Options) < 2 || len(q.Options) > 4 {
			return errResult("ask: questions[%d] must have 2-4 options; got %d", i, len(q.Options)), nil
		}
	}

	handler := currentAskHandler()
	if handler == nil {
		return errResult(
			"ask: there is no interactive frontend attached, so nobody can answer. " +
				"Put the question in your reply to the user instead, and state which " +
				"option you would choose if they do not respond."), nil
	}

	answers, err := handler(ctx, in.Questions)
	if err != nil {
		return errResult("ask: the user could not be asked: %v", err), nil
	}

	var b strings.Builder
	b.WriteString("User answered your questions:\n")
	for _, a := range answers {
		b.WriteString("- " + a.Question + " → " + a.Answer + "\n")
	}
	return okResult(b.String(), map[string]any{
		"questions": in.Questions,
		"answers":   answers,
	}), nil
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[AskInput](askTool{spec: spec[AskInput]{
		Name_:            "AskUserQuestion",
		Hint:             "ask the user a question",
		Desc:             askDescription,
		Schema:           askSchema,
		ReadOnly:         true,
		ConcurrencySafe:  false,
		NeedsInteraction: true,
		SearchRead:       tool.SearchRead{},
	}}))
}
