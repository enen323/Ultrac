package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"ultrac/internal/contract/tool"
)

// WriteInput mirrors FileWriteTool's schema.
type WriteInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

const writeSchema = `{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "The absolute path to the file to write (must be absolute, not relative)"
    },
    "content": {
      "type": "string",
      "description": "The content to write to the file"
    }
  },
  "required": ["file_path", "content"],
  "additionalProperties": false
}`

const writeDescription = `Writes a file to the local filesystem.

Usage:
- This tool will overwrite the existing file if there is one at the provided path.
- If this is an existing file, you MUST use the Read tool first to read the file's contents. This tool will fail if you did not read the file first.
- Prefer the Edit tool for modifying existing files — it only sends the diff. Use this tool to create new files or for complete rewrites.
- NEVER create documentation files (*.md) or README files unless explicitly requested by the User.
- Only use emojis if the user explicitly requests it.`

type writeTool struct {
	spec[WriteInput]
}

func (writeTool) Call(_ context.Context, in WriteInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	if strings.TrimSpace(in.FilePath) == "" {
		return errResult("write: file_path is required"), nil
	}
	path := resolvePath(uc, in.FilePath)

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return errResult("write: %s is a directory", displayPath(path)), nil
	}

	old, readErr := os.ReadFile(path)
	oldText := ""
	existed := readErr == nil
	if existed {
		oldText = string(old)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errResult("write: cannot create directory for %s: %v", displayPath(path), err), nil
	}
	// 0644 rather than 0600: these are the user's own source files, and a file
	// only the owner can read is a surprise in a shared checkout.
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return errResult("write: %s: %v", displayPath(path), err), nil
	}

	kind := "create"
	if existed {
		kind = "update"
	}
	oldLines := lineCount(oldText)
	newLines := lineCount(in.Content)

	var b strings.Builder
	b.WriteString("Wrote " + itoa(len(in.Content)) + " bytes to " + displayPath(path) + "\n")
	if existed {
		b.WriteString("Lines: " + itoa(oldLines) + " → " + itoa(newLines) + "\n")
	} else {
		b.WriteString("New file, " + itoa(newLines) + " lines\n")
	}
	if d := unifiedDiff(oldText, in.Content, 3); d != "" {
		b.WriteString("\n" + d)
	}

	return okResult(b.String(), map[string]any{
		"type":         kind,
		"filePath":     path,
		"bytesWritten": len(in.Content),
		"originalFile": nullableString(oldText, existed),
	}), nil
}

func nullableString(s string, present bool) any {
	if !present {
		return nil
	}
	return s
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[WriteInput](writeTool{spec: spec[WriteInput]{
		Name_: "Write",
		// Not Destructive, deliberately. The predicate marks operations a user
		// cannot review after the fact — delete, send, publish. Overwriting a
		// file is visible in a diff and is exactly what acceptEdits exists to
		// approve; marking it destructive would make acceptEdits a no-op.
		Destructive: false,
		ReadOnly:    false,
		Hint:        "create or overwrite a file",
		Desc:        writeDescription,
		Schema:      writeSchema,
		SearchRead:  tool.SearchRead{},
	}}))
}
