package database

import (
	"context"
	"testing"
)

// The record of a removed suppression is kept three years from when it was
// written, then the content sweep's step deletes it, as the record of a
// reversed unsubscribe is (Yusuf, 2026-10-27).
func TestSuppressionRemovalsAreKeptThreeYears(t *testing.T) {
	s := lifecycleStore(t)
	ctx := context.Background()
	if _, err := s.Conn.Exec(ctx, `
		INSERT INTO mail_suppression_removals (removed_at, actor_sub, source, reason_code, count, first_seen) VALUES
		    (now() - interval '3 years' - interval '1 day', 'sub', 'dsn', '5.1.1', 1, now() - interval '4 years'),
		    (now() - interval '4 years', 'sub', 'smtp', '550', 2, now() - interval '5 years'),
		    (now() - interval '3 years' + interval '1 day', 'sub', 'dsn', '5.1.1', 1, now() - interval '4 years'),
		    (now(), 'sub', 'dsn', '5.1.1', 1, now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.DeleteSuppressionRemovals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d, want the two older than three years", deleted)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_suppression_removals WHERE removed_at > now() - interval '3 years'`); n != 2 {
		t.Errorf("%d records within three years left, want 2", n)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_suppression_removals`); n != 2 {
		t.Errorf("%d records left, want 2", n)
	}
}
