package eventbus

import "crypto/rand"

// readRandBytes is a small indirection so tests can swap the source.
// Defaults to crypto/rand — security-sensitive (event_id is the
// idempotency key; predictable values would let an attacker forge
// "already delivered" ACKs against the consumer).
var readRandBytes = func(b []byte) (int, error) {
	return rand.Read(b)
}
