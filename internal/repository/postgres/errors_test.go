package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/drzbraz/deus.ai/internal/domain"
)

func TestWrapErr_TranslatesDriverErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"no rows", pgx.ErrNoRows, domain.ErrNotFound},
		{"unique violation", &pgconn.PgError{Code: codeUniqueViolation}, domain.ErrAlreadyExists},
		{"foreign key violation", &pgconn.PgError{Code: codeForeignKeyViolation}, domain.ErrNotFound},
		{"deadline", context.DeadlineExceeded, domain.ErrUnavailable},
		{"statement timeout", &pgconn.PgError{Code: codeQueryCanceled}, domain.ErrUnavailable},
		{"admin shutdown", &pgconn.PgError{Code: codeAdminShutdown}, domain.ErrUnavailable},
		{"too many connections", &pgconn.PgError{Code: codeTooManyConnections}, domain.ErrUnavailable},
		{"connection failure class", &pgconn.PgError{Code: "08006"}, domain.ErrUnavailable},
		{"connection severed mid-query", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), domain.ErrUnavailable},
		{"connection closed", io.EOF, domain.ErrUnavailable},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, domain.ErrUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := wrapErr("op", tt.err)

			assert.ErrorIs(t, err, tt.want)
			assert.ErrorIs(t, err, tt.err, "original error must stay inspectable")
			assert.Contains(t, err.Error(), "op: ")
		})
	}
}

func TestWrapErr_UnknownErrorsStayInternal(t *testing.T) {
	cause := &pgconn.PgError{Code: "23001"} // e.g. the append-only trigger
	err := wrapErr("op", cause)

	assert.ErrorIs(t, err, cause)
	assert.False(t, errors.Is(err, domain.ErrUnavailable))
	assert.False(t, isUnavailable(errors.New("plain")))
}

func TestToMigrateURL(t *testing.T) {
	assert.Equal(t, "pgx5://u:p@h/db", toMigrateURL("postgres://u:p@h/db"))
	assert.Equal(t, "pgx5://u:p@h/db", toMigrateURL("postgresql://u:p@h/db"))
	assert.Equal(t, "pgx5://h/db", toMigrateURL("pgx5://h/db"))
}
