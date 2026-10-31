package pg

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL SQLSTATE codes we actually translate. The full list is huge; a code
// that is not here is a bug or an infrastructure problem, and both should be a
// 500 with the driver message intact.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeNotNullViolation    = "23502"
	codeSerializationFail   = "40001"
	codeDeadlock            = "40P01"
)

// asPgError unwraps a *pgconn.PgError, if there is one.
func asPgError(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr, true
	}
	return nil, false
}

// SQLState returns the SQLSTATE of err, or "" when it is not a database error.
func SQLState(err error) string {
	if pgErr, ok := asPgError(err); ok {
		return pgErr.Code
	}
	return ""
}

// IsUniqueViolation reports whether err is a unique-constraint violation.
//
// This is how every idempotency key in this backend works: the store tries the
// write, the database refuses the duplicate, and the store translates that into
// "already applied" instead of trusting a prior SELECT.
func IsUniqueViolation(err error) bool { return SQLState(err) == codeUniqueViolation }

// IsCheckViolation reports whether err is a CHECK constraint violation, i.e. the
// database refused an invariant the Go code should have refused first.
func IsCheckViolation(err error) bool { return SQLState(err) == codeCheckViolation }

// IsForeignKeyViolation reports a missing parent row.
func IsForeignKeyViolation(err error) bool { return SQLState(err) == codeForeignKeyViolation }

// IsRetryable reports whether the statement lost a race that a retry can win.
func IsRetryable(err error) bool {
	state := SQLState(err)
	return state == codeSerializationFail || state == codeDeadlock
}

// ConstraintName returns the violated constraint, which is a stable, human
// meaningful string (e.g. "ledger_entries_idempotent").
func ConstraintName(err error) string {
	if pgErr, ok := asPgError(err); ok {
		return pgErr.ConstraintName
	}
	return ""
}
