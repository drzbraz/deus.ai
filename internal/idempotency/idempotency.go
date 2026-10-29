// Package idempotency defines how responses to requests carrying an
// Idempotency-Key are remembered, so retries and concurrent duplicates are
// answered with the original response instead of repeating side effects.
package idempotency

import (
	"context"
	"time"
)

// Record is the first response produced for a (scope, key) pair.
type Record struct {
	Scope       string
	Key         string
	RequestHash string
	StatusCode  int
	// Headers keeps response headers that must survive a replay (ETag, Location).
	Headers map[string]string
	Body    []byte
}

// Store persists records. Claim and Complete run inside the same transaction
// as the business write, which makes "do the work" and "remember the answer"
// a single atomic step.
type Store interface {
	// Claim reserves the key for the current transaction. When the key was
	// already completed it returns the stored record (claimed is false).
	// A concurrent duplicate blocks inside Claim until the first transaction
	// ends: it then either sees the committed record or takes over the key if
	// the first transaction rolled back.
	Claim(ctx context.Context, scope, key, requestHash string) (existing Record, claimed bool, err error)
	Complete(ctx context.Context, record Record) error
	DeleteExpired(ctx context.Context, createdBefore time.Time) (int64, error)
}
