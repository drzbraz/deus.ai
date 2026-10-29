// Package postgres implements the repository ports on top of PostgreSQL (pgx).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/drzbraz/deus.ai/internal/domain"
)

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so repositories run
// the same SQL inside or outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

// conn is embedded by every repository. It returns the transaction stored in
// ctx by TxManager, or the pool when no transaction is active.
type conn struct {
	pool *pgxpool.Pool
}

func (c conn) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return c.pool
}

// TxManager implements the Unit of Work pattern on top of pgx transactions.
type TxManager struct {
	pool *pgxpool.Pool
}

func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// WithinTx runs fn in a transaction, committing when fn succeeds. When ctx
// already carries a transaction, a savepoint is used instead, so a failing
// inner unit of work rolls back only its own writes.
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	tx, err := m.begin(ctx)
	if err != nil {
		return err
	}
	// Rollback is a no-op after a successful commit. WithoutCancel ensures we
	// still release locks when the request context was cancelled.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return wrapErr("commit transaction", err)
	}
	return nil
}

// begin starts a transaction, or a savepoint inside the one already in ctx.
func (m *TxManager) begin(ctx context.Context) (pgx.Tx, error) {
	var (
		tx  pgx.Tx
		err error
	)
	if outer, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		tx, err = outer.Begin(ctx)
	} else {
		tx, err = m.pool.Begin(ctx)
	}
	if err != nil {
		return nil, wrapErr("begin transaction", err)
	}
	return tx, nil
}

// Postgres error codes we translate into domain errors.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeQueryCanceled       = "57014"
	codeAdminShutdown       = "57P01"
	codeCrashShutdown       = "57P02"
	codeCannotConnectNow    = "57P03"
	codeTooManyConnections  = "53300"
	codeLockNotAvailable    = "55P03"
	classConnectionFailure  = "08"
)

// wrapErr adds the failed operation to err and translates driver errors into
// domain errors, so upper layers never depend on pgx. The driver error stays
// in the chain for logging.
func wrapErr(op string, err error) error {
	if kind := classify(err); kind != nil {
		return fmt.Errorf("%s: %w: %w", op, kind, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func classify(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation:
		return domain.ErrAlreadyExists
	case errors.As(err, &pgErr) && pgErr.Code == codeForeignKeyViolation:
		return domain.ErrNotFound
	case isUnavailable(err):
		return domain.ErrUnavailable
	default:
		return nil
	}
}

// isUnavailable reports errors caused by the database being down, overloaded
// or too slow. Clients receive 503 for these and may retry later.
func isUnavailable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	// A pooled connection severed mid-query (database restart, network cut)
	// surfaces as an EOF from the driver.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}

	var connectErr *pgconn.ConnectError
	var netErr net.Error
	if errors.As(err, &connectErr) || errors.As(err, &netErr) {
		return true
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case codeQueryCanceled, codeAdminShutdown, codeCrashShutdown, codeCannotConnectNow,
		codeTooManyConnections, codeLockNotAvailable:
		return true
	}
	return len(pgErr.Code) == 5 && pgErr.Code[:2] == classConnectionFailure
}
