package tool

// The read tool, ported from pi's
// packages/coding-agent/src/core/tools/read.ts (225 lines).
//
// The continuation notices are the model's navigation for large files, so
// they are transcribed character for character from the TypeScript —
// "[Showing lines 1-2000 of 5432. Use offset=2001 to continue.]" — because
// a reworded notice is a broken contract with every prompt that has ever
// told the model how to page through a file.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// ReadInput mirrors pi's readSchema (read.ts:14-18).
type ReadInput struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
}

const readSchema = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to read (relative or absolute)"
    },
    "offset": {
      "type": "integer",
      "description": "Line number to start reading from (1-indexed)"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of lines to read"
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`

// readOutputSchema is pi's readOutputSchema (read.ts:33-36): the text for
// text files, or an image object for images.
const readOutputSchema = `{
  "anyOf": [
    { "type": "string" },
    {
      "type": "object",
      "properties": {
        "type": { "const": "image" },
        "data": { "type": "string" },
        "mimeType": { "type": "string" },
        "note": { "type": "string" }
      },
      "required": ["type", "data", "mimeType", "note"]
    }
  ]
}`

// ReadDetails carries the truncation accounting for logs and UI.
type ReadDetails struct {
	Truncation *TruncationResult `json:"truncation,omitempty"`
}

// NewReadTool builds the read tool for cwd.
func NewReadTool(cwd string) *agent.AgentTool[ReadInput] {
	return &agent.AgentTool[ReadInput]{
		Tool: ai.Tool{
			Name: "read",
			// pi: read.ts:96. DEFAULT_MAX_BYTES/1024 = 50 is computed in
			// the source string so the two limits cannot drift apart.
			Description: fmt.Sprintf(
				"Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
				defaultMaxLines, defaultMaxBytes/1024),
			Parameters:          json.RawMessage(readSchema),
			ConstrainedSampling: jsonSchemaPrefer(),
		},
		Label:        "read",
		OutputSchema: json.RawMessage(readOutputSchema),
		Execute: func(ctx context.Context, _ string, in ReadInput, _ func(agent.Result)) (agent.Result, error) {
			return executeRead(ctx, cwd, in), nil
		},
	}
}

func init() {
	Register("read", func(cwd string) agent.Tool { return agent.Erase(NewReadTool(cwd)) })
}

func executeRead(ctx context.Context, cwd string, in ReadInput) agent.Result {
	if err := ctx.Err(); err != nil {
		return errResult("Operation aborted")
	}
	path := resolveToCwd(in.Path, cwd)
	if path == "" {
		return errResult("read: path is required")
	}

	// pi checks readability up front (ops.access, R_OK) and lets the
	// error propagate to the model verbatim.
	info, err := os.Stat(path)
	if err != nil {
		return errResult(err.Error())
	}
	if err := ctx.Err(); err != nil {
		return errResult("Operation aborted")
	}

	// Sniff the leading bytes to tell images from other binaries. pi
	// detects image mime types and treats everything else as UTF-8 text;
	// this port keeps the pre-port engine's honest guard for non-image
	// binaries instead of pushing mojibake into the transcript
	// (documented deviation: pi would return the decoded bytes as text).
	head, _ := peekFile(path, 8192)
	if mime := sniffImageMime(head); mime != "" {
		return readImageResult(path, mime)
	}
	if isBinaryHead(head) && info.Size() > 0 {
		return errResult(fmt.Sprintf(
			"read: %s looks like binary data (%s, %s) and is not text. Use Bash with a tool that handles it.",
			toSlash(path), sniffKind(head), formatSize(int(info.Size()))))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return errResult(err.Error())
	}
	if err := ctx.Err(); err != nil {
		return errResult("Operation aborted")
	}

	// --- pi read.ts:155-205, transcribed -----------------------------------
	text := string(raw)
	allLines := strings.Split(text, "\n")
	totalFileLines := len(allLines)

	startLine := 0
	if in.Offset != nil && *in.Offset > 0 {
		startLine = *in.Offset - 1
	}
	startLineDisplay := startLine + 1
	if startLine >= len(allLines) {
		offset := 0
		if in.Offset != nil {
			offset = *in.Offset
		}
		return errResult(fmt.Sprintf("Offset %d is beyond end of file (%d lines total)", offset, totalFileLines))
	}

	var selected string
	userLimited := false
	if in.Limit != nil {
		end := startLine + *in.Limit
		if end > len(allLines) {
			end = len(allLines)
		}
		selected = strings.Join(allLines[startLine:end], "\n")
		userLimited = true
	} else {
		selected = strings.Join(allLines[startLine:], "\n")
	}

	tr := truncateHead(selected, TruncationOptions{})
	var outputText string
	switch {
	case tr.FirstLineExceeds:
		// First line alone exceeds the byte limit: point at a bash
		// fallback rather than showing an empty result.
		firstLineSize := formatSize(len(allLines[startLine]))
		outputText = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startLineDisplay, firstLineSize, formatSize(defaultMaxBytes), startLineDisplay, in.Path, defaultMaxBytes)
	case tr.Truncated:
		endLineDisplay := startLineDisplay + tr.OutputLines - 1
		nextOffset := endLineDisplay + 1
		outputText = tr.Content
		if tr.TruncatedBy == "lines" {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]",
				startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
		} else {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]",
				startLineDisplay, endLineDisplay, totalFileLines, formatSize(defaultMaxBytes), nextOffset)
		}
	case userLimited && startLine+tr.OutputLines < len(allLines):
		remaining := len(allLines) - (startLine + tr.OutputLines)
		nextOffset := startLine + tr.OutputLines + 1
		outputText = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]",
			tr.Content, remaining, nextOffset)
	default:
		outputText = tr.Content
	}

	// Deviation from pi, for provider wire-validity: pi returns the empty
	// string for an empty file, but an empty text block can be rejected by
	// the API. Claude Code's own Read tool emits a note here too.
	if outputText == "" {
		outputText = "File exists but is empty (0 bytes)."
	}

	res := agent.Result{Content: []ai.ContentBlock{ai.TextBlock(outputText)}}
	if tr.Truncated {
		res.Details = ReadDetails{Truncation: &tr}
	}
	res.StructuredContent = outputText
	return res
}

// readImageResult returns the image as an attachment.
//
// pi resizes images to the model's input limits before attaching them
// (processImage, read.ts:139). The harness port carries no model image
// limits in the tool call path, so the bytes are attached as-is
// (documented deviation).
func readImageResult(path, mime string) agent.Result {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errResult(err.Error())
	}
	note := fmt.Sprintf("Read image file [%s]", mime)
	return agent.Result{
		Content: []ai.ContentBlock{
			ai.TextBlock(note),
			ai.ImageBlock(base64Std(raw), mime),
		},
		StructuredContent: map[string]any{
			"type": "image", "data": base64Std(raw), "mimeType": mime, "note": note,
		},
	}
}

// sniffImageMime reports the mime type when head is a supported image
// header (jpg, png, gif, webp, bmp — the types read's description names),
// and "" otherwise.
func sniffImageMime(head []byte) string {
	if len(head) >= 4 {
		switch {
		case head[0] == 0x89 && head[1] == 'P' && head[2] == 'N' && head[3] == 'G':
			return "image/png"
		case head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
			return "image/jpeg"
		case string(head[0:3]) == "GIF":
			return "image/gif"
		}
	}
	if len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		return "image/webp"
	}
	if len(head) >= 2 && head[0] == 'B' && head[1] == 'M' {
		return "image/bmp"
	}
	return ""
}

// peekFile reads up to n leading bytes, returning what it could read.
func peekFile(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, _ := f.Read(buf)
	return buf[:m], nil
}

// isBinaryHead applies the cheap NUL-byte heuristic to an already-read
// prefix. Ported from the pre-port builtin read tool: one stray non-UTF-8
// byte is not enough (a Latin-1 comment still reads as text), but a NUL in
// the first block is decisive.
func isBinaryHead(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	return strings.IndexByte(string(head), 0) >= 0
}

// sniffKind names a binary payload for the not-text error, from its magic
// bytes. Kept from the pre-port builtin read tool.
func sniffKind(head []byte) string {
	if mime := sniffImageMime(head); mime != "" {
		return mime
	}
	if len(head) >= 4 && string(head[0:4]) == "%PDF" {
		return "application/pdf"
	}
	return "application/octet-stream"
}
