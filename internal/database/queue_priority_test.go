package database

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The priority lanes: a claim takes due rows of one lane only, oldest due
// first, so the dispatcher can take the system lane before the bulk one.
func TestClaimTakesOnlyTheLaneItAsksFor(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	// Three bulk rows due well before the system row: the system lane still
	// gets the system row, and the bulk lane never does.
	bulk := queueOf(t, db, 3)
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET priority = 0`); err != nil {
		t.Fatal(err)
	}
	system := queueOf(t, db, 1)[0]

	taken, err := db.ProcessQueueItems(ctx, ProcessQueueItemsParams{ClaimedBy: "skymail-a-1", MaxRows: 10, SystemLane: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 || taken[0].ID != system || taken[0].Priority != 1 {
		t.Fatalf("system lane took %v, want only the system row %s", ids(taken), system)
	}

	taken, err = db.ProcessQueueItems(ctx, ProcessQueueItemsParams{ClaimedBy: "skymail-a-1", MaxRows: 2, SystemLane: false})
	if err != nil {
		t.Fatal(err)
	}
	// RETURNING has no order of its own: compare as a set.
	got := map[uuid.UUID]bool{}
	for _, row := range taken {
		got[row.ID] = true
	}
	if len(taken) != 2 || !got[bulk[0]] || !got[bulk[1]] {
		t.Fatalf("bulk lane took %v, want the two oldest bulk rows %v", ids(taken), bulk[:2])
	}
	for _, row := range taken {
		if row.Priority != 0 {
			t.Fatalf("bulk lane took row %s of priority %d", row.ID, row.Priority)
		}
	}
}

// A row queued without naming a priority (an image from before the lanes, a
// hand-made insert) is in the system lane.
func TestQueueRowWithoutPriorityIsInTheSystemLane(t *testing.T) {
	db := lifecycleStore(t)
	id := queueOf(t, db, 1)[0]
	var priority int16
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT priority FROM mail_queue WHERE id = $1`, id).Scan(&priority); err != nil {
		t.Fatal(err)
	}
	if priority != 1 {
		t.Fatalf("priority %d, want 1 (system lane)", priority)
	}
}

func ids(rows []MailQueue) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}
