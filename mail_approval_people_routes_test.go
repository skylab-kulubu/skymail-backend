package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// Mail onayı to several people (Yusuf, 2026-09-24; ticket 21): a request
// goes to a list or to 1..100 people; the approver decides it once, and each
// person gets a send of their own, as POST /mail_tasks/single sends to one.

var (
	ayseKaya    = approvalRecipient{Email: "ayse@example.com", FullName: "Ayşe Kaya"}
	mehmetDemir = approvalRecipient{Email: "mehmet@example.com", FullName: "Mehmet Demir"}
	// Someone the submitter knows only by address.
	adiYok = approvalRecipient{Email: "zeynep@example.com", FullName: ""}
)

// A GECEKODU announcement to people, as the compose form fills it for its
// "kişiler" audience.
func (w *approvalWorld) peopleSend(people ...approvalRecipient) map[string]any {
	send := w.listSend()
	delete(send, "mail_list_id")
	send["recipients"] = people
	return send
}

// A send to several people is one request. Nothing goes out; the preview is
// the first person's mail and says so; the approvers hear how many it goes to.
func TestSubmittingASendToSeveralPeople(t *testing.T) {
	w := newApprovalWorld(t)

	answer := w.submit("elif", w.peopleSend(approvalRecipient{Email: "ayse@example.com", FullName: "  Ayşe Kaya "}, mehmetDemir, adiYok))

	people := []approvalRecipient{ayseKaya, mehmetDemir, adiYok}
	if !reflect.DeepEqual(answer.Recipients, people) {
		t.Errorf("recipients = %+v, want %+v", answer.Recipients, people)
	}
	if answer.Audience.Kind != "people" || answer.Audience.MailListID != nil || answer.Audience.RecipientEmail != nil ||
		answer.Audience.RecipientFullName != nil {
		t.Errorf("audience = %+v, want people with no one-person fields", answer.Audience)
	}
	if answer.RecipientCount == nil || *answer.RecipientCount != 3 || answer.TaskID != nil || len(answer.TaskIDs) != 0 {
		t.Errorf("recipient_count %v, task_id %v, task_ids %v", answer.RecipientCount, answer.TaskID, answer.TaskIDs)
	}
	if answer.PreviewRecipient == nil || *answer.PreviewRecipient != ayseKaya || answer.Preview == nil ||
		answer.Preview.RenderedFor != ayseKaya || !strings.Contains(answer.Preview.HTML, "<p>Ayşe Kaya</p>") {
		t.Errorf("preview_recipient %+v, preview %+v", answer.PreviewRecipient, answer.Preview)
	}
	if sent := w.mail.of(w.freeBasic.ID); len(sent) != 0 {
		t.Fatalf("a submission sent %d mails of the template", len(sent))
	}
	vars := w.mail.of(w.requested.ID)[0].variables
	if vars["RecipientCount"] != "3" || vars["AudienceName"] != "Ayşe Kaya <ayse@example.com> ve 2 kişi daha" {
		t.Errorf("approval-requested variables = %v", vars)
	}

	_, read := w.get("fatih", answer.ID)
	var items []approvalAnswer
	w.call("fatih", fiber.MethodGet, "/v1/mail_approvals", nil, &items)
	if !reflect.DeepEqual(read.Recipients, people) || len(items) != 1 || !reflect.DeepEqual(items[0].Recipients, people) {
		t.Errorf("read %+v, listed %+v", read.Recipients, items)
	}
}

// A request to a list names no people and, until it is approved, no sends:
// both are empty lists, never null.
func TestAListRequestNamesNoPeople(t *testing.T) {
	w := newApprovalWorld(t)
	status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", w.listSend(), nil)
	if status != fiber.StatusCreated || !strings.Contains(string(raw), `"recipients":[]`) || !strings.Contains(string(raw), `"task_ids":[]`) ||
		!strings.Contains(string(raw), `"task_id":null`) {
		t.Fatalf("submit = %d %s", status, raw)
	}
	var answer approvalAnswer
	_ = json.Unmarshal(raw, &answer)
	if answer.PreviewRecipient == nil || answer.PreviewRecipient.Email != elif.email || answer.RecipientCount == nil || *answer.RecipientCount != 2 {
		t.Errorf("preview_recipient %+v, recipient_count %v: want the submitter, and the list's two", answer.PreviewRecipient, answer.RecipientCount)
	}
}

