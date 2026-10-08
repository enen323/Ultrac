package anthropic

import (
	"crypto/rand"
	"encoding/binary"
)

// jitterFloat returns a value in [0,1) for retry jitter.
//
// crypto/rand rather than math/rand on purpose. Jitter exists to de-synchronise
// clients that hit the same overload; a seeded math/rand produces the same
// sequence in every process, so every client backs off in lockstep and
// reproduces the thundering herd the jitter was meant to break.
//
// If the entropy source fails there is no correct fallback — any value would be
// predictable — so this returns ok=false and the caller retries with no
// jitter. That is degraded but not wrong; a fixed jitter would be actively
// harmful, since a synchronised jitter is no jitter at all.
func jitterFloat() (float64, bool) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, false
	}
	// Top 53 bits give a uniform double in [0,1).
	v := binary.BigEndian.Uint64(b[:]) >> 11
	return float64(v) / float64(uint64(1)<<53), true
}
