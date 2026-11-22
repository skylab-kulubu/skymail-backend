package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
)

// Who opened a masked mail (hub ticket skymail-frontend-rewrite 32; Yusuf,
// 2026-11-22): GET /v1/mail_tasks/{id}/content-views takes the audit role
// alone. The roles that open the content — system-mails:read, mails:write —
// and mails:read and the tracking role do not read it: those audited do not
// read their own record (Purview's Audit Reader, NIST SP 800-53 AU-9(4)).
// Without skymail:access nothing of /v1 answers.
func TestContentViewsTakeTheAuditRoleAlone(t *testing.T) {
	w := newApprovalWorld(t)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	send := w.listSend()
	if status, raw := w.call("yusuf", "POST", "/v1/mail_tasks", send, &created); status != 201 {
		t.Fatalf("create: %d %s", status, raw)
	}
	approvalPeople["denetci"] = approvalPerson{sub: uuid.NewString(), roles: []string{"skymail:access", handlers.AuditReadRole}}
	approvalPeople["icerik_acan"] = approvalPerson{sub: uuid.NewString(), roles: []string{"skymail:access", "skymail:mails:read",
		"skymail:mails:write", handlers.SystemMailsReadRole, handlers.TrackingReadRole}}
	approvalPeople["koltuksuz"] = approvalPerson{sub: uuid.NewString(), roles: []string{handlers.AuditReadRole}}
	defer delete(approvalPeople, "denetci")
	defer delete(approvalPeople, "icerik_acan")
	defer delete(approvalPeople, "koltuksuz")
	path := "/v1/mail_tasks/" + created.ID.String() + "/content-views"

	var views []handlers.SentContentView
	if status, raw := w.call("denetci", "GET", path, nil, &views); status != 200 || len(views) != 0 {
		t.Fatalf("the audit role: %d %s, want 200 and no openings", status, raw)
	}
	for _, who := range []string{"icerik_acan", "yusuf", "baska", "koltuksuz"} {
		if status, raw := w.call(who, "GET", path, nil, nil); status != 403 {
			t.Errorf("%s: %d %s, want 403", who, status, raw)
		}
	}
	if status, raw := w.call("denetci", "GET", "/v1/mail_tasks/"+uuid.NewString()+"/content-views", nil, nil); status != 404 {
		t.Errorf("an unknown send: %d %s, want 404", status, raw)
	}
	if status, raw := w.call("denetci", "GET", "/v1/mail_tasks/bozuk/content-views", nil, nil); status != 400 {
		t.Errorf("a malformed id: %d %s, want 400", status, raw)
	}
}
