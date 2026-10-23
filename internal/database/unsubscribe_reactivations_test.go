package database

import (
	"context"
	"testing"
)

// The record of a reversed unsubscribe is kept three years from when it was
// written, then the content sweep's step deletes it (KVKK notice s6; hub
// ticket kvkk-s6-yayin-oncesi 14).
func TestUnsubscribeReactivationsAreKeptThreeYears(t *testing.T) {
	s := lifecycleStore(t)
	ctx := context.Background()
	if _, err := s.Conn.Exec(ctx, `
		INSERT INTO mail_unsubscribe_reactivations (reactivated_at, actor, actor_sub, reason, stream, source, unsubscribed_at) VALUES
		    (now() - interval '3 years' - interval '1 day', 'recipient', NULL, NULL, 'duyuru', 'link', now() - interval '4 years'),
		    (now() - interval '4 years', 'operator', 'sub', 'yanlışlıkla', 'duyuru', 'link', now() - interval '5 years'),
		    (now() - interval '3 years' + interval '1 day', 'recipient', NULL, NULL, 'duyuru', 'link', now() - interval '4 years'),
		    (now(), 'recipient', NULL, NULL, 'duyuru', 'link', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.DeleteUnsubscribeReactivations(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d, want the two older than three years", deleted)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_unsubscribe_reactivations WHERE reactivated_at > now() - interval '3 years'`); n != 2 {
		t.Errorf("%d records within three years left, want 2", n)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_unsubscribe_reactivations`); n != 2 {
		t.Errorf("%d records left, want 2", n)
	}
}
