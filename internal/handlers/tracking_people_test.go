package handlers

import (
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// Who reads a send's people (Yusuf, 2026-11-08; ADR-0074 decision 9,
// addendum 2026-11-08): skymail:mails:tracking:read always; the send's own
// sender only while SKYMAIL_SENDER_SEES_RECIPIENTS is on, and never for a
// service or for mail the system sends.
func TestTrackingPeopleReadableClosesTheSenderUnlessTheSwitchIsOn(t *testing.T) {
	const sender = "6d3c3a52-1f1b-4c1e-9d6a-1d0c9b7e2a10"
	const other = "0b8a0f43-6f7e-4d3a-8f2b-5c1d2e3f4a50"
	person := database.TrackingTaskAccessRow{SentBy: sender}
	system := database.TrackingTaskAccessRow{SentBy: sender, SystemLane: true}
	role := []string{"skymail:access", TrackingReadRole}
	plain := []string{"skymail:access", "skymail:mails:read"}
	for _, tt := range []struct {
		name       string
		caller     contentCaller
		service    bool
		task       database.TrackingTaskAccessRow
		senderSees bool
		want       bool
	}{
		{"sender, switch off (default)", contentCaller{sub: sender, roles: plain}, false, person, false, false},
		{"sender, switch on", contentCaller{sub: sender, roles: plain}, false, person, true, true},
		{"sender with the role, switch off", contentCaller{sub: sender, roles: role}, false, person, false, true},
		{"role holder, switch off", contentCaller{sub: other, roles: role}, false, person, false, true},
		{"role holder, switch on", contentCaller{sub: other, roles: role}, false, person, true, true},
		{"another person, switch on", contentCaller{sub: other, roles: plain}, false, person, true, false},
		{"no subject, switch on", contentCaller{roles: plain}, false, database.TrackingTaskAccessRow{}, true, false},
		{"service sender, switch on", contentCaller{sub: sender, roles: plain}, true, person, true, false},
		{"system mail's named sender, switch on", contentCaller{sub: sender, roles: plain}, false, system, true, false},
		{"system mail, role holder", contentCaller{sub: other, roles: role}, false, system, false, true},
	} {
		if got := trackingPeopleReadable(tt.caller, tt.service, tt.task, tt.senderSees); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSenderSeesRecipientsFromEnv(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "  ": false, "off": false, "OFF": false, " on ": true, "on": true, "On": true} {
		got, err := SenderSeesRecipientsFromEnv(func(name string) string {
			if name != SenderSeesRecipientsEnv {
				t.Fatalf("read %s", name)
			}
			return raw
		})
		if err != nil || got != want {
			t.Errorf("%q = %v (%v), want %v", raw, got, err, want)
		}
	}
	if SenderSeesRecipientsEnv != "SKYMAIL_SENDER_SEES_RECIPIENTS" {
		t.Errorf("SenderSeesRecipientsEnv = %s", SenderSeesRecipientsEnv)
	}
	for _, raw := range []string{"true", "1", "yes", "enabled", "of"} {
		if got, err := SenderSeesRecipientsFromEnv(func(string) string { return raw }); err == nil {
			t.Errorf("%q = %v, want an error that stops startup", raw, got)
		}
	}
}
