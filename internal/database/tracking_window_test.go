package database

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// Every window on tracking events in query.sql — the writers', the
// reports' — is tracking.EventRetentionDays, the retention run's: a year
// (Yusuf, 2026-10-19). A query cannot read the Go constant, so this keeps
// the two from drifting apart.
func TestTrackingQueriesUseTheEventRetention(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "query.sql"))
	if err != nil {
		t.Fatal(err)
	}
	days := regexp.MustCompile(`interval '(\d+) days'`)
	windows := 0
	for _, block := range strings.Split(string(raw), "-- name: ")[1:] {
		name := strings.Fields(block)[0]
		if !strings.Contains(name, "Tracking") {
			continue
		}
		for _, match := range days.FindAllStringSubmatch(block, -1) {
			windows++
			if n, _ := strconv.Atoi(match[1]); n != tracking.EventRetentionDays {
				t.Errorf("%s: interval '%s days', want %d", name, match[1], tracking.EventRetentionDays)
			}
		}
	}
	if windows < 5 {
		t.Errorf("%d tracking windows found, want the writers' and the reports' (5)", windows)
	}
}
