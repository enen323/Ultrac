package tool

// Per-path serialization for file-mutating tools, ported from pi's
// packages/coding-agent/src/core/tools/file-mutation-queue.ts.
//
// The agent loop runs independent tool calls concurrently (P2's parallel
// batch executor). Two calls touching the same file — an edit and a write,
// or two edits whose ranges the model mis-declared as disjoint — would
// otherwise read-modify-write the same bytes from two goroutines and one
// change would vanish. The queue serializes same-file operations while
// different files still run in parallel.
//
// The queue key is the realpath when the file exists (so a symlink and its
// target serialize together, as pi does) and the cleaned absolute path
// otherwise (a not-yet-created file has no realpath).

import (
	"path/filepath"
	"sync"
)

var (
	fileQueueMu sync.Mutex
	fileQueues  = map[string]*fileQueueEntry{}
)

type fileQueueEntry struct {
	mu   sync.Mutex
	refs int
}

// withFileMutationQueue runs fn while holding the mutation lock for path.
func withFileMutationQueue(path string, fn func() error) error {
	key := mutationQueueKey(path)

	fileQueueMu.Lock()
	entry, ok := fileQueues[key]
	if !ok {
		entry = &fileQueueEntry{}
		fileQueues[key] = entry
	}
	entry.refs++
	fileQueueMu.Unlock()

	entry.mu.Lock()
	err := fn()
	entry.mu.Unlock()

	fileQueueMu.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(fileQueues, key)
	}
	fileQueueMu.Unlock()
	return err
}

// mutationQueueKey resolves the queue key for a path.
func mutationQueueKey(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}
