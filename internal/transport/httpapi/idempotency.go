package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/idempotency"
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	headerReplayed       = "Idempotent-Replayed"
	maxIdempotencyKeyLen = 255
)

// replayedHeaders are the response headers that must be identical on replay.
var replayedHeaders = []string{"Location", "ETag"}

// TxManager opens the transaction shared by the idempotency record and the
// business write.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// idempotencyGuard is a decorator for unsafe endpoints. When a request
// carries an Idempotency-Key, the key is claimed, the handler runs and its
// response is stored, all in one transaction:
//
//   - a retry after success replays the stored response;
//   - a concurrent duplicate waits for the first request, then replays it;
//   - a 5xx rolls everything back, so the client can safely retry;
//   - reusing a key for a different payload is rejected.
//
// Requests without the header behave exactly as before.
type idempotencyGuard struct {
	responder
	tx    TxManager
	store idempotency.Store
}

// errHandlerFailed carries a 5xx response out of the transaction so it is
// rolled back but the client still receives the original error.
type errHandlerFailed struct {
	response *bufferedResponse
}

func (e errHandlerFailed) Error() string { return "handler failed, idempotent transaction rolled back" }

func (g idempotencyGuard) protect(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(headerIdempotencyKey)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > maxIdempotencyKeyLen {
				g.error(w, r, badRequest("%s must be at most %d characters", headerIdempotencyKey, maxIdempotencyKeyLen))
				return
			}

			hash, err := hashRequest(w, r)
			if err != nil {
				g.error(w, r, err)
				return
			}
			g.serve(w, r, next, idempotency.Record{Scope: scope, Key: key, RequestHash: hash})
		})
	}
}

func (g idempotencyGuard) serve(w http.ResponseWriter, r *http.Request, next http.Handler, claim idempotency.Record) {
	var (
		fresh  *bufferedResponse
		replay *idempotency.Record
	)

	err := g.tx.WithinTx(r.Context(), func(ctx context.Context) error {
		existing, claimed, err := g.store.Claim(ctx, claim.Scope, claim.Key, claim.RequestHash)
		if err != nil {
			return err
		}
		if !claimed {
			replay = &existing
			return nil
		}

		fresh = newBufferedResponse()
		next.ServeHTTP(fresh, r.WithContext(ctx))
		if fresh.status >= http.StatusInternalServerError {
			return errHandlerFailed{fresh}
		}
		return g.store.Complete(ctx, fresh.toRecord(claim))
	})

	var failed errHandlerFailed
	switch {
	case errors.As(err, &failed):
		failed.response.writeTo(w)
	case err != nil:
		g.error(w, r, err)
	case replay != nil && replay.RequestHash != claim.RequestHash:
		g.error(w, r, domain.ErrIdempotencyKeyReuse)
	case replay != nil:
		writeReplay(w, *replay)
	default:
		fresh.writeTo(w)
	}
}

// hashRequest fingerprints method, path and body, and restores the body so
// the handler can still read it.
func hashRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return "", describeDecodeError(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	sum := sha256.New()
	sum.Write([]byte(r.Method + " " + r.URL.Path + "\n"))
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func writeReplay(w http.ResponseWriter, rec idempotency.Record) {
	for name, value := range rec.Headers {
		w.Header().Set(name, value)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerReplayed, "true")
	w.WriteHeader(rec.StatusCode)
	_, _ = w.Write(rec.Body)
}

// bufferedResponse captures a handler's response so it can be stored before
// anything is sent to the client.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: http.Header{}, status: http.StatusOK}
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }

func (b *bufferedResponse) toRecord(claim idempotency.Record) idempotency.Record {
	claim.StatusCode = b.status
	claim.Body = b.body.Bytes()
	claim.Headers = map[string]string{}
	for _, name := range replayedHeaders {
		if value := b.header.Get(name); value != "" {
			claim.Headers[name] = value
		}
	}
	return claim
}

func (b *bufferedResponse) writeTo(w http.ResponseWriter) {
	for name, values := range b.header {
		w.Header()[name] = values
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
