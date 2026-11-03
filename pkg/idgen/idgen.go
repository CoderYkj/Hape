// Package idgen mints identifiers that stay unique under concurrency, without a database
// round trip.
//
// Why not time.Now().UnixNano() alone: on Windows the wall clock advances in steps of a
// millisecond or worse, so goroutines in the same tick produce the same value. A load test
// against the real handlers found exactly that - sessions, tickets, teams and match ids all
// collided under concurrency and surfaced as 409 conflicts that looked like state bugs
// (spec 0019).
//
// Layout: <prefix><base36 unix-nano>-<base36 counter>-<6 hex chars of randomness>
//
//   - the counter makes collisions impossible inside one process even when the clock stalls;
//   - the random suffix covers a restart inside the same clock tick, and two hosts whose
//     clocks agree to the nanosecond.
//
// The result stays readable and sortable-ish by time, which is what these ids are for.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync/atomic"
	"time"
)

var counter atomic.Uint64

// New returns a unique identifier with the given prefix (typically "s_", "t_", "m_").
func New(prefix string) string {
	now := time.Now().UnixNano()
	seq := counter.Add(1)

	var entropy [3]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		// Randomness is the belt, not the braces: the timestamp plus the in-process counter
		// already cannot collide on one host, so a failing RNG must not stop the service.
		entropy = [3]byte{}
	}

	return prefix +
		strconv.FormatInt(now, 36) + "-" +
		strconv.FormatUint(seq, 36) + "-" +
		hex.EncodeToString(entropy[:])
}
