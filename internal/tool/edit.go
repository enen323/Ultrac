package tool

// The edit tool, ported from pi's
// packages/coding-agent/src/core/tools/edit.ts (221 lines).
//
// # Shape
//
// One call carries a batch of non-overlapping targeted replacements, each
// matched against the ORIGINAL file (not incrementally). The batch shape is
// pi's: "If two changes touch the same block or nearby lines, merge them
// into one edit instead" is in the tool's own description because
// overlapping edits are rejected, not silently serialized.
//
// # The shim
//
// PrepareArguments exists because models bend the schema: some send edits
// as a JSON string, some send a single edit object instead of a one-element
// array, and older prompts send the flat oldText/newText pair (pi's
// prepareEditArguments, edit.ts:103-134). With a shim installed, the agent
// runtime validates the shim's output rather than the raw arguments (P0
// decision), so the shim owns the final shape and must be strict.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// EditPair is one replacement inside edits[] (edit.ts:21-30).
type EditPair struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// EditInput mirrors pi's editSchema (edit.ts:32-41).
type EditInput struct {
	Path  string     `json:"path"`
	Edits []EditPair `json:"edits"`
}

const editSchema = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to edit (relative or absolute)"
    },
    "edits": {
      "type": "array",
      "description": "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.",
      "items": {
        "type": "object",
        "properties": {
          "oldText": {
            "type": "string",
            "description": "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."
          },
          "newText": {
            "type": "string",
            "description": "Replacement text for this targeted edit."
          }
        },
        "required": ["oldText", "newText"],
        "additionalProperties": false
      }
    }
  },
  "required": ["path", "edits"],
  "additionalProperties": false
}`

// EditDetails is the tool's Details payload (edit.ts:70-77): a display diff,
// a standard unified patch, and the first changed line for editor
// navigation.
type EditDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	FirstChangedLine *int   `json:"firstChangedLine,omitempty"`
}

// NewEditTool builds the edit tool for cwd.
func NewEditTool(cwd string) *agent.AgentTool[EditInput] {
	return &agent.AgentTool[EditInput]{
		Tool: ai.Tool{
			Name:        "edit",
			Description: "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
			Parameters:  json.RawMessage(editSchema),
			// pi: edit.ts:156.
			ConstrainedSampling: jsonSchemaPrefer(),
		},
		Label: "edit",
		// Port of prepareEditArguments (edit.ts:103-134).
		PrepareArguments: prepareEditArguments,
		Execute: func(ctx context.Context, _ string, in EditInput, _ func(agent.Result)) (agent.Result, error) {
			return executeEdit(ctx, cwd, in), nil
		},
	}
}

func init() {
	Register("edit", func(cwd string) agent.Tool { return agent.Erase(NewEditTool(cwd)) })
}

// prepareEditArguments normalizes the shapes models actually send into the
// schema's shape: edits as a JSON string, edits as a single object, and the
// legacy flat oldText/newText pair.
func prepareEditArguments(raw map[string]any) (EditInput, error) {
	var in EditInput
	if raw == nil {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return in, fmt.Errorf("Edit tool input is invalid. edits must contain at least one replacement.")
	}

	if p, ok := raw["path"]; ok {
		s, isString := p.(string)
		if !isString {
			//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
			return in, fmt.Errorf("Edit tool input is invalid. path must be a string.")
		}
		in.Path = s
	}

	edits, err := normalizeEditsField(raw["edits"])
	if err != nil {
		return in, err
	}

	// Legacy flat form: oldText/newText at the top level append to edits.
	if old, ok := stringField(raw, "oldText"); ok {
		newText, hasNew := stringField(raw, "newText")
		if !hasNew {
			//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
			return in, fmt.Errorf("Edit tool input is invalid. newText must be a string when oldText is present.")
		}
		edits = append(edits, EditPair{OldText: old, NewText: newText})
	}
	in.Edits = edits
	return in, nil
}

// normalizeEditsField accepts an array of edit objects, a single edit
// object, or a JSON string containing either.
func normalizeEditsField(v any) ([]EditPair, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(t), &parsed); err != nil {
			// A string that is not JSON is not an edits array; leave it
			// for schema validation to reject with a clear message.
			return nil, nil
		}
		return normalizeEditsField(parsed)
	case []any:
		out := make([]EditPair, 0, len(t))
		for _, elem := range t {
			pair, err := asEditPair(elem)
			if err != nil {
				return nil, err
			}
			out = append(out, pair)
		}
		return out, nil
	case map[string]any:
		pair, err := asEditPair(t)
		if err != nil {
			return nil, err
		}
		return []EditPair{pair}, nil
	default:
		return nil, nil
	}
}

// asEditPair converts one decoded JSON object into an EditPair. Unlike pi —
// which passes unrecognized elements through and lets schema validation
// reject them — this must produce the typed shape the runtime validates, so
// it checks the element itself and fails with the same message the schema
// would have produced.
func asEditPair(v any) (EditPair, error) {
	m, ok := v.(map[string]any)
	if !ok {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return EditPair{}, fmt.Errorf("Edit tool input is invalid. each edits entry must be an object with oldText and newText strings.")
	}
	old, okOld := stringField(m, "oldText")
	newText, okNew := stringField(m, "newText")
	if !okOld || !okNew {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return EditPair{}, fmt.Errorf("Edit tool input is invalid. each edits entry must be an object with oldText and newText strings.")
	}
	return EditPair{OldText: old, NewText: newText}, nil
}

// stringField reads a string field, reporting presence separately from
// emptiness (an explicit "" is a value the schema accepts).
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, isString := v.(string)
	if !isString {
		return "", false
	}
	return s, true
}

// executeEdit runs the batch (edit.ts:159-213).
func executeEdit(ctx context.Context, cwd string, in EditInput) agent.Result {
	if err := ctx.Err(); err != nil {
		return errResult("Operation aborted")
	}
	// pi's validateEditInput (edit.ts:136-141): the schema cannot express
	// "at least one" for the shim's output, so it is checked here.
	if len(in.Edits) == 0 {
		return errResult("Edit tool input is invalid. edits must contain at least one replacement.")
	}
	path := resolveToCwd(in.Path, cwd)
	if path == "" {
		return errResult("edit: path is required")
	}

	edits := make([]Edit, len(in.Edits))
	for i, e := range in.Edits {
		edits[i] = Edit(e)
	}

	var finalContent string
	var details EditDetails
	err := withFileMutationQueue(path, func() error {
		if err := ctx.Err(); err != nil {
			return errAborted
		}
		// Existence and readability are checked up front so the error
		// names the file the model asked about (edit.ts:175-182).
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
				return fmt.Errorf("Could not edit file: %s. %v.", in.Path, err)
			}
			//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
			return fmt.Errorf("Could not edit file: %s. %v.", in.Path, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return errAborted
		}

		// Strip the BOM before matching — the model never includes an
		// invisible BOM in oldText — and normalize line endings so edits
		// authored with "\n" apply to CRLF files (edit.ts:190-194).
		bom, content := splitBom(string(raw))
		ending := detectLineEnding(content)
		normalized := normalizeToLF(content)
		applied, err := applyEditsToNormalizedContent(normalized, edits, in.Path)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return errAborted
		}
		finalContent = bom + restoreLineEndings(applied.NewContent, ending)

		// 0644 rather than the file's prior mode is NOT done here: the
		// write goes through the same path the model sees, and preserving
		// an executable bit matters for scripts (kept from the pre-port
		// builtin edit tool).
		info, statErr := os.Stat(path)
		mode := os.FileMode(0o644)
		if statErr == nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(path, []byte(finalContent), mode); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return errAborted
		}

		diffStr, firstChanged := generateDiffString(applied.BaseContent, applied.NewContent, 4)
		details = EditDetails{
			Diff:  diffStr,
			Patch: generateUnifiedPatch(in.Path, applied.BaseContent, applied.NewContent, 4),
		}
		if firstChanged > 0 {
			n := firstChanged
			details.FirstChangedLine = &n
		}
		return nil
	})
	if err != nil {
		if err == errAborted {
			return errResult("Operation aborted")
		}
		return errResult(err.Error())
	}

	res := textResult(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(in.Edits), in.Path))
	res.Details = details
	return res
}
