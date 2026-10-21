package compact

// The four prompts of the compaction engine, transcribed verbatim from pi.
//
// Verbatim is a requirement, not laziness: the summary text is data that
// survives in the session file and is re-read by both implementations, and a
// drifted instruction silently changes what a checkpoint contains. Each
// constant names its pi source.

// SummarizationPrompt builds a fresh structured checkpoint.
//
// pi: compaction.ts:507-538 (SUMMARIZATION_PROMPT).
const SummarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// updateSummarizationInstructions merges new information into an existing
// checkpoint.
//
// pi: compaction.ts:540-575 (UPDATE_SUMMARIZATION_INSTRUCTIONS). Not exported:
// pi keeps it private and only ever embeds it in UpdateSummarizationPrompt.
const updateSummarizationInstructions = `Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// UpdateSummarizationPrompt is the incremental-mode prompt: the new messages
// arrive above, the previous checkpoint inside <previous-summary> tags.
//
// pi: compaction.ts:577-579 (UPDATE_SUMMARIZATION_PROMPT).
const UpdateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

` + updateSummarizationInstructions

// TurnPrefixSummarizationPrompt checkpoints the prefix of a turn that had to
// be cut in half. The later messages of that turn are kept verbatim, so this
// summary must not try to reconstruct them.
//
// pi: compaction.ts:942-955 (TURN_PREFIX_SUMMARIZATION_PROMPT).
const TurnPrefixSummarizationPrompt = `The messages above are earlier context from an ongoing conversation. Later messages are stored separately and do not need to be reconstructed.

Create a concise checkpoint of the user's request and the progress shown above. This checkpoint will be placed before the later messages so the conversation can continue with the necessary context.

## Original Request
[What did the user ask for?]

## Progress So Far
- [Key decisions and work completed in these messages]

## Context Needed to Continue
- [Information from these messages needed to understand the later work]

Only summarize information explicitly present above. Do not infer or recreate later messages.`

// SummarizationSystemPrompt is the system prompt of the one-shot summary
// request. Its whole job is to stop the model from answering the
// conversation instead of summarizing it.
//
// pi: utils.ts:161-163 (SUMMARIZATION_SYSTEM_PROMPT).
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// splitTurnSeparator joins the history summary and the turn-prefix checkpoint
// of a split-turn compaction into one stored summary.
//
// pi: compaction.ts:1032.
const splitTurnSeparator = "\n\n---\n\n**Turn Context (split turn):**\n\n"

// noPriorHistory is the stand-in previous summary when a split turn has a
// turn prefix but no earlier history to summarize (and no previous
// compaction either).
//
// pi: compaction.ts:995 (`previousSummary ?? "No prior history."`).
const noPriorHistory = "No prior history."
