package compact

// Settings is the compaction configuration: the trigger threshold expressed
// as a token reserve, and how much recent context survives a compaction.
//
// pi: CompactionSettings (compaction.ts:120-124). The plan (D6) persists this
// type through config.Settings' carve-out; this struct is the engine-side
// shape and carries no JSON tags of its own.

// Settings controls when compaction runs and how much it keeps.
type Settings struct {
	// Enabled gates the automatic trigger entirely. Manual compaction
	// (P-C2) runs regardless.
	Enabled bool
	// ReserveTokens is the headroom a request must keep free: compaction
	// fires when the estimated context exceeds
	// contextWindow - ReserveTokens.
	ReserveTokens int
	// KeepRecentTokens is roughly how much recent context a compaction
	// keeps ahead of the cut point.
	KeepRecentTokens int
}

// DefaultSettings is pi's DEFAULT_COMPACTION_SETTINGS (compaction.ts:126-130).
func DefaultSettings() Settings {
	return Settings{
		Enabled:          true,
		ReserveTokens:    16384,
		KeepRecentTokens: 20000,
	}
}

// ShouldCompact reports whether the estimated context has crossed the
// trigger line.
//
// pi: shouldCompact (compaction.ts:267-270). A model with no recorded window
// (contextWindow <= 0) never triggers: guessing a window would compact
// sessions that fit comfortably.
func ShouldCompact(contextTokens, contextWindow int, s Settings) bool {
	if !s.Enabled {
		return false
	}
	if contextWindow <= 0 {
		return false
	}
	return contextTokens > contextWindow-s.ReserveTokens
}
