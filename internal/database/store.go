package database

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	*Queries
	Conn *pgxpool.Pool
}

func NewStore(conn *pgxpool.Pool) *Store {
	return &Store{
		Queries: New(conn),
		Conn:    conn,
	}
}

// InTx runs fn in one transaction: it commits when fn returns nil and rolls
// back everything fn wrote when fn returns an error — a check it made after
// writing included.
func (s *Store) InTx(ctx context.Context, fn func(*Queries) error) error {
	return pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		return fn(s.WithTx(tx))
	})
}