// Approving it queues one send per person — the direct send's one-person
// send, by the submitter, with the approved values — in the order submitted;
// each mail greets its own person. The request lists them all.
func TestApprovingSendsEachPersonTheirOwn(t *testing.T) {
	w := newApprovalWorld(t)
	people := []approvalRecipient{ayseKaya, mehmetDemir, adiYok}
	send := w.peopleSend(people...)
	submitted := w.submit("elif", send)

	status, approved, failure := w.act("fatih", submitted.ID, "approve", nil)
	if status != fiber.StatusOK {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	if approved.State != "approved" || len(approved.TaskIDs) != 3 || approved.TaskID == nil || *approved.TaskID != approved.TaskIDs[0] {
		t.Fatalf("approved: %s task_id %v task_ids %v", approved.State, approved.TaskID, approved.TaskIDs)
	}
	if event := approved.History[1]; event.Kind != "approved" || event.TaskID == nil || *event.TaskID != approved.TaskIDs[0] {
		t.Errorf("approved event = %+v", event)
	}

	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 3 {
		t.Fatalf("sends = %+v, want one per person", sent)
	}
	for i, person := range people {
		s := sent[i]
		if s.kind != "single" || s.recipients[0] != person.Email || s.sentBy != elif.sub || s.taskID != approved.TaskIDs[i] ||
			!reflect.DeepEqual(s.variables, send["body_variables"]) {
			t.Errorf("send %d = %+v, want %s's", i, s, person.Email)
		}
		rows := w.queuedRows(approved.TaskIDs[i])
		if len(rows) != 1 || !strings.Contains(*rows[person.Email].BodyHtml, "<p>"+person.FullName+"</p>") {
			t.Errorf("queued for %s = %+v", person.Email, rows)
		}
	}

	status, again, _ := w.act("yusuf", submitted.ID, "approve", nil)
	if status != fiber.StatusOK || !reflect.DeepEqual(again.TaskIDs, approved.TaskIDs) || len(w.mail.of(w.freeBasic.ID)) != 3 {
		t.Errorf("approving again = %d %v, %d sends", status, again.TaskIDs, len(w.mail.of(w.freeBasic.ID)))
	}
	notice := w.mail.of(w.resolved.ID)[0].variables
	if notice["AudienceName"] != "Ayşe Kaya <ayse@example.com> ve 2 kişi daha" {
		t.Errorf("approval-resolved variables = %v", notice)
	}
}

// The sends go out together or not at all: when one person's cannot be
// queued, no one's is, the request is left pending, and approving it again
// sends each person theirs once.
func TestSendsToSeveralPeopleGoOutTogetherOrNotAtAll(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.peopleSend(ayseKaya, mehmetDemir, adiYok))
	w.mail.failSingle = 3

	if status, _, failure := w.act("fatih", submitted.ID, "approve", nil); status != fiber.StatusInternalServerError {
		t.Fatalf("approve with the third send failing = %d %+v, want 500", status, failure)
	}
	sends := func() int64 {
		t.Helper()
		var n int64
		if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks WHERE template_id = $1`, w.freeBasic.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := sends(); n != 0 {
		t.Fatalf("after the failed approval: %d sends, want none", n)
	}
	if _, read := w.get("fatih", submitted.ID); read.State != "pending" || read.kinds() != "submitted" || len(read.TaskIDs) != 0 {
		t.Fatalf("after the failed approval: %s %s %v", read.State, read.kinds(), read.TaskIDs)
	}

	status, approved, failure := w.act("fatih", submitted.ID, "approve", nil)
	if status != fiber.StatusOK || len(approved.TaskIDs) != 3 {
		t.Fatalf("approving again = %d %+v %v", status, failure, approved.TaskIDs)
	}
	if n := sends(); n != 3 {
		t.Fatalf("after approving again: %d sends, want 3", n)
	}
}

// Two approvers approving a request to several people at once open one set of
// sends: the request is locked while they are queued.
func TestTwoApproversAtOnceSendToSeveralPeopleOnce(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.peopleSend(ayseKaya, mehmetDemir, adiYok))
	w.mail.gate = make(chan struct{})
	w.mail.entered = make(chan struct{}, 1)

	type result struct {
		status int
		code   string
	}
	results := make(chan result, 2)
	for _, who := range []string{"fatih", "yusuf"} {
		go func(who string) {
			status, _, failure := w.act(who, submitted.ID, "approve", nil)
			results <- result{status, failure.Code}
		}(who)
	}
	select {
	case <-w.mail.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no approval reached the mailer")
	}
	if first := <-results; first.status != fiber.StatusConflict || first.code != "mail_approval.busy" {
		t.Fatalf("the approval that came second = %+v, want 409 mail_approval.busy", first)
	}
	close(w.mail.gate)
	if second := <-results; second.status != fiber.StatusOK {
		t.Fatalf("the approval that came first = %+v", second)
	}

	if got := emails(w.mail.of(w.freeBasic.ID)); strings.Join(got, ",") != "ayse@example.com,mehmet@example.com,zeynep@example.com" {
		t.Fatalf("sends went to %v, want each person once", got)
	}
	if _, read := w.get("elif", submitted.ID); read.kinds() != "submitted,approved" || len(read.TaskIDs) != 3 {
		t.Fatalf("after two approvals: %s %v", read.kinds(), read.TaskIDs)
	}
}

// Who a request goes to is checked before it is kept: one audience, 1..100
// people, each a real address, each once — however its case is written.
func TestPeopleAreCheckedBeforeARequestIsKept(t *testing.T) {
	w := newApprovalWorld(t)

	crowd := func(n int) []approvalRecipient {
		people := make([]approvalRecipient, n)
		for i := range people {
			people[i] = approvalRecipient{Email: fmt.Sprintf("uye%03d@example.com", i+1)}
		}
		return people
	}
	withList := w.peopleSend(ayseKaya)
	withList["mail_list_id"] = w.list.ID
	withOldField := w.peopleSend(ayseKaya)
	withOldField["recipient_email"] = "mehmet@example.com"

	for name, tc := range map[string]struct {
		send  map[string]any
		field string
		code  string
	}{
		"no one":                        {w.peopleSend(), "mail_list_id", "exactly_one_of"},
		"a list and people":             {withList, "mail_list_id", "exactly_one_of"},
		"people and the one-person one": {withOldField, "mail_list_id", "exactly_one_of"},
		"a malformed address":           {w.peopleSend(ayseKaya, approvalRecipient{Email: "mehmet"}), "recipients[1].email", "invalid_email"},
		"no address":                    {w.peopleSend(approvalRecipient{FullName: "Adı Var"}), "recipients[0].email", "required"},
		"an address twice":              {w.peopleSend(ayseKaya, mehmetDemir, approvalRecipient{Email: "AYSE@Example.com"}), "recipients[2].email", "duplicate"},
		"101 people":                    {w.peopleSend(crowd(101)...), "recipients", "max_length"},
	} {
		t.Run(name, func(t *testing.T) {
			var failure struct {
				Code   string `json:"code"`
				Params struct {
					Errors []struct {
						Field  string         `json:"field"`
						Code   string         `json:"code"`
						Params map[string]any `json:"params"`
					} `json:"errors"`
				} `json:"params"`
			}
			status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", tc.send, &failure)
			if status != fiber.StatusBadRequest || failure.Code != "validation.error" || len(failure.Params.Errors) != 1 ||
				failure.Params.Errors[0].Field != tc.field || failure.Params.Errors[0].Code != tc.code {
				t.Fatalf("submit = %d %s, want 400 %s on %s", status, raw, tc.code, tc.field)
			}
			if tc.code == "duplicate" && failure.Params.Errors[0].Params["first"] != "recipients[0].email" {
				t.Errorf("a duplicate names %v, want the first of it", failure.Params.Errors[0].Params)
			}
		})
	}
	var kept int64
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_approvals`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 || len(w.mail.sent) != 0 {
		t.Fatalf("refused submissions left %d requests and %d mails", kept, len(w.mail.sent))
	}

	hundred := w.submit("elif", w.peopleSend(crowd(100)...))
	if len(hundred.Recipients) != 100 || *hundred.RecipientCount != 100 {
		t.Errorf("a request to 100 people keeps %d", len(hundred.Recipients))
	}
}

// Until the screens send recipients (ticket 22), recipient_email and
// recipient_full_name still submit a send to one person, and a request to
// one person still answers with them — and with task_id, its one send.
func TestTheOnePersonFieldsStillWork(t *testing.T) {
	w := newApprovalWorld(t)

	send := w.listSend()
	delete(send, "mail_list_id")
	send["recipient_email"] = "konusmaci@example.com"
	send["recipient_full_name"] = "Konuşmacı"
	byOldFields := w.submit("elif", send)
	byRecipients := w.submit("elif", w.peopleSend(approvalRecipient{Email: "konusmaci@example.com", FullName: "Konuşmacı"}))

	konusmaci := approvalRecipient{Email: "konusmaci@example.com", FullName: "Konuşmacı"}
	for name, answer := range map[string]approvalAnswer{"recipient_email": byOldFields, "recipients": byRecipients} {
		if !reflect.DeepEqual(answer.Recipients, []approvalRecipient{konusmaci}) || answer.Audience.Kind != "single" ||
			answer.Audience.RecipientEmail == nil || *answer.Audience.RecipientEmail != konusmaci.Email ||
			answer.Audience.RecipientFullName == nil || *answer.Audience.RecipientFullName != konusmaci.FullName {
			t.Errorf("submitted with %s: recipients %+v, audience %+v", name, answer.Recipients, answer.Audience)
		}
	}
	vars := w.mail.of(w.requested.ID)[0].variables
	if vars["AudienceName"] != "Konuşmacı <konusmaci@example.com>" || vars["RecipientCount"] != "1" {
		t.Errorf("approval-requested variables = %v", vars)
	}

	status, approved, failure := w.act("fatih", byOldFields.ID, "approve", nil)
	if status != fiber.StatusOK || len(approved.TaskIDs) != 1 || approved.TaskID == nil || *approved.TaskID != approved.TaskIDs[0] {
		t.Fatalf("approve = %d %+v: task_id %v task_ids %v", status, failure, approved.TaskID, approved.TaskIDs)
	}
	if sent := w.mail.of(w.freeBasic.ID); len(sent) != 1 || sent[0].kind != "single" || sent[0].taskID != *approved.TaskID {
		t.Fatalf("sends = %+v", sent)
	}
}

// A resubmission may change who a request goes to, from a list to people and
// back; the change is recorded, and an approval sends to whom it names now.
func TestResubmittingChangesWhoARequestGoesTo(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())
	w.act("fatih", submitted.ID, "reject", map[string]any{"reason": "Yalnız konuşmacılara gitsin."})

	status, resubmitted, failure := w.act("elif", submitted.ID, "resubmit", w.peopleSend(ayseKaya, mehmetDemir))
	if status != fiber.StatusOK || !reflect.DeepEqual(resubmitted.Recipients, []approvalRecipient{ayseKaya, mehmetDemir}) ||
		resubmitted.Audience.Kind != "people" {
		t.Fatalf("resubmit = %d %+v: %+v %+v", status, failure, resubmitted.Recipients, resubmitted.Audience)
	}
	change := resubmitted.History[2].Changes[0]
	var after struct {
		Recipients []approvalRecipient `json:"recipients"`
	}
	if change.Field != "audience" || !strings.Contains(string(change.Before), w.list.ID.String()) ||
		json.Unmarshal(change.After, &after) != nil || !reflect.DeepEqual(after.Recipients, []approvalRecipient{ayseKaya, mehmetDemir}) {
		t.Errorf("audience change = %s → %s", change.Before, change.After)
	}

	w.act("fatih", submitted.ID, "reject", map[string]any{"reason": "Mehmet çıksın."})
	status, _, failure = w.act("elif", submitted.ID, "resubmit", w.peopleSend(ayseKaya))
	if status != fiber.StatusOK {
		t.Fatalf("resubmit to one = %d %+v", status, failure)
	}
	if status, approved, failure := w.act("fatih", submitted.ID, "approve", nil); status != fiber.StatusOK || len(approved.TaskIDs) != 1 {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	if got := emails(w.mail.of(w.freeBasic.ID)); !reflect.DeepEqual(got, []string{"ayse@example.com"}) {
		t.Fatalf("sent to %v, want only the person the request names now", got)
	}
}
