package database

import (
	"context"
	"fmt"
)

// BounceReaderLockName names the session advisory lock one poll of the NDR
// reader holds (hashtextextended(BounceReaderLockName, 0)): with several
// replicas, one reads the mailboxes at a time.
const BounceReaderLockName = "skymail-backend bounce reader"

// WithBounceReaderLock runs fn holding the NDR reader's advisory lock, on a
// connection of its own; ran is false (and fn not run) when another process
// holds it. The lock goes with the connection if the process dies.
func (s *Store) WithBounceReaderLock(ctx context.Context, fn func(context.Context) error) (ran bool, err error) {
	conn, err := s.Conn.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("bounce reader lock: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, BounceReaderLockName).Scan(&locked); err != nil {
		return false, fmt.Errorf("bounce reader lock: %w", err)
	}
	if !locked {
		return false, nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, BounceReaderLockName)
	}()
	return true, fn(ctx)
}
