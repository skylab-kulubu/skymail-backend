package database

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The record of who read a person's mail history (recipient_history_views,
// skymail-yeni-tasarim B2).

// Account erasure deletes the reads of the person's addresses, however the
// reader wrote the address, and replaces the person where they read someone
// else's; Ayşe's history read by Ayşe's colleague stays as it was.
func TestEraseAccountDeletesTheReadsOfThePersonsHistory(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	for _, read := range []struct {
		address, sub, name string
	}{
		{"  DENIZ@example.com ", otherSub, "Ayşe Kaya"},
		{erasedSchool, otherSub, "Ayşe Kaya"},
		{"ayse@example.com", erasedSub, "Deniz Yılmaz"},
		{"ayse@example.com", otherSub, "Ayşe Kaya"},
	} {
		sub, name := read.sub, read.name
		if err := store.RecordRecipientHistoryView(ctx, RecordRecipientHistoryViewParams{
			Email: read.address, ActorSub: &sub, ActorName: &name, Results: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}

	receipt, err := store.EraseAccount(ctx, eraseDeniz(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if n := receiptCounts(t, receipt)[AccountErasureHistoryViewsDeleted]; n != 2 {
		t.Errorf("history_views_deleted = %d, want the two reads of Deniz's addresses", n)
	}
	if n := queryCount(t, store, `SELECT count(*) FROM recipient_history_views`); n != 2 {
		t.Fatalf("reads left = %d, want Ayşe's two", n)
	}
	if n := queryCount(t, store, `SELECT count(*) FROM recipient_history_views
		WHERE actor_sub = $1 AND actor_name = $2`, DeletedUserSubject, DeletedUserName); n != 1 {
		t.Errorf("Deniz's read of Ayşe's history: %d rows read by Silinmiş kullanıcı, want 1", n)
	}
	if n := queryCount(t, store, `SELECT count(*) FROM recipient_history_views
		WHERE actor_sub = $1 AND address_sha256 = encode(sha256('ayse@example.com'::bytea), 'hex')`, otherSub); n != 1 {
		t.Errorf("the colleague's read of Ayşe's history: %d, want it kept", n)
	}
}

// The content sweep's turn deletes the reads older than their retention, a
// batch at a time, and leaves the newer ones.
func TestOldHistoryReadsAreDeleted(t *testing.T) {
	store := lifecycleStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO recipient_history_views (address_sha256, actor_sub, actor_name, results, created_at)
		SELECT repeat('a', 64), 'okur', 'Okur', 1, now() - interval '800 days' FROM generate_series(1, 5);
		INSERT INTO recipient_history_views (address_sha256, actor_sub, actor_name, results, created_at)
		VALUES (repeat('b', 64), 'okur', 'Okur', 1, now() - interval '10 days');`); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteRecipientHistoryViews(ctx, 730, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want the five past 730 days", deleted)
	}
	if n := queryCount(t, store, `SELECT count(*) FROM recipient_history_views`); n != 1 {
		t.Errorf("reads left = %d, want the recent one", n)
	}
}
