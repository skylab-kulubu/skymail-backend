package handlers

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

func TestFormSourceCanonicalConsumeHashBindsTargetContentAndTracking(t *testing.T) {
	id := uuid.New()
	a, err := formSourceRequestHash("approval", id, []byte(`{"a":1,"b":{"z":2,"x":3}}`), false, mailer.SendPurpose{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := formSourceRequestHash("approval", id, []byte(`{ "b":{"x":3,"z":2},"a":1 }`), false, mailer.SendPurpose{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("equivalent objects do not retry the same target")
	}
	for _, other := range []struct {
		kind     string
		body     []byte
		tracking bool
	}{{"direct", []byte(`{"a":1,"b":{"x":3,"z":2}}`), false}, {"approval", []byte(`{"a":2}`), false}, {"approval", []byte(`{"a":1,"b":{"x":3,"z":2}}`), true}} {
		h, err := formSourceRequestHash(other.kind, id, other.body, other.tracking, mailer.SendPurpose{})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(a, h) {
			t.Fatal("different consume request accepted as retry")
		}
	}
}

func TestFormSourceLimitDoesNotRaiseTheManualApprovalLimit(t *testing.T) {
	people := make([]requests.MailApprovalRecipient, 101)
	for i := range people {
		people[i] = requests.MailApprovalRecipient{Email: fmt.Sprintf("recipient-%d@example.invalid", i)}
	}
	source := requests.CreateFormMailSource{OwnerSub: uuid.New(), FormID: uuid.New(), FormTitle: "Form", Recipients: people, ResponseCount: 101}
	if _, err := validateFormSource(source, 101); err != nil {
		t.Fatalf("101 source recipients: %v", err)
	}
	if _, err := validateFormSource(source, 100); err == nil {
		t.Fatal("configured source limit did not hold")
	}
	if err := validator.NewStructValidator().Validate(requests.SubmitMailApproval{TemplateID: uuid.New(), Recipients: people}); err == nil {
		t.Fatal("manual 101 limit was removed")
	}
	source.Recipients[100].Email = source.Recipients[0].Email
	if _, err := validateFormSource(source, 101); err == nil {
		t.Fatal("duplicate source recipients accepted")
	}
}
