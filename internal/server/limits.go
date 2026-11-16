package server

import (
	"crypto/rand"
	"time"
)

// CodeAlphabet is what codes are made of: letters and digits without the ones
// that look alike (I, O, 0, 1), 32 of them.
const CodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// CodeLength is as long as RBEnhanced's public server's codes are; the field
// holds up to 8.
const CodeLength = 5

// RandomCode is a code from crypto/rand: 32^5, about 33.5 million of them.
func RandomCode() string {
	var b [CodeLength]byte
	_, _ = rand.Read(b[:]) // never fails (Go 1.24+ crashes rather than return an error)
	for i := range b {
		b[i] = CodeAlphabet[b[i]&31]
	}
	return string(b[:])
}

// bucket allows perMinute events a minute, perMinute of them at once: a token
// bucket that refills continuously.
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) refill(perMinute int, now time.Time) {
	if b.last.IsZero() {
		b.tokens, b.last = float64(perMinute), now
		return
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(float64(perMinute), b.tokens+elapsed.Minutes()*float64(perMinute))
		b.last = now
	}
}

// allows is whether one more event fits now.
func (b *bucket) allows(perMinute int, now time.Time) bool {
	b.refill(perMinute, now)
	return b.tokens >= 1
}

// take counts an event.
func (b *bucket) take(perMinute int, now time.Time) {
	b.refill(perMinute, now)
	b.tokens--
}

// full is whether the bucket has refilled, so forgetting it changes nothing.
func (b *bucket) full(perMinute int, now time.Time) bool {
	b.refill(perMinute, now)
	return b.tokens >= float64(perMinute)
}
