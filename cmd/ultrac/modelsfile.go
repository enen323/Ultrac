package main

// modelsfile.go — loading the two models.json layers into one merged
// snapshot (docs/开发计划/新功能计划/provider-config-plan.md P1).
//
// # The two files
//
//	<ConfigHome>/models.json      the user layer (~/.ultrac/models.json)
//	<cwd>/.ultrac/models.json     the project layer
//
// They merge over the built-in catalog with the same precedence the
// settings layers use: project over user, and a project entry with the
// same providerId replaces the user entry for that provider wholesale
// (D1). Everything below the provider id follows internal/models/merge.go.
//
// # Why a load failure is only a warning
//
// pi's model-config.ts catches per file and continues with whatever
// loaded: a broken models.json must not take down a run the built-in
// catalog alone can serve. Per-provider composition problems (an empty
// entry, a model with no api) travel the same road — they are reported
// here and again, as a hard error, by resolveModel when a run actually
// selects the broken provider. That asymmetry is the point: the warning
// tells the user something is wrong; the error stops them from sending a
// request that cannot work.

import (
	"fmt"
	"io"
	"path/filepath"

	"ultrac/internal/config"
	"ultrac/internal/models"
)

// loadModelSnapshot reads both layers and merges them over the built-in
// catalog. The result is never nil: with no files, or with every file
// broken, the snapshot is the built-in catalog.
func loadModelSnapshot(cwd string, stderr io.Writer) *models.Snapshot {
	cfgs := make([]models.Config, 0, 2)
	for _, path := range modelsFilePaths(cwd) {
		cfg, err := models.Load(path)
		if err != nil {
			// Degraded, not fatal (see the file comment). The error
			// already names the file.
			fmt.Fprintf(stderr, "warning: %v\n", err)
			continue
		}
		cfgs = append(cfgs, cfg)
	}
	return models.Build(cfgs...)
}

// modelsFilePaths lists the models.json layers, lowest precedence first.
func modelsFilePaths(cwd string) []string {
	return []string{
		filepath.Join(config.ConfigHome(), "models.json"),
		filepath.Join(cwd, config.DirName, "models.json"),
	}
}
