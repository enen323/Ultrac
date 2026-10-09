package builtin

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"ultrac/internal/contract/tool"
)

// ReadInput mirrors FileReadTool's input schema field for field.
//
// The names are the model's interface. `file_path` rather than `path` is not a
// style choice that can be tidied later: prompts, permission rules and every
// transcript on disk refer to it.
type ReadInput struct {
	FilePath string `json:"file_path"`
	Offset   *int   `json:"offset,omitempty"`
	Limit    *int   `json:"limit,omitempty"`
	Pages    string `json:"pages,omitempty"`
}

const readSchema = `{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "The absolute path to the file to read"
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "description": "The line number to start reading from. Only provide if the file is too large to read at once"
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "description": "The number of lines to read. Only provide if the file is too large to read at once."
    },
    "pages": {
      "type": "string",
      "description": "Page range for PDF files (e.g., \"1-5\", \"3\", \"10-20\"). Only applicable to PDF files. Maximum 20 pages per request."
    }
  },
  "required": ["file_path"],
  "additionalProperties": false
}`

const readDescription = `Reads a file from the local filesystem. You can access any file directly by using this tool.
Assume this tool is able to read all files on the machine. If the User provides a path to a file assume that path is valid. It is okay to read a file that does not exist; an error will be returned.

Usage:
- The file_path parameter must be an absolute path, not a relative path
- By default, it reads up to 2000 lines starting from the beginning of the file
- You can optionally specify a line offset and limit (especially handy for long files), but it's recommended to read the whole file by not providing these parameters
- Results are returned using cat -n format, with line numbers starting at 1
- This tool can read images (eg PNG, JPG, etc) and PDFs, but this Go engine returns their bytes as base64 text alongside a note rather than a rendered image
- This tool can only read files, not directories. To read a directory, use an ls command via the Bash tool.
- If you read a file that exists but has empty contents you will receive a note in place of contents.`

// maxReadBytes caps the file a single Read will pull in.
//
// The TypeScript limit is 256KB and it gates the TOTAL file size, not the
// requested slice. That is deliberate there and preserved here: a 2GB file with
// a 10-line window still gets refused, because serving it means opening and
// scanning something the session has no business holding.
const maxReadBytes = 256 * 1024

type readTool struct {
	spec[ReadInput]
}

func (readTool) Call(_ context.Context, in ReadInput, uc *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	if strings.TrimSpace(in.FilePath) == "" {
		return errResult("read: file_path is required"), nil
	}
	path := resolvePath(uc, in.FilePath)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errResult("read: %s does not exist", displayPath(path)), nil
		}
		return errResult("read: %s: %v", displayPath(path), err), nil
	}
	if info.IsDir() {
		// Listing the directory is more useful than refusing, and it is what
		// the model was about to ask Bash for anyway.
		entries, lerr := os.ReadDir(path)
		if lerr != nil {
			return errResult("read: %s is a directory and could not be listed: %v", displayPath(path), lerr), nil
		}
		var b strings.Builder
		b.WriteString(displayPath(path) + " is a directory. Its entries:\n")
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(name + "\n")
		}
		return okResult(b.String(), map[string]any{
			"path": path, "isDirectory": true, "count": len(entries),
		}), nil
	}
	if info.Size() > maxReadBytes {
		return errResult(
			"read: %s is %s, which is over the %s limit; use Grep to search it, or Bash with head/tail to read a slice",
			displayPath(path), humanBytes(info.Size()), humanBytes(maxReadBytes)), nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return errResult("read: %s: %v", displayPath(path), err), nil
	}

	if isBinary(raw) {
		return readBinaryResult(path, raw)
	}

	text := string(raw)
	total := lineCount(text)
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}

	lines := splitLines(text)
	// The empty case comes first. Checking the offset before it would report
	// "offset 0 is past the end" for a file that simply has nothing in it,
	// which sends the model looking for a bug that is not there.
	if len(lines) == 0 {
		return okResult(
			"read: "+displayPath(path)+" exists but is empty (0 bytes). It may be an empty file that was just created.",
			map[string]any{"path": path, "totalLines": 0, "empty": true}), nil
	}

	startLine := 1
	if in.Offset != nil && *in.Offset > 0 {
		startLine = *in.Offset + 1
	}
	limit := maxLinesDefault
	if in.Limit != nil && *in.Limit > 0 {
		limit = *in.Limit
	}

	if startLine > len(lines) {
		return errResult(
			"read: offset %d is past the end of %s, which has %d lines",
			startLine-1, displayPath(path), total), nil
	}
	end := startLine - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	window := strings.Join(lines[startLine-1:end], "\n")
	if len(lines) > 0 {
		window += "\n"
	}

	out := numberLines(window, startLine)
	truncated := end < len(lines)
	if truncated {
		out += "\n[showing lines " + itoa(startLine) + "-" + itoa(end) + " of " + itoa(total) +
			"; the file continues. Use offset to read on.]\n"
	}

	return okResult(out, map[string]any{
		"path":        path,
		"numLines":    end - startLine + 1,
		"startLine":   startLine,
		"totalLines":  total,
		"truncated":   truncated,
		"sizeBytes":   info.Size(),
		"isDirectory": false,
	}), nil
}

// readBinaryResult handles images and PDFs.
//
// The Go engine has no multimodal pipeline yet, so it does the honest thing:
// say what the file is, and hand back the bytes. Pretending to have read an
// image would be worse than admitting the limitation.
func readBinaryResult(path string, raw []byte) (*tool.Result, error) {
	kind := sniffKind(raw)
	if !strings.HasPrefix(kind, "image/") && kind != "application/pdf" {
		return errResult(
			"read: %s looks like binary data (%s, %s) and is not text. Use Bash with a tool that handles it.",
			displayPath(path), kind, humanBytes(int64(len(raw)))), nil
	}
	// Base64 rather than raw: a tool result is a string, and raw bytes would
	// corrupt the transcript.
	note := "read: " + displayPath(path) + " is a " + kind + " (" + humanBytes(int64(len(raw))) + "). " +
		"This engine does not render media inline; the bytes follow as base64.\n\n"
	return okResult(note+base64Encode(raw), map[string]any{
		"path": path, "mediaType": kind, "sizeBytes": len(raw), "binary": true,
	}), nil
}

func isBinary(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	probe := raw
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if strings.IndexByte(string(probe), 0) >= 0 {
		return true
	}
	// A high proportion of non-UTF8 bytes means it is not text. One stray byte
	// is not enough — a Latin-1 comment should still read as text.
	if utf8.Valid(probe) {
		return false
	}
	return true
}

func sniffKind(raw []byte) string {
	if len(raw) >= 4 {
		switch {
		case raw[0] == 0x89 && raw[1] == 'P' && raw[2] == 'N' && raw[3] == 'G':
			return "image/png"
		case raw[0] == 0xFF && raw[1] == 0xD8:
			return "image/jpeg"
		case string(raw[0:3]) == "GIF":
			return "image/gif"
		case string(raw[0:4]) == "%PDF":
			return "application/pdf"
		}
	}
	if len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "application/octet-stream"
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return itoa(int(n)) + " B"
	case n < 1024*1024:
		return itoa(int(n/1024)) + " KB"
	default:
		return itoa(int(n/(1024*1024))) + " MB"
	}
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[ReadInput](readTool{spec: spec[ReadInput]{
		Name_:           "Read",
		Hint:            "read a file from disk",
		Desc:            readDescription,
		Schema:          readSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
		SearchRead:      tool.SearchRead{Read: true},
	}}))
}
