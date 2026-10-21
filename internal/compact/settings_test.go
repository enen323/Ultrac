package compact

import "testing"

func TestDefaultSettings(t *testing.T) {
	// pi: DEFAULT_COMPACTION_SETTINGS (compaction.ts:126-130). Pinned
	// because the numbers are the contract between this engine, the config
	// defaults (P-C2) and pi.
	got := DefaultSettings()
	want := Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}
	if got != want {
		t.Fatalf("DefaultSettings = %+v, want %+v", got, want)
	}
}

func TestShouldCompact(t *testing.T) {
	cases := []struct {
		name          string
		contextTokens int
		contextWindow int
		settings      Settings
		want          bool
	}{
		{"over the line", 190000, 200000, DefaultSettings(), true},
		{"under the line", 100000, 200000, DefaultSettings(), false},
		{"exactly at the line does not trigger", 200000 - 16384, 200000, DefaultSettings(), false},
		{"one over the line triggers", 200000 - 16384 + 1, 200000, DefaultSettings(), true},
		{"disabled never triggers", 199999, 200000, Settings{Enabled: false, ReserveTokens: 16384}, false},
		{"no window never triggers", 199999, 0, DefaultSettings(), false},
		{"negative window never triggers", 10, -5, DefaultSettings(), false},
		{"custom reserve", 160000, 200000, Settings{Enabled: true, ReserveTokens: 50000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldCompact(c.contextTokens, c.contextWindow, c.settings); got != c.want {
				t.Errorf("ShouldCompact(%d, %d, %+v) = %v, want %v",
					c.contextTokens, c.contextWindow, c.settings, got, c.want)
			}
		})
	}
}
