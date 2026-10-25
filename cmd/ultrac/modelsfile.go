package main

// modelsfile.go — where a request's provider definitions come from.
//
// One place: the settings `providers` section. The file keeps its name from
// the two-file era; what it does now is turn the resolved settings into the
// catalog snapshot the agent loop and the model picker read.
//
// There is no fallback source. models.json and auth.json were read here while
// the configuration was being unified; with the migration command gone they
// would only be a second authority nobody maintains, so they are not read at
// all.

import (
	"ultrac/internal/config"
	"ultrac/internal/models"
)

// snapshotFromSettings builds the catalog snapshot from the resolved settings.
// The result is never nil: with no providers declared, it is the built-in
// catalog, and model resolution reports "no model configured" by name rather
// than pretending a provider exists.
func snapshotFromSettings(merged config.Settings) *models.Snapshot {
	return models.Build(models.Config{Providers: merged.Providers})
}
