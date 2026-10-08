package session

import "time"

// parseISO reads a transcript timestamp.
//
// The two accepted layouts are what transcripts actually contain: the
// millisecond form this package writes, and the plain second form some older
// writers used. Both are UTC; a local-time timestamp would sort wrongly
// against everything else in the file.
func parseISO(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ParseTimestamp exposes parseISO for callers that need an entry's time.
func (e *Entry) ParsedTimestamp() time.Time { return parseISO(e.Timestamp) }
