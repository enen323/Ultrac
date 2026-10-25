package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// modelsSchema is the models.json schema, in the shape of pi's
// model-config.ts (ModelDefinitionSchema / ModelOverrideSchema /
// ProviderConfigSchema / ModelsConfigSchema) with the two documented
// deviations: `compat` and `authHeader` are absent (unknown fields are
// rejected with a path), and `oauth` is declared as an optional any-typed
// field so pi's files keep loading (plan D4).
//
// It is draft 2020-12, which is jsonschema/v6's default dialect.
const modelsSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Ultrac models.json",
  "type": "object",
  "required": ["providers"],
  "additionalProperties": false,
  "properties": {
    "$schema": { "type": "string" },
    "providers": {
      "type": "object",
      "additionalProperties": { "$ref": "#/$defs/provider" }
    }
  },
  "$defs": {
    "provider": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "name": { "type": "string", "minLength": 1 },
        "baseUrl": { "type": "string", "minLength": 1 },
        "apiKey": { "type": "string", "minLength": 1 },
        "api": { "type": "string", "minLength": 1 },
        "oauth": true,
        "headers": { "$ref": "#/$defs/stringMap" },
        "models": {
          "type": "array",
          "items": { "$ref": "#/$defs/model" }
        },
        "modelOverrides": {
          "type": "object",
          "additionalProperties": { "$ref": "#/$defs/modelOverride" }
        }
      }
    },
    "model": {
      "type": "object",
      "required": ["id"],
      "additionalProperties": false,
      "properties": {
        "id": { "type": "string", "minLength": 1 },
        "name": { "type": "string", "minLength": 1 },
        "api": { "type": "string", "minLength": 1 },
        "baseUrl": { "type": "string", "minLength": 1 },
        "reasoning": { "type": "boolean" },
        "thinkingLevelMap": { "$ref": "#/$defs/thinkingLevelMap" },
        "input": {
          "type": "array",
          "items": { "enum": ["text", "image"] }
        },
        "inputLimits": { "$ref": "#/$defs/inputLimits" },
        "cost": { "$ref": "#/$defs/cost" },
        "promptCache": { "$ref": "#/$defs/promptCache" },
        "contextWindow": { "type": "integer", "exclusiveMinimum": 0 },
        "maxTokens": { "type": "integer", "exclusiveMinimum": 0 },
        "samplingParams": { "type": "object" },
        "samplingParamsByThinkingLevel": { "$ref": "#/$defs/samplingByLevel" },
        "headers": { "$ref": "#/$defs/stringMap" }
      }
    },
    "modelOverride": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "name": { "type": "string", "minLength": 1 },
        "reasoning": { "type": "boolean" },
        "thinkingLevelMap": { "$ref": "#/$defs/thinkingLevelMap" },
        "input": {
          "type": "array",
          "items": { "enum": ["text", "image"] }
        },
        "inputLimits": { "$ref": "#/$defs/inputLimits" },
        "cost": { "$ref": "#/$defs/costOverride" },
        "promptCache": { "$ref": "#/$defs/promptCache" },
        "contextWindow": { "type": "integer", "exclusiveMinimum": 0 },
        "maxTokens": { "type": "integer", "exclusiveMinimum": 0 },
        "samplingParams": { "type": "object" },
        "samplingParamsByThinkingLevel": { "$ref": "#/$defs/samplingByLevel" },
        "headers": { "$ref": "#/$defs/stringMap" }
      }
    },
    "stringMap": {
      "type": "object",
      "additionalProperties": { "type": "string" }
    },
    "thinkingLevelMap": {
      "type": "object",
      "propertyNames": {
        "enum": ["off", "minimal", "low", "medium", "high", "xhigh", "max"]
      },
      "additionalProperties": { "type": ["string", "null"] }
    },
    "promptCache": {
      "type": "object",
      "propertyNames": { "enum": ["short", "long"] },
      "additionalProperties": { "type": "number", "exclusiveMinimum": 0 }
    },
    "samplingByLevel": {
      "type": "object",
      "propertyNames": {
        "enum": ["off", "minimal", "low", "medium", "high", "xhigh", "max"]
      },
      "additionalProperties": { "type": "object" }
    },
    "cost": {
      "type": "object",
      "required": ["input", "output", "cacheRead", "cacheWrite"],
      "additionalProperties": false,
      "properties": {
        "input": { "type": "number" },
        "output": { "type": "number" },
        "cacheRead": { "type": "number" },
        "cacheWrite": { "type": "number" },
        "tiers": {
          "type": "array",
          "items": { "$ref": "#/$defs/costTier" }
        }
      }
    },
    "costOverride": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "input": { "type": "number" },
        "output": { "type": "number" },
        "cacheRead": { "type": "number" },
        "cacheWrite": { "type": "number" },
        "tiers": {
          "type": "array",
          "items": { "$ref": "#/$defs/costTier" }
        }
      }
    },
    "costTier": {
      "type": "object",
      "required": ["inputTokensAbove", "input", "output", "cacheRead", "cacheWrite"],
      "additionalProperties": false,
      "properties": {
        "inputTokensAbove": { "type": "number" },
        "input": { "type": "number" },
        "output": { "type": "number" },
        "cacheRead": { "type": "number" },
        "cacheWrite": { "type": "number" }
      }
    },
    "inputLimits": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "maxRequestBytes": { "type": "integer", "minimum": 1 },
        "images": { "$ref": "#/$defs/imageInputLimits" }
      }
    },
    "imageInputLimits": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "resize": { "$ref": "#/$defs/resizeOptions" },
        "maxPerMessage": { "type": "integer", "minimum": 1 },
        "maxPerRequest": { "type": "integer", "minimum": 1 }
      }
    },
    "resizeOptions": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "maxWidth": { "type": "integer", "minimum": 1 },
        "maxHeight": { "type": "integer", "minimum": 1 },
        "maxBytes": { "type": "integer", "minimum": 1 },
        "jpegQuality": { "type": "integer", "minimum": 1, "maximum": 100 }
      }
    }
  }
}`

// schemaURN is the resource name the compiled schema is registered under.
const schemaURN = "urn:ultrac:models:models.json"

var (
	schemaOnce sync.Once
	compiled   *jsonschema.Schema
	compileErr error
)

// schema compiles the models.json schema once. A compile failure is a
// programming error (the constant above is broken), so it surfaces as a
// validation-time error rather than a panic.
func schema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		value, err := jsonschema.UnmarshalJSON(strings.NewReader(modelsSchema))
		if err != nil {
			compileErr = fmt.Errorf("models: compile schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(schemaURN, value); err != nil {
			compileErr = fmt.Errorf("models: register schema: %w", err)
			return
		}
		compiled, compileErr = compiler.Compile(schemaURN)
		if compileErr != nil {
			compileErr = fmt.Errorf("models: compile schema: %w", compileErr)
		}
	})
	return compiled, compileErr
}

// Load reads one models.json file.
//
// The semantics are pi's (model-config.ts:124-162):
//
//   - an empty path or ENOENT yields an empty config and no error — "the
//     user has no models.json" is the normal case;
//   - any other read failure, a parse failure, or a schema violation
//     yields an error that names the file. The caller downgrades it to a
//     warning: the built-in catalog still resolves.
//
// A UTF-8 BOM is stripped before parsing (editors on Windows write one,
// and encoding/json rejects it with a confusing "invalid character").
func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("failed to load models.json: %w\n\nFile: %s", err, path)
	}
	cfg, err := Parse(raw)
	if err != nil {
		// The trailer names the file — Parse's own wording is
		// file-agnostic (write.go's self-check has no path to blame).
		return Config{}, fmt.Errorf("%w\n\nFile: %s", err, path)
	}
	return cfg, nil
}

// Parse decodes and schema-validates one models.json document: the file's
// bytes, BOM tolerated. It is Load's body without the file — same
// parse-then-validate-then-decode chain, same error wording minus the
// "File: <path>" trailer.
//
// write.go's Save uses it as its self-check: a Config that does not
// round-trip through Parse is a returned error, never a document on disk
// the next launch warns about (plan P1).
func Parse(raw []byte) (Config, error) {
	raw = stripBOM(raw)

	// Validate the document as generic JSON first: jsonschema's decoder
	// is the one that produces the value shape Validate expects (numbers
	// as json.Number), and it doubles as the parse check.
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse models.json: %w", err)
	}
	if err := validate(value); err != nil {
		return Config{}, fmt.Errorf("invalid models.json schema:\n%s", err)
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// Unreachable in practice: the schema check above already parsed
		// the same bytes. Kept so a decoder/validator disagreement is a
		// named error rather than a silently empty config.
		return Config{}, fmt.Errorf("failed to parse models.json: %w", err)
	}
	return cfg, nil
}

// validate checks a decoded document against the schema and formats the
// violations the way pi does: one "  - <dotted.path>: <message>" line per
// leaf error, with required/additionalProperties failures pointing at the
// missing or unknown property rather than its parent object.
func validate(value any) error {
	s, err := schema()
	if err != nil {
		return err
	}
	verr := s.Validate(value)
	if verr == nil {
		return nil
	}
	var validationErr *jsonschema.ValidationError
	if !errors.As(verr, &validationErr) {
		return fmt.Errorf("  - %v", verr)
	}
	var lines []string
	collectValidationErrors(validationErr, &lines)
	if len(lines) == 0 {
		lines = append(lines, "  - unknown schema error")
	}
	return fmt.Errorf("%s", strings.Join(lines, "\n"))
}

// collectValidationErrors walks the error tree to its leaves and formats
// each one. The tree matters: a wrong-typed property reports one leaf
// under the object's error, and only the leaf carries the useful path.
func collectValidationErrors(err *jsonschema.ValidationError, lines *[]string) {
	if len(err.Causes) == 0 {
		*lines = append(*lines, formatValidationError(err))
		return
	}
	for _, cause := range err.Causes {
		collectValidationErrors(cause, lines)
	}
}

func formatValidationError(err *jsonschema.ValidationError) string {
	base := strings.Join(err.InstanceLocation, ".")
	if base == "" {
		base = "root"
	}
	switch k := err.ErrorKind.(type) {
	case *kind.Required:
		// Point at the missing property itself, the way pi's
		// formatValidationPath does.
		for _, missing := range k.Missing {
			return fmt.Sprintf("  - %s: missing required property", joinPath(base, missing))
		}
	case *kind.AdditionalProperties:
		for _, prop := range k.Properties {
			return fmt.Sprintf("  - %s: unknown field", joinPath(base, prop))
		}
	}
	return fmt.Sprintf("  - %s: %s", base, localize(err))
}

// localize renders the error kind's message. The English printer keeps
// the output stable across machines; these lines are diagnostics, not UI.
func localize(err *jsonschema.ValidationError) string {
	printer := message.NewPrinter(language.English)
	if err.ErrorKind == nil {
		return "schema violation"
	}
	return err.ErrorKind.LocalizedString(printer)
}

func joinPath(base, prop string) string {
	if base == "root" {
		return prop
	}
	return base + "." + prop
}

// stripBOM drops a leading UTF-8 byte-order mark.
func stripBOM(b []byte) []byte {
	const bom = "\xef\xbb\xbf"
	if len(b) >= len(bom) && string(b[:len(bom)]) == bom {
		return b[len(bom):]
	}
	return b
}
