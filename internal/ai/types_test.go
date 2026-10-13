package ai

// C1 comparison tests: the Go message types must produce exactly the JSON pi's
// TypeScript interfaces produce. Every expected literal below was transcribed
// from the pi source named in the comment above the case; the file:line
// references are the audit trail required by the plan (docs/pi-core-harness-
// plan.md §2.4: "不要靠'读起来一样'确认对齐").

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestMessageWireShape pins each role's JSON keys and the string-or-array
// content polymorphism of system/user messages.
//
// pi: packages/ai/src/types.ts:541-639.
func TestMessageWireShape(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want string
	}{{
		name: "system prompt is a bare string",
		// SystemMessage.content is `string | TextContent[]` (types.ts:544);
		// a lone plain text block collapses to the string form.
		msg:  Message{Role: RoleSystem, Content: []ContentBlock{TextBlock("You are pi.")}, TimestampMS: 0},
		want: `{"role":"system","content":"You are pi.","timestamp":0}`,
	}, {
		name: "system prompt with tools declared",
		// toolsAdded carries complete definitions (types.ts:552-554).
		msg: Message{
			Role:        RoleSystem,
			Content:     []ContentBlock{TextBlock("p")},
			ToolsAdded:  []Tool{{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)}},
			TimestampMS: 1,
		},
		want: `{"role":"system","content":"p","toolsAdded":[{"name":"read","description":"Read a file","parameters":{"type":"object"}}],"timestamp":1}`,
	}, {
		name: "system sections with a nil value removes a section",
		// sections maps name → replacement text, null removes (types.ts:551).
		msg: Message{
			Role:        RoleSystem,
			Content:     []ContentBlock{TextBlock("p")},
			Sections:    map[string]*string{"tools": ptr("new tools"), "rules": nil},
			TimestampMS: 2,
		},
		want: `{"role":"system","content":"p","sections":{"rules":null,"tools":"new tools"},"timestamp":2}`,
	}, {
		name: "user text message",
		msg:  Message{Role: RoleUser, Content: []ContentBlock{TextBlock("hello")}, TimestampMS: 2},
		want: `{"role":"user","content":"hello","timestamp":2}`,
	}, {
		name: "user mixed content stays an array",
		// ImageContent (types.ts:430-434).
		msg:  Message{Role: RoleUser, Content: []ContentBlock{TextBlock("look"), ImageBlock("AAAA", "image/png")}, TimestampMS: 3},
		want: `{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","data":"AAAA","mimeType":"image/png"}],"timestamp":3}`,
	}, {
		name: "assistant text plus tool call",
		// AssistantMessage.content is (TextContent | ThinkingContent | ToolCall)[],
		// always an array (types.ts:567).
		msg: Message{
			Role:        RoleAssistant,
			Content:     []ContentBlock{TextBlock("hi"), ToolCallBlock("tu_1", "read", map[string]any{"path": "a.go"})},
			API:         "anthropic-messages",
			Provider:    "anthropic",
			Model:       "claude-x",
			Usage:       &Usage{Input: 1, Output: 2, TotalTokens: 3},
			StopReason:  StopToolUse,
			TimestampMS: 4,
		},
		want: `{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"toolCall","id":"tu_1","name":"read","arguments":{"path":"a.go"}}],"api":"anthropic-messages","provider":"anthropic","model":"claude-x","usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":4}`,
	}, {
		name: "assistant thinking keeps its replay signature",
		// ThinkingContent.thinkingSignature must survive (types.ts:423).
		msg: Message{
			Role:        RoleAssistant,
			Content:     []ContentBlock{ThinkingBlock("reasoning", "sig-123", false), TextBlock("answer")},
			API:         "anthropic-messages",
			Provider:    "anthropic",
			Model:       "claude-x",
			Usage:       &Usage{Input: 1, Output: 1, TotalTokens: 2},
			StopReason:  StopStop,
			TimestampMS: 5,
		},
		want: `{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","thinkingSignature":"sig-123"},{"type":"text","text":"answer"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-x","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":5}`,
	}, {
		name: "tool result message",
		// ToolResultMessage (types.ts:620-637): isError is always present.
		msg:  NewToolResultMessage("tu_1", "read", "file contents", false),
		want: `{"role":"toolResult","toolCallId":"tu_1","toolName":"read","content":[{"type":"text","text":"file contents"}],"isError":false,"timestamp":0}`,
	}, {
		name: "tool result error flag",
		msg:  Message{Role: RoleToolResult, ToolCallID: "tu_2", ToolName: "bash", Content: []ContentBlock{TextBlock("command failed")}, IsError: true, TimestampMS: 6},
		want: `{"role":"toolResult","toolCallId":"tu_2","toolName":"bash","content":[{"type":"text","text":"command failed"}],"isError":true,"timestamp":6}`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("marshal mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
			// Round-trip: pi's JSON must decode back into an equal value.
			var back Message
			if err := json.Unmarshal([]byte(tc.want), &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if diff := cmp.Diff(tc.msg, back); diff != "" {
				t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// TestUsageWireShape pins Usage's seven counters and the cost object.
//
// pi: packages/ai/src/types.ts:446-467.
func TestUsageWireShape(t *testing.T) {
	u := Usage{
		Input: 10, Output: 20, CacheRead: 5, CacheWrite: 7,
		CacheWrite1h: ptrInt(3), Reasoning: ptrInt(4),
		TotalTokens: 42,
		Cost:        Cost{Input: 0.1, Output: 0.2, CacheRead: 0.01, CacheWrite: 0.02, Total: 0.33},
	}
	got, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"input":10,"output":20,"cacheRead":5,"cacheWrite":7,"cacheWrite1h":3,"reasoning":4,"totalTokens":42,"cost":{"input":0.1,"output":0.2,"cacheRead":0.01,"cacheWrite":0.02,"total":0.33}}`
	if string(got) != want {
		t.Errorf("marshal mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func ptrInt(n int) *int { return &n }

// TestStopReasonValues pins the StopReason enum, including "deferred", which
// the plan's draft struct list had omitted.
//
// pi: packages/ai/src/types.ts:469.
func TestStopReasonValues(t *testing.T) {
	got := []StopReason{
		StopPending, StopStop, StopLength, StopToolUse,
		StopError, StopAborted, StopDeferred,
	}
	want := []string{"pending", "stop", "length", "toolUse", "error", "aborted", "deferred"}
	if len(got) != len(want) {
		t.Fatalf("got %d stop reasons, want %d", len(got), len(want))
	}
	for i, g := range got {
		if string(g) != want[i] {
			t.Errorf("stop reason %d: got %q, want %q", i, g, want[i])
		}
	}
}

// TestToolWireShape pins Tool's JSON and the three-state
// constrainedSampling field.
//
// pi: packages/ai/src/types.ts:739-754.
func TestToolWireShape(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)

	plain, err := json.Marshal(Tool{Name: "read", Description: "Read a file", Parameters: schema})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}`
	if string(plain) != want {
		t.Errorf("plain tool mismatch:\n got: %s\nwant: %s", plain, want)
	}

	off, err := json.Marshal(Tool{Name: "x", Description: "d", Parameters: schema, ConstrainedSampling: &ConstrainedSampling{Disabled: true}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"name":"x","description":"d","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"constrainedSampling":false}`; string(off) != want {
		t.Errorf("disabled sampling mismatch:\n got: %s\nwant: %s", off, want)
	}

	jsonSchema, err := json.Marshal(Tool{Name: "x", Description: "d", Parameters: schema, ConstrainedSampling: &ConstrainedSampling{Kind: "json_schema", Strict: "require"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"name":"x","description":"d","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"constrainedSampling":{"type":"json_schema","strict":"require"}}`; string(jsonSchema) != want {
		t.Errorf("json_schema sampling mismatch:\n got: %s\nwant: %s", jsonSchema, want)
	}

	grammar, err := json.Marshal(Tool{Name: "x", Description: "d", Parameters: schema, ConstrainedSampling: &ConstrainedSampling{Kind: "grammar", Variants: map[string]string{"openai_lark": "start: \"x\""}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"name":"x","description":"d","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"constrainedSampling":{"type":"grammar","variants":{"openai_lark":"start: \"x\""}}}`; string(grammar) != want {
		t.Errorf("grammar sampling mismatch:\n got: %s\nwant: %s", grammar, want)
	}
}

// TestNormalizeContextFoldsPromptAndTools pins the normalize contract: the
// prompt and tool declarations move into a leading system message, and an
// empty prompt with no tools leaves the transcript untouched.
//
// pi: packages/ai/src/utils/transcript.ts:10-34.
func TestNormalizeContextFoldsPromptAndTools(t *testing.T) {
	user := Message{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}, TimestampMS: 9}
	tc := NormalizeContext(Context{
		SystemPrompt: "You are pi.",
		Messages:     []Message{user},
		Tools:        []Tool{{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if len(tc.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(tc.Messages))
	}
	if tc.Messages[0].Role != RoleSystem {
		t.Fatalf("first message role = %q, want system", tc.Messages[0].Role)
	}
	if got := MessageText(tc.Messages[0]); got != "You are pi." {
		t.Errorf("leading system text = %q", got)
	}
	if len(tc.Messages[0].ToolsAdded) != 1 || tc.Messages[0].ToolsAdded[0].Name != "read" {
		t.Errorf("toolsAdded = %+v, want [read]", tc.Messages[0].ToolsAdded)
	}

	empty := NormalizeContext(Context{Messages: []Message{user}})
	if len(empty.Messages) != 1 || empty.Messages[0].Role != RoleUser {
		t.Errorf("empty prompt must not synthesize a system message, got %+v", empty.Messages)
	}

	// The replayed prompt and tools survive collapsing (providers without
	// mid-conversation system messages).
	collapsed := CollapseSystemMessages([]Message{
		{Role: RoleSystem, Content: []ContentBlock{TextBlock("base")}, TimestampMS: 1},
		user,
		{Role: RoleSystem, Content: []ContentBlock{TextBlock(" patch")}, ToolsRemoved: []ToolReference{{Name: "gone"}}},
	})
	if len(collapsed) != 2 {
		t.Fatalf("collapsed has %d messages, want 2", len(collapsed))
	}
	if got := GetCurrentSystemPrompt([]Message{
		{Role: RoleSystem, Content: []ContentBlock{TextBlock("base")}},
		{Role: RoleSystem, Content: []ContentBlock{TextBlock("patch")}},
	}); got != "base\n\npatch" {
		t.Errorf("replayed prompt = %q, want %q", got, "base\n\npatch")
	}
}

// TestMessageTextJoinsWithNewline pins contentText's separator: pi joins text
// blocks with "\n", not "".
//
// pi: packages/ai/src/utils/text.ts:6-12.
func TestMessageTextJoinsWithNewline(t *testing.T) {
	m := Message{Role: RoleUser, Content: []ContentBlock{TextBlock("a"), TextBlock("b")}}
	if got := MessageText(m); got != "a\nb" {
		t.Errorf("MessageText = %q, want %q", got, "a\nb")
	}
	// Non-text blocks are skipped, not joined as empty strings.
	m2 := Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock("x"), ToolCallBlock("tu_1", "read", nil), TextBlock("y")}}
	if got := MessageText(m2); got != "x\ny" {
		t.Errorf("MessageText = %q, want %q", got, "x\ny")
	}
}

// TestSystemMessageTextIncludesSections pins getSystemMessageText: content
// followed by non-nil sections, joined with blank lines.
//
// pi: packages/ai/src/utils/text.ts:14-21.
func TestSystemMessageTextIncludesSections(t *testing.T) {
	rules := "be brief"
	m := Message{
		Role:     RoleSystem,
		Content:  []ContentBlock{TextBlock("base")},
		Sections: map[string]*string{"rules": &rules, "gone": nil},
	}
	if got, want := SystemMessageText(m), "base\n\nbe brief"; got != want {
		t.Errorf("SystemMessageText = %q, want %q", got, want)
	}
}

// TestSystemPromptReplayOrder pins the replay semantics that P4's section
// diffing depends on: sections are patched by name, nil removes, and a
// re-declared section moves to the end of the order.
//
// pi: packages/ai/src/utils/transcript.ts:73-96 + text.ts:14-21.
func TestSystemPromptReplayOrder(t *testing.T) {
	a, b := "A", "B"
	messages := []Message{
		{Role: RoleSystem, Content: []ContentBlock{TextBlock("base")}, Sections: map[string]*string{"rules": &a, "tools": &b}, TimestampMS: 1},
		{Role: RoleSystem, Sections: map[string]*string{"rules": nil}},
		{Role: RoleSystem, Sections: map[string]*string{"rules": &b}},
	}
	msg, order, ok := replaySystemMessage(messages)
	if !ok {
		t.Fatal("replay returned not-ok")
	}
	// rules was removed then re-declared, so it lands after tools.
	if diff := cmp.Diff([]string{"tools", "rules"}, order); diff != "" {
		t.Errorf("section order mismatch (-want +got):\n%s", diff)
	}
	if _, present := msg.Sections["gone"]; present {
		t.Error("removed section must be absent")
	}
	// The rendered prompt carries content then sections in replay order.
	want := "base\n\nB\n\nB"
	if got := GetCurrentSystemPrompt(messages); got != want {
		t.Errorf("GetCurrentSystemPrompt = %q, want %q", got, want)
	}
}

// TestGetCurrentToolsOrder pins the tool resolution order across removals
// and re-additions (pi's Map semantics).
//
// pi: packages/ai/src/utils/transcript.ts:57-66.
func TestGetCurrentToolsOrder(t *testing.T) {
	mk := func(name string) Tool { return Tool{Name: name} }
	messages := []Message{
		{Role: RoleSystem, ToolsAdded: []Tool{mk("read"), mk("bash")}, TimestampMS: 1},
		{Role: RoleSystem, ToolsRemoved: []ToolReference{{Name: "read"}}},
		{Role: RoleSystem, ToolsAdded: []Tool{mk("read"), mk("grep")}},
	}
	got := GetCurrentTools(messages)
	if len(got) != 3 {
		t.Fatalf("got %d tools, want 3", len(got))
	}
	// read was removed then re-added, so it moves to the end — but grep is
	// added after it in the same message, so grep is last.
	if got[0].Name != "bash" || got[1].Name != "read" || got[2].Name != "grep" {
		t.Errorf("tool order = %s/%s/%s, want bash/read/grep", got[0].Name, got[1].Name, got[2].Name)
	}
	// A removal of an absent tool is a no-op, and the caller's slice must not
	// be mutated (the replay reuses it).
	order := []string{"a", "b"}
	if got := removeString(order, "zzz"); len(got) != 2 || order[0] != "a" || order[1] != "b" {
		t.Errorf("removeString corrupted the input: %v (input now %v)", got, order)
	}
}

// pi: packages/ai/src/utils/transcript.ts:150-167.
// TestGetToolStateChanges pins the diff used for the loop's tool-loading
// announcement.
//
// pi: packages/ai/src/utils/transcript.ts:150-167.
func TestGetToolStateChanges(t *testing.T) {
	prev := []Tool{{Name: "read", Description: "d", Parameters: json.RawMessage(`{}`)}, {Name: "gone", Description: "d", Parameters: json.RawMessage(`{}`)}}
	cur := []Tool{{Name: "read", Description: "d2", Parameters: json.RawMessage(`{}`)}, {Name: "new", Description: "d", Parameters: json.RawMessage(`{}`)}}
	added, removed := GetToolStateChanges(prev, cur)
	if len(added) != 2 || added[0].Name != "read" || added[1].Name != "new" {
		t.Errorf("added = %+v, want [read(new def), new]", added)
	}
	if len(removed) != 2 || removed[0].Name != "read" || removed[1].Name != "gone" {
		t.Errorf("removed = %+v, want [read, gone]", removed)
	}
}
