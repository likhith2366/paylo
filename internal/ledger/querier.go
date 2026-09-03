package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Querier is the read surface shared by *pgxpool.Pool and pgx.Tx, so read
// helpers work both inside and outside a transaction without duplication.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
