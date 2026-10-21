package handlers

import (
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// A send's counts tell of people below the bound retained totals keep, and
// always in mail the system sends (hub review T6; KVKK notice s3, §4).
func TestCountsTellOfPeopleInASmallSendAndInSystemMail(t *testing.T) {
	for _, tt := range []struct {
		system  bool
		tracked int
		want    bool
	}{
		{false, 0, true},
		{false, 1, true},
		{false, tracking.TotalsMinRecipients - 1, true},
		{false, tracking.TotalsMinRecipients, false},
		{false, 500, false},
		{true, 1, true},
		{true, 500, true},
	} {
		got := countsTellOfPeople(database.TrackingTaskAccessRow{SystemLane: tt.system}, &TrackingAggregate{TrackedRecipients: tt.tracked})
		if got != tt.want {
			t.Errorf("system %v, %d tracked: %v, want %v", tt.system, tt.tracked, got, tt.want)
		}
	}
}
