package handlers

import (
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// An approved request's opens and clicks are summed over its tracked sends
// and withheld by GET /mail_tasks/{id}/tracking's rule (hub ticket
// ekip-postasi 09; ADR-0074 decision 9, addendum 2026-11-08): the sum of
// fewer than 10 tracked recipients, or of a send in the system lane, from one
// who may not read the sends' people; and the small sends among large ones,
// which a reader of the large ones' counts would subtract from the sum.
func TestApprovalTrackingSumsTheSendsAndWithholdsWhatTellsOfPeople(t *testing.T) {
	const submitter = "6d3c3a52-1f1b-4c1e-9d6a-1d0c9b7e2a10"
	plain := contentCaller{sub: submitter, roles: []string{"skymail:access"}}
	role := contentCaller{sub: "0b8a0f43-6f7e-4d3a-8f2b-5c1d2e3f4a50", roles: []string{"skymail:access", TrackingReadRole}}
	send := func(tracked, opened, clicked int) trackedSend {
		return trackedSend{
			task:   database.TrackingTaskAccessRow{SentBy: submitter},
			report: &TrackingAggregate{TrackedRecipients: tracked, UniqueOpens: opened + 1, UniqueLikelyHumanOpens: opened, UniqueClickers: clicked},
		}
	}
	people := func(n int) []trackedSend {
		out := make([]trackedSend, n)
		for i := range out {
			out[i] = send(1, i%2, i%3/2)
		}
		return out
	}
	system := send(40, 20, 5)
	system.task.SystemLane = true
	gone := send(1, 0, 0)
	gone.report = nil

	for _, tt := range []struct {
		name         string
		caller       contentCaller
		senderSees   bool
		sends        []trackedSend
		want         *MailApprovalTracking
		wantWithheld bool
	}{
		{"no tracked send", plain, false, nil, nil, false},
		{"one list send of 12", plain, false, []trackedSend{send(12, 7, 3)}, &MailApprovalTracking{12, 7, 3}, false},
		{"one list send of 9", plain, false, []trackedSend{send(9, 7, 3)}, nil, true},
		{"one list send of 9, tracking role", role, false, []trackedSend{send(9, 7, 3)}, &MailApprovalTracking{9, 7, 3}, false},
		{"one list send of 9, switch on", plain, true, []trackedSend{send(9, 7, 3)}, &MailApprovalTracking{9, 7, 3}, false},
		{"12 people, one send each", plain, false, people(12), &MailApprovalTracking{12, 6, 4}, false},
		{"9 people, one send each", plain, false, people(9), nil, true},
		{"Teams of 20 and 3: the 3 would be read off the sum", plain, false, []trackedSend{send(20, 8, 2), send(3, 1, 1)}, nil, true},
		{"Teams of 20 and 3, tracking role", role, false, []trackedSend{send(20, 8, 2), send(3, 1, 1)}, &MailApprovalTracking{23, 9, 3}, false},
		{"Teams of 20, 5 and 5: the small ones add up to 10", plain, false, []trackedSend{send(20, 8, 2), send(5, 1, 1), send(5, 2, 0)}, &MailApprovalTracking{30, 11, 3}, false},
		{"system lane", plain, true, []trackedSend{system}, nil, true},
		{"system lane, tracking role", role, false, []trackedSend{system}, &MailApprovalTracking{40, 20, 5}, false},
		{"a send's totals are gone", role, false, []trackedSend{send(20, 8, 2), gone}, nil, false},
	} {
		got, withheld := approvalTracking(tt.caller, false, tt.sends, tt.senderSees)
		if withheld != tt.wantWithheld || (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%s: %+v withheld %v, want %+v withheld %v", tt.name, got, withheld, tt.want, tt.wantWithheld)
		}
	}

	// A service's token is no sender, the switch on or not.
	if got, withheld := approvalTracking(plain, true, []trackedSend{send(9, 7, 3)}, true); got != nil || !withheld {
		t.Errorf("service: %+v withheld %v, want withheld", got, withheld)
	}
}
