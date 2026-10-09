package builtin

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func openForProbe(path string) (io.ReadCloser, error) { return os.Open(path) }

// ignoreDirs are directory names never descended into.
//
// The TypeScript implementation got this for free by shelling out to
// ripgrep, which honours .gitignore, so node_modules was invisible without
// anything in the code saying so. The Go walker has no .gitignore support, so
// the exclusion has to be explicit or a Grep in this repository would spend
// its entire result budget inside node_modules.
//
// The list is names, not paths, and they are skipped at any depth — a nested
// `vendor/` is as uninteresting as a top-level one.
var ignoreDirs = map[string]bool{
	// Version control internals.
	".git": true, ".svn": true, ".hg": true, ".bzr": true, ".jj": true, ".sl": true,
	// Dependency trees.
	"node_modules": true, "bower_components": true, "vendor": true,
	// Language caches and build output.
	"__pycache__": true, ".venv": true, "venv": true, ".tox": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".gradle": true, ".maven": true, "target": true,
	// JavaScript build output.
	"dist": true, ".next": true, ".nuxt": true, ".turbo": true, ".parcel-cache": true,
	// Editors and tooling state.
	".idea": true, ".workbuddy": true,
}

// walkFiles visits every regular file under root, skipping ignored directories.
//
// The visitor's error stops the walk, which is how a result limit is enforced
// without walking the remaining tree.
func walkFiles(root string, visit func(path string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped rather than fatal: a single
			// permission-denied subdirectory should not abort a whole search.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && ignoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return visit(path, d)
	})
}

// relativeToRoot renders a path relative to root, with forward slashes.
//
// Relative output is what the model wants: an absolute path in every result
// line costs tokens and adds nothing, because the working directory is in the
// system prompt.
func relativeToRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return toSlash(path)
	}
	return toSlash(rel)
}

// isProbablyBinary applies the cheap heuristic: a NUL byte in the first block.
//
// grep needs this because a binary file that happens to contain a match would
// otherwise inject unreadable bytes into the transcript. It reads only the
// first 8KB, so the cost is one small read per candidate file.
func isProbablyBinary(path string) bool {
	f, err := openForProbe(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if n <= 0 {
		return false
	}
	return strings.IndexByte(string(head[:n]), 0) >= 0
}
