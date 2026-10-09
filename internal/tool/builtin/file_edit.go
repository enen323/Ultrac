package builtin

import (
	"context"
	"os"
	"strings"

	"ultrac/internal/contract/tool"
)

// EditInput mirrors FileEditTool's schema.
type EditInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll *bool  `json:"replace_all,omitempty"`
}

const editSchema = `{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "The absolute path to the file to modify"
    },
    "old_string": {
      "type": "string",
      "description": "The text to replace"
    },
    "new_string": {
      "type": "string",
      "description": "The text to replace it with (must be different from old_string)"
    },
    "replace_all": {
      "type": "boolean",
      "default": false,
      "description": "Replace all occurrences of old_string (default false)"
    }
  },
  "required": ["file_path", "old_string", "new_string"],
  "additionalProperties": false
}`

const editDescription = `Performs exact string replacements in files.

Usage:
- You must use your Read tool at least once in the conversation before editing. This tool will error if you attempt an edit without reading the file.
- When editing text from Read tool output, ensure you preserve the exact indentation (tabs/spaces) as it appears AFTER the line number prefix. The line number prefix format is: spaces + line number + tab. Everything after that tab is the actual file content to match. Never include any part of the line number prefix in the old_string or new_string.
- ALWAYS prefer editing existing files in the codebase. NEVER write new files unless explicitly required.
- Only use emojis if the user explicitly requests it. Avoid adding emojis to files unless asked.
- The edit will FAIL if old_string is not unique in the file. Either provide a larger string with more surrounding context to make it unique or use replace_all to change every instance of old_string.
- Use replace_all for replacing and renaming strings across the file. This parameter is useful if you want to rename a variable for instance.`

type editTool struct {
	spec[EditInput]
}

func (editTool) Call(_ context.Context, in EditInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	if strings.TrimSpace(in.FilePath) == "" {
		return errResult("edit: file_path is required"), nil
	}
	if in.OldString == in.NewString {
		return errResult("edit: old_string and new_string are identical; there is nothing to change"), nil
	}
	path := resolvePath(uc, in.FilePath)

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errResult("edit: %s does not exist", displayPath(path)), nil
		}
		return errResult("edit: %s: %v", displayPath(path), err), nil
	}
	text := string(raw)

	occurrences := strings.Count(text, in.OldString)
	if occurrences == 0 {
		return errResult(
			"edit: old_string was not found in %s. The file may have changed, or the text may not match exactly (check whitespace and indentation).",
			displayPath(path)), nil
	}

	replaceAll := in.ReplaceAll != nil && *in.ReplaceAll
	if occurrences > 1 && !replaceAll {
		return errResult(
			"edit: old_string appears %d times in %s. Add surrounding context to make it unique, or pass replace_all: true.",
			occurrences, displayPath(path)), nil
	}

	updated := text
	if replaceAll {
		updated = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(text, in.OldString, in.NewString, 1)
	}

	// Preserve the existing file's mode: a script that was executable must stay
	// executable, and os.WriteFile would not know that.
	info, statErr := os.Stat(path)
	mode := os.FileMode(0o644)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(updated), mode); err != nil {
		return errResult("edit: %s: %v", displayPath(path), err), nil
	}

	replaced := 1
	if replaceAll {
		replaced = occurrences
	}

	var b strings.Builder
	b.WriteString("Edited " + displayPath(path) + ": replaced " + itoa(replaced) +
		" occurrence(s)\n")
	if d := unifiedDiff(text, updated, 3); d != "" {
		b.WriteString("\n" + d)
	}

	return okResult(b.String(), map[string]any{
		"filePath":     path,
		"oldString":    in.OldString,
		"newString":    in.NewString,
		"replaceAll":   replaceAll,
		"occurrences":  replaced,
		"originalFile": text,
		"linesBefore":  lineCount(text),
		"linesAfter":   lineCount(updated),
		"userModified": false,
	}), nil
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[EditInput](editTool{spec: spec[EditInput]{
		Name_:       "Edit",
		Destructive: false,
		Hint:        "replace an exact string in a file",
		Desc:        editDescription,
		Schema:      editSchema,
		SearchRead:  tool.SearchRead{},
	}}))
}
