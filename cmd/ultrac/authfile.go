package main

// authfile.go — loading the two auth.json layers into one credential
// stack (docs/开发计划/新功能计划/provider-config-plan.md P3).
//
// # The two files
//
//	<ConfigHome>/auth.json      the user layer (~/.ultrac/auth.json)
//	<cwd>/.ultrac/auth.json     the project layer
//
// They merge with the same precedence the settings and models.json
// layers use: project over user, and a project entry with the same
// providerId replaces the user entry for that provider wholesale (D1).
// The package itself (internal/config/auth) owns the file semantics —
// 0600, literal keys, oauth rejection, per-provider degradation; this
// file only names the paths, exactly as modelsfile.go does for
// models.json.
//
// # Why a load failure is only a warning
//
// Same rule as every other layer file: a broken auth.json must not take
// down a run the other layers can serve. File-level failures (unreadable,
// unparseable) come back through the warn callback; per-provider problems
// (an oauth block, a malformed entry) are on the returned Stack and the
// caller prints them as warnings too — each costs exactly one provider
// its credential.

import (
	"fmt"
	"io"
	"path/filepath"

	"ultrac/internal/config"
	"ultrac/internal/config/auth"
)

// loadAuthStack reads both layers and merges them. The result is never
// nil: with no files, or with every file broken, the stack is empty.
func loadAuthStack(cwd string, stderr io.Writer) *auth.Stack {
	return auth.LoadStack(authFilePaths(cwd), func(err error) {
		// Degraded, not fatal (see the file comment). The error
		// already names the file.
		fmt.Fprintf(stderr, "warning: %v\n", err)
	})
}

// authFilePaths lists the auth.json layers, lowest precedence first.
func authFilePaths(cwd string) []string {
	return []string{
		filepath.Join(config.ConfigHome(), "auth.json"),
		filepath.Join(cwd, config.DirName, "auth.json"),
	}
}
