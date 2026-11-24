package handlers

import (
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// A send's counts tell of people only in mail the system sends (KVKK notice
// s3, §4); a person's send is shown whatever its number of tracked
// recipients (Yusuf, 2026-11-24, K6: the threshold of 10 no longer limits
// who sees them).
func TestCountsTellOfPeopleOnlyInSystemMail(t *testing.T) {
	if countsTellOfPeople(database.TrackingTaskAccessRow{SystemLane: false}) {
		t.Error("a person's send tells of people")
	}
	if !countsTellOfPeople(database.TrackingTaskAccessRow{SystemLane: true}) {
		t.Error("system mail does not tell of people")
	}
}
