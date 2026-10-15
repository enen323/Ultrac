package tool

// The write tool, ported from pi's
// packages/coding-agent/src/core/tools/write.ts (98 lines).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// WriteInput mirrors pi's writeSchema (write.ts:11-14).
type WriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

const writeSchema = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to write (relative or absolute)"
    },
    "content": {
      "type": "string",
      "description": "Content to write to the file"
    }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`

// NewWriteTool builds the write tool for cwd.
func NewWriteTool(cwd string) *agent.AgentTool[WriteInput] {
	return &agent.AgentTool[WriteInput]{
		Tool: ai.Tool{
			Name:        "write",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
			Parameters:  json.RawMessage(writeSchema),
			// pi: write.ts:57.
			ConstrainedSampling: jsonSchemaPrefer(),
		},
		Label: "write",
		Execute: func(ctx context.Context, _ string, in WriteInput, _ func(agent.Result)) (agent.Result, error) {
			return executeWrite(ctx, cwd, in), nil
		},
	}
}

func init() {
	Register("write", func(cwd string) agent.Tool { return agent.Erase(NewWriteTool(cwd)) })
}

func executeWrite(ctx context.Context, cwd string, in WriteInput) agent.Result {
	if in.Path == "" {
		return errResult("write: path is required")
	}
	path := resolveToCwd(in.Path, cwd)
	if path == "" {
		return errResult("write: path is required")
	}

	err := withFileMutationQueue(path, func() error {
		// Abort is checked after each step rather than from a listener:
		// rejecting mid-queue would release the mutation lock while the
		// filesystem operation may still be in flight (pi write.ts:69-71).
		if err := ctx.Err(); err != nil {
			return errAborted
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return errAborted
		}
		// 0644, not 0600: these are the user's own source files, and a
		// file only the owner can read is a surprise in a shared checkout
		// (kept from the pre-port builtin write tool).
		return os.WriteFile(path, []byte(in.Content), 0o644)
	})
	if err != nil {
		if err == errAborted {
			return errResult("Operation aborted")
		}
		return errResult(err.Error())
	}

	// pi reports the path exactly as the model sent it (write.ts:86).
	return textResult(fmt.Sprintf("Successfully wrote to %s", in.Path))
}
