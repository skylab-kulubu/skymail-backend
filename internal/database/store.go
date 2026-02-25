package database

import (
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
