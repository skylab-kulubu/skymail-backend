package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Account erasure (ADR-0051; spec §3.1). Deniz Yılmaz is erased: a Keycloak
// subject and two addresses, one of them stored with capitals. Ayşe Kaya is
// in the same lists, sends and requests and keeps everything, and so does
// every row that does not name Deniz — but for another Deniz Yılmaz, whose
// mail names the same full name: that body goes too, the documented cost of
// searching by name.
const (
	erasedSub      = "3f1c9d70-5b8e-4c1a-9d2e-0a1b2c3d4e5f"
	erasedSchool   = "deniz.yilmaz@std.yildiz.edu.tr"
	erasedPersonal = "deniz@example.com"
	otherSub       = "b7d2e4f0-2a6c-4e8b-8f1d-5e6f7a8b9c0d"
	approverSub    = "c9e8f7a6-1b2c-4d3e-9f40-a1b2c3d4e5f6"
)

var erasedEmails = []string{erasedSchool, erasedPersonal}

// erasureFixture is one of everything SkyMail keeps about people. Ids are
// fixed so a row can be named in the assertions.
const erasureFixture = `
INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
VALUES ('10000000-0000-4000-8000-000000000001', 'Duyuru', 'Duyuru', '<p>{{.FullName}}</p>', '{{.FullName}}', '');
INSERT INTO template_versions (id, template_id, seq, name, subject, html_source, main_mode, html_content,
                               plain_text_content, author_kind, author_sub, author_name, created_at, published_at)
VALUES ('11000000-0000-4000-8000-000000000001', '10000000-0000-4000-8000-000000000001', 1, 'Duyuru', 'Duyuru',
        '<p>{{.FullName}}</p>', 'html', '<p>{{.FullName}}</p>', '{{.FullName}}', 'operator',
        '` + erasedSub + `', 'Deniz Yılmaz', '2026-09-01T10:00:00Z', '2026-09-01T10:00:00Z'),
       ('11000000-0000-4000-8000-000000000002', '10000000-0000-4000-8000-000000000001', 2, 'Duyuru', 'Duyuru 2',
        '<p>{{.FullName}}</p>', 'html', '<p>{{.FullName}}</p>', '{{.FullName}}', 'operator',
        '` + otherSub + `', 'Ayşe Kaya', '2026-09-02T10:00:00Z', NULL);
UPDATE templates SET published_version_id = '11000000-0000-4000-8000-000000000001'
WHERE id = '10000000-0000-4000-8000-000000000001';
INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content, archived_at, archived_by)
VALUES ('10000000-0000-4000-8000-000000000002', 'Eski', 'Eski', '<p>x</p>', 'x', '', '2026-09-03T10:00:00Z', '` + erasedSub + `'),
       ('10000000-0000-4000-8000-000000000003', 'Eski 2', 'Eski 2', '<p>x</p>', 'x', '', '2026-09-03T10:00:00Z', '` + otherSub + `');

INSERT INTO mailing_lists (id, name) VALUES
    ('20000000-0000-4000-8000-000000000001', 'Tüm üyeler'),
    ('20000000-0000-4000-8000-000000000002', 'Okul adresleri');
INSERT INTO mailing_lists (id, name, archived_at, archived_by) VALUES
    ('20000000-0000-4000-8000-000000000003', 'Eski liste', '2026-09-03T10:00:00Z', '` + erasedSub + `'),
    ('20000000-0000-4000-8000-000000000004', 'Eski liste 2', '2026-09-03T10:00:00Z', '` + otherSub + `');

INSERT INTO recipients (id, full_name, email) VALUES
    ('30000000-0000-4000-8000-000000000001', 'Deniz Yılmaz', 'Deniz@Example.com'),
    ('30000000-0000-4000-8000-000000000002', 'Deniz', '` + erasedSchool + `'),
    ('30000000-0000-4000-8000-000000000003', 'Ayşe Kaya', 'ayse@example.com'),
    ('30000000-0000-4000-8000-000000000004', 'Deniz Yılmaz', 'd.yilmaz@example.org');
INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) VALUES
    ('20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000001'),
    ('20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000003'),
    ('20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000004'),
    ('20000000-0000-4000-8000-000000000002', '30000000-0000-4000-8000-000000000002'),
    ('20000000-0000-4000-8000-000000000002', '30000000-0000-4000-8000-000000000003');

INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables) VALUES
    -- Deniz's send to a list: the sender is replaced, the values stay.
    ('40000000-0000-4000-8000-00000000000a', '` + erasedSub + `', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{"Heading": "Bahar şenliği"}'),
    -- Keycloak's reset mail to Deniz alone, and one that bounced.
    ('40000000-0000-4000-8000-00000000000b', 'service-account-keycloak-mailer', '10000000-0000-4000-8000-000000000001', NULL, '{"link": "https://e.yildizskylab.com/reset?key=abc", "firstName": "Deniz"}'),
    ('40000000-0000-4000-8000-0000000000b2', 'service-account-keycloak-mailer', '10000000-0000-4000-8000-000000000001', NULL, '{"link": "https://e.yildizskylab.com/verify?key=def", "firstName": "Deniz"}'),
    -- Sends to Ayşe whose values name Deniz, by name or by address.
    ('40000000-0000-4000-8000-00000000000c', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{"Note": "deniz yılmaz ile görüşme"}'),
    ('40000000-0000-4000-8000-00000000000d', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{"Contact": "DENIZ@EXAMPLE.COM"}'),
    -- A single word is never searched for.
    ('40000000-0000-4000-8000-00000000000e', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{"Place": "Deniz kıyısı"}'),
    -- A list send still queued for Deniz and for Ayşe.
    ('40000000-0000-4000-8000-00000000000f', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{"Heading": "Yeni dönem"}'),
    -- A send to the other Deniz Yılmaz.
    ('40000000-0000-4000-8000-000000000010', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{}'),
    -- Mail onayı sends.
    ('40000000-0000-4000-8000-000000000021', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{}'),
    ('40000000-0000-4000-8000-000000000022', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{}'),
    ('40000000-0000-4000-8000-000000000031', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{}'),
    ('40000000-0000-4000-8000-000000000032', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{}'),
    ('40000000-0000-4000-8000-000000000033', '` + otherSub + `', '10000000-0000-4000-8000-000000000001', NULL, '{}');

INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status, error) VALUES
    ('50000000-0000-4000-8000-0000000000a1', '40000000-0000-4000-8000-00000000000a', 'Deniz Yılmaz', 'Deniz@Example.com', 'Bahar şenliği', 'Merhaba Deniz Yılmaz', '<p>Merhaba Deniz Yılmaz</p>', 'sent', NULL),
    ('50000000-0000-4000-8000-0000000000a2', '40000000-0000-4000-8000-00000000000a', 'Ayşe Kaya', 'ayse@example.com', 'Bahar şenliği', 'Merhaba Ayşe Kaya', '<p>Merhaba Ayşe Kaya</p>', 'sent', NULL),
    ('50000000-0000-4000-8000-0000000000b1', '40000000-0000-4000-8000-00000000000b', 'Deniz', '` + erasedSchool + `', 'Parola sıfırlama', 'Bağlantın: https://e.yildizskylab.com/reset?key=abc', '<a href="https://e.yildizskylab.com/reset?key=abc">Sıfırla</a>', 'sent', NULL),
    ('50000000-0000-4000-8000-0000000000b2', '40000000-0000-4000-8000-0000000000b2', 'Deniz', '` + erasedSchool + `', 'Doğrulama', 'Bağlantın', '<p>Bağlantın</p>', 'failed', '550 5.1.1 <` + erasedSchool + `>: unknown user'),
    ('50000000-0000-4000-8000-0000000000c1', '40000000-0000-4000-8000-00000000000c', 'Ayşe Kaya', 'ayse@example.com', 'Not', 'Deniz Yılmaz ile görüşme', '<p>Deniz  Yılmaz ile görüşme</p>', 'sent', NULL),
    ('50000000-0000-4000-8000-0000000000d1', '40000000-0000-4000-8000-00000000000d', 'Ayşe Kaya', 'ayse@example.com', 'İletişim', 'Bilgi için yaz', '<a href="mailto:DENIZ@EXAMPLE.COM">yaz</a>', 'failed', 'timeout'),
    ('50000000-0000-4000-8000-0000000000e1', '40000000-0000-4000-8000-00000000000e', 'Ayşe Kaya', 'ayse@example.com', 'Buluşma', 'Deniz kıyısında buluşalım', '<p>Deniz kıyısında buluşalım</p>', 'sent', NULL),
    ('50000000-0000-4000-8000-0000000000f1', '40000000-0000-4000-8000-00000000000f', 'Deniz Yılmaz', '` + erasedPersonal + `', 'Yeni dönem', 'Merhaba Deniz Yılmaz', '<p>Merhaba Deniz Yılmaz</p>', 'pending', NULL),
    ('50000000-0000-4000-8000-0000000000f2', '40000000-0000-4000-8000-00000000000f', 'Ayşe Kaya', 'ayse@example.com', 'Yeni dönem', 'Merhaba Ayşe Kaya', '<p>Merhaba Ayşe Kaya</p>', 'pending', NULL),
    ('50000000-0000-4000-8000-000000000101', '40000000-0000-4000-8000-000000000010', 'Deniz Yılmaz', 'd.yilmaz@example.org', 'Selam', 'Merhaba Deniz Yılmaz', '<p>Merhaba Deniz Yılmaz</p>', 'sent', NULL);

INSERT INTO mail_approvals (id, submitter_sub, submitter_name, submitter_email, submitter_email_unverified, state,
                            template_id, template_version_id, mail_list_id, body_variables, created_at,
                            submitted_at, deadline_at, updated_at) VALUES
    -- Deniz asked to send to a list.
    ('60000000-0000-4000-8000-000000000001', '` + erasedSub + `', 'Deniz Yılmaz', '` + erasedSchool + `', false, 'pending',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001',
     '{"Heading": "x"}', '2026-09-20T10:00:00Z', '2026-09-20T10:00:00Z', '2026-09-27T10:00:00Z', '2026-09-20T10:00:00Z'),
    -- Ayşe asked to send to Deniz alone: rejected.
    ('60000000-0000-4000-8000-000000000002', '` + otherSub + `', 'Ayşe Kaya', 'ayse@example.com', false, 'rejected',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', NULL,
     '{"Heading": "y"}', '2026-09-20T10:00:00Z', '2026-09-20T10:00:00Z', '2026-09-27T10:00:00Z', '2026-09-20T11:00:00Z'),
    -- Ayşe asked to send to Deniz and herself: pending.
    ('60000000-0000-4000-8000-000000000003', '` + otherSub + `', 'Ayşe Kaya', 'ayse@example.com', false, 'pending',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', NULL,
     '{"Heading": "z"}', '2026-09-20T10:00:00Z', '2026-09-21T10:00:00Z', '2026-09-28T10:00:00Z', '2026-09-21T10:00:00Z'),
    -- Approved and sent to Deniz and Ayşe, one send each.
    ('60000000-0000-4000-8000-000000000004', '` + otherSub + `', 'Ayşe Kaya', 'ayse@example.com', false, 'approved',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', NULL,
     '{}', '2026-09-20T10:00:00Z', '2026-09-20T10:00:00Z', '2026-09-27T10:00:00Z', '2026-09-20T12:00:00Z'),
    -- Approved and sent to both of Deniz's addresses and to Ayşe.
    ('60000000-0000-4000-8000-000000000005', '` + otherSub + `', 'Ayşe Kaya', 'ayse@example.com', false, 'approved',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', NULL,
     '{}', '2026-09-20T10:00:00Z', '2026-09-20T10:00:00Z', '2026-09-27T10:00:00Z', '2026-09-20T12:00:00Z'),
    -- Ayşe's request to a list whose values name Deniz.
    ('60000000-0000-4000-8000-000000000006', '` + otherSub + `', 'Ayşe Kaya', 'ayse@example.com', false, 'pending',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001',
     '{"Name": "Deniz Yılmaz"}', '2026-09-20T10:00:00Z', '2026-09-22T10:00:00Z', '2026-09-29T10:00:00Z', '2026-09-22T10:00:00Z'),
    -- Deniz's request whose token carried an unverified address.
    ('60000000-0000-4000-8000-000000000007', '` + erasedSub + `', NULL, NULL, true, 'expired',
     '10000000-0000-4000-8000-000000000001', '11000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001',
     '{"Heading": "w"}', '2026-09-10T10:00:00Z', '2026-09-10T10:00:00Z', '2026-09-17T10:00:00Z', '2026-09-17T10:00:00Z');
INSERT INTO mail_approval_recipients (approval_id, position, email, full_name) VALUES
    ('60000000-0000-4000-8000-000000000002', 1, '` + erasedPersonal + `', 'Deniz Yılmaz'),
    ('60000000-0000-4000-8000-000000000003', 1, 'Deniz.Yilmaz@std.yildiz.edu.tr', 'Deniz Yılmaz'),
    ('60000000-0000-4000-8000-000000000003', 2, 'ayse@example.com', 'Ayşe Kaya'),
    ('60000000-0000-4000-8000-000000000004', 1, '` + erasedPersonal + `', 'Deniz Yılmaz'),
    ('60000000-0000-4000-8000-000000000004', 2, 'ayse@example.com', 'Ayşe Kaya'),
    ('60000000-0000-4000-8000-000000000005', 1, '` + erasedPersonal + `', 'Deniz Yılmaz'),
    ('60000000-0000-4000-8000-000000000005', 2, '` + erasedSchool + `', 'Deniz Yılmaz'),
    ('60000000-0000-4000-8000-000000000005', 3, 'ayse@example.com', 'Ayşe Kaya');
INSERT INTO mail_approval_tasks (approval_id, position, task_id) VALUES
    ('60000000-0000-4000-8000-000000000004', 1, '40000000-0000-4000-8000-000000000021'),
    ('60000000-0000-4000-8000-000000000004', 2, '40000000-0000-4000-8000-000000000022'),
    ('60000000-0000-4000-8000-000000000005', 1, '40000000-0000-4000-8000-000000000031'),
    ('60000000-0000-4000-8000-000000000005', 2, '40000000-0000-4000-8000-000000000032'),
    ('60000000-0000-4000-8000-000000000005', 3, '40000000-0000-4000-8000-000000000033');
INSERT INTO mail_approval_events (id, approval_id, seq, kind, actor_sub, actor_name, note, changes, task_id, created_at) VALUES
    ('70000000-0000-4000-8000-000000000011', '60000000-0000-4000-8000-000000000001', 1, 'submitted', '` + erasedSub + `', 'Deniz Yılmaz', 'İlk gönderim', NULL, NULL, '2026-09-20T10:00:00Z'),
    ('70000000-0000-4000-8000-000000000021', '60000000-0000-4000-8000-000000000002', 1, 'submitted', '` + otherSub + `', 'Ayşe Kaya', NULL, NULL, NULL, '2026-09-20T10:00:00Z'),
    ('70000000-0000-4000-8000-000000000022', '60000000-0000-4000-8000-000000000002', 2, 'rejected', '` + erasedSub + `', 'Deniz Yılmaz', 'Uygun değil', NULL, NULL, '2026-09-20T11:00:00Z'),
    ('70000000-0000-4000-8000-000000000031', '60000000-0000-4000-8000-000000000003', 1, 'submitted', '` + otherSub + `', 'Ayşe Kaya', NULL, NULL, NULL, '2026-09-20T10:00:00Z'),
    ('70000000-0000-4000-8000-000000000032', '60000000-0000-4000-8000-000000000003', 2, 'resubmitted', '` + otherSub + `', 'Ayşe Kaya', NULL,
     '[{"field": "audience", "before": {"recipient_email": "` + erasedPersonal + `", "recipient_full_name": "Deniz Yılmaz"}, "after": {"recipient_email": "ayse@example.com", "recipient_full_name": "Ayşe Kaya"}}]', NULL, '2026-09-21T10:00:00Z'),
    ('70000000-0000-4000-8000-000000000033', '60000000-0000-4000-8000-000000000003', 3, 'edited', '` + approverSub + `', 'Fatih Demir', NULL,
     '[{"field": "audience", "before": {"recipients": [{"email": "Deniz.Yilmaz@std.yildiz.edu.tr", "full_name": "Deniz Yılmaz"}]}, "after": {"recipients": []}}]', NULL, '2026-09-21T11:00:00Z'),
    ('70000000-0000-4000-8000-000000000041', '60000000-0000-4000-8000-000000000004', 1, 'approved', '` + approverSub + `', 'Fatih Demir', 'tamam', NULL, '40000000-0000-4000-8000-000000000021', '2026-09-20T12:00:00Z'),
    ('70000000-0000-4000-8000-000000000061', '60000000-0000-4000-8000-000000000006', 1, 'rejected', '` + approverSub + `', 'Fatih Demir', 'deniz@example.com adresine gitmesin', NULL, NULL, '2026-09-22T11:00:00Z'),
    ('70000000-0000-4000-8000-000000000062', '60000000-0000-4000-8000-000000000006', 2, 'resubmitted', '` + otherSub + `', 'Ayşe Kaya', NULL,
     '[{"field": "variable", "name": "Place", "before": "Deniz kıyısı", "after": "Göl"}]', NULL, '2026-09-22T12:00:00Z'),
    ('70000000-0000-4000-8000-000000000071', '60000000-0000-4000-8000-000000000007', 1, 'submitted', '` + erasedSub + `', 'Deniz Yılmaz', NULL, NULL, NULL, '2026-09-10T10:00:00Z'),
    ('70000000-0000-4000-8000-000000000072', '60000000-0000-4000-8000-000000000007', 2, 'expired', NULL, NULL, NULL, NULL, NULL, '2026-09-17T10:00:00Z');
`

// erasureTables names every table erasure may touch and the key each row is
// told apart by.
var erasureTables = map[string]string{
	"recipients":               "id::text",
	"mailing_list_recipients":  "mail_list_id::text || '/' || recipient_id::text",
	"templates":                "id::text",
	"mailing_lists":            "id::text",
	"template_versions":        "id::text",
	"mail_tasks":               "id::text",
	"mail_queue":               "id::text",
	"mail_approvals":           "id::text",
	"mail_approval_recipients": "approval_id::text || '/' || position::text",
	"mail_approval_tasks":      "approval_id::text || '/' || position::text",
	"mail_approval_events":     "id::text",
}

type tableRows map[string]map[string]string

func snapshotErasureTables(t *testing.T, store *Store) tableRows {
	t.Helper()
	snapshot := tableRows{}
	for table, key := range erasureTables {
		rows, err := store.Conn.Query(context.Background(),
			`SELECT `+key+`, row_to_json(x)::text FROM `+table+` x`)
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		snapshot[table] = map[string]string{}
		for rows.Next() {
			var id, row string
			if err := rows.Scan(&id, &row); err != nil {
				t.Fatal(err)
			}
			snapshot[table][id] = row
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

// assertOnlyChanged fails for every row that changed, appeared or went
// away outside changed[table].
func assertOnlyChanged(t *testing.T, before, after tableRows, changed map[string][]string) {
	t.Helper()
	for table := range erasureTables {
		allowed := map[string]bool{}
		for _, id := range changed[table] {
			allowed[id] = true
		}
		for id, row := range before[table] {
			if allowed[id] {
				continue
			}
			if after[table][id] != row {
				t.Errorf("%s %s changed:\nbefore %s\nafter  %s", table, id, row, after[table][id])
			}
		}
		for id := range after[table] {
			if _, ok := before[table][id]; !ok && !allowed[id] {
				t.Errorf("%s %s appeared: %s", table, id, after[table][id])
			}
		}
	}
}

func erasureStore(t *testing.T) *Store {
	t.Helper()
	store := lifecycleStore(t)
	if _, err := store.Conn.Exec(context.Background(), erasureFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return store
}

func eraseDeniz(requestID uuid.UUID) AccountErasure {
	return AccountErasure{RequestID: requestID, Subject: erasedSub, Emails: erasedEmails}
}

func queryString(t *testing.T, store *Store, sql string, args ...any) *string {
	t.Helper()
	var value *string
	if err := store.Conn.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return value
}

func queryCount(t *testing.T, store *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := store.Conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestEraseAccountRemovesThePersonAndLeavesEveryoneElse(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	before := snapshotErasureTables(t, store)

	receipt, err := store.EraseAccount(ctx, eraseDeniz(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotErasureTables(t, store)

	wantCounts := map[string]int64{
		AccountErasureRecipientsDeleted:         2,
		AccountErasureListMembershipsDeleted:    2,
		AccountErasureQueueRowsDeleted:          1,
		AccountErasureQueueRowsCleared:          2 + 1,
		AccountErasureBodiesCleared:             3,
		AccountErasureVariablesCleared:          4 + 1,
		AccountErasureActorColumnsReplaced:      9,
		AccountErasureNotesCleared:              3,
		AccountErasureChangesCleared:            2,
		AccountErasureApprovalRecipientsRemoved: 5,
		AccountErasureApprovalPlaceholders:      3,
		AccountErasureApprovalSendLinksDeleted:  1,
	}
	if !reflect.DeepEqual(receipt.Counts, wantCounts) {
		t.Errorf("counts = %v\nwant     %v", receipt.Counts, wantCounts)
	}

	assertOnlyChanged(t, before, after, map[string][]string{
		"recipients": {"30000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000002"},
		"mailing_list_recipients": {
			"20000000-0000-4000-8000-000000000001/30000000-0000-4000-8000-000000000001",
			"20000000-0000-4000-8000-000000000002/30000000-0000-4000-8000-000000000002",
		},
		"templates":         {"10000000-0000-4000-8000-000000000002"},
		"mailing_lists":     {"20000000-0000-4000-8000-000000000003"},
		"template_versions": {"11000000-0000-4000-8000-000000000001"},
		"mail_tasks": {
			"40000000-0000-4000-8000-00000000000a", "40000000-0000-4000-8000-00000000000b",
			"40000000-0000-4000-8000-0000000000b2", "40000000-0000-4000-8000-00000000000c",
			"40000000-0000-4000-8000-00000000000d",
		},
		"mail_queue": {
			"50000000-0000-4000-8000-0000000000a1", "50000000-0000-4000-8000-0000000000b1",
			"50000000-0000-4000-8000-0000000000b2", "50000000-0000-4000-8000-0000000000c1",
			"50000000-0000-4000-8000-0000000000d1", "50000000-0000-4000-8000-0000000000f1",
			// The other Deniz Yılmaz: same full name, so the body goes.
			"50000000-0000-4000-8000-000000000101",
		},
		"mail_approvals": {
			"60000000-0000-4000-8000-000000000001", "60000000-0000-4000-8000-000000000006",
			"60000000-0000-4000-8000-000000000007",
		},
		"mail_approval_recipients": {
			"60000000-0000-4000-8000-000000000002/1", "60000000-0000-4000-8000-000000000003/1",
			"60000000-0000-4000-8000-000000000004/1", "60000000-0000-4000-8000-000000000005/1",
			"60000000-0000-4000-8000-000000000005/2",
		},
		"mail_approval_tasks": {"60000000-0000-4000-8000-000000000005/2"},
		"mail_approval_events": {
			"70000000-0000-4000-8000-000000000011", "70000000-0000-4000-8000-000000000022",
			"70000000-0000-4000-8000-000000000032", "70000000-0000-4000-8000-000000000033",
			"70000000-0000-4000-8000-000000000061", "70000000-0000-4000-8000-000000000071",
		},
	})

	t.Run("recipients and list memberships are deleted", func(t *testing.T) {
		if n := queryCount(t, store, `SELECT count(*) FROM recipients WHERE lower(email) = ANY($1)`, erasedEmails); n != 0 {
			t.Errorf("recipients left = %d", n)
		}
		if n := queryCount(t, store, `SELECT count(*) FROM mailing_lists`); n != 4 {
			t.Errorf("mailing lists = %d, want all 4", n)
		}
	})

	t.Run("actor columns become Silinmiş kullanıcı", func(t *testing.T) {
		for _, check := range []struct{ sql, want string }{
			{`SELECT sent_by FROM mail_tasks WHERE id = '40000000-0000-4000-8000-00000000000a'`, DeletedUserSubject},
			{`SELECT archived_by FROM templates WHERE id = '10000000-0000-4000-8000-000000000002'`, DeletedUserSubject},
			{`SELECT archived_by FROM mailing_lists WHERE id = '20000000-0000-4000-8000-000000000003'`, DeletedUserSubject},
			{`SELECT author_sub FROM template_versions WHERE id = '11000000-0000-4000-8000-000000000001'`, DeletedUserSubject},
			{`SELECT author_name FROM template_versions WHERE id = '11000000-0000-4000-8000-000000000001'`, DeletedUserName},
			{`SELECT actor_sub FROM mail_approval_events WHERE id = '70000000-0000-4000-8000-000000000011'`, DeletedUserSubject},
			{`SELECT actor_name FROM mail_approval_events WHERE id = '70000000-0000-4000-8000-000000000071'`, DeletedUserName},
		} {
			if got := queryString(t, store, check.sql); got == nil || *got != check.want {
				t.Errorf("%s = %v, want %q", check.sql, got, check.want)
			}
		}
		// The template's content and the version's content stay: editorial record.
		if got := queryString(t, store, `SELECT html_content FROM template_versions WHERE id = '11000000-0000-4000-8000-000000000001'`); *got != "<p>{{.FullName}}</p>" {
			t.Errorf("version content = %q", *got)
		}
	})

	t.Run("submitter becomes Silinmiş kullanıcı with no address", func(t *testing.T) {
		for _, id := range []string{"60000000-0000-4000-8000-000000000001", "60000000-0000-4000-8000-000000000007"} {
			var sub string
			var name, email *string
			var unverified bool
			if err := store.Conn.QueryRow(ctx, `SELECT submitter_sub, submitter_name, submitter_email, submitter_email_unverified
				FROM mail_approvals WHERE id = $1`, id).Scan(&sub, &name, &email, &unverified); err != nil {
				t.Fatal(err)
			}
			if sub != DeletedUserSubject || name == nil || *name != DeletedUserName || email != nil || unverified {
				t.Errorf("%s: submitter %s %v %v %v", id, sub, name, email, unverified)
			}
		}
	})

	t.Run("queue rows to the person are cleared or deleted, not masked", func(t *testing.T) {
		for _, id := range []string{"50000000-0000-4000-8000-0000000000a1", "50000000-0000-4000-8000-0000000000b1", "50000000-0000-4000-8000-0000000000b2"} {
			var name, email, subject, body string
			var html, failure *string
			if err := store.Conn.QueryRow(ctx, `SELECT recipient_full_name, recipient_email, subject, body, body_html, error
				FROM mail_queue WHERE id = $1`, id).Scan(&name, &email, &subject, &body, &html, &failure); err != nil {
				t.Fatal(err)
			}
			if name != DeletedUserName || email != "" || subject != "" || body != "" || html != nil || failure != nil {
				t.Errorf("%s = %q %q %q %q %v %v", id, name, email, subject, body, html, failure)
			}
		}
		if n := queryCount(t, store, `SELECT count(*) FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000f1'`); n != 0 {
			t.Error("the pending row to the person is still queued")
		}
		// Sent and failed rows are still counted.
		if got := queryString(t, store, `SELECT mail_task_status('40000000-0000-4000-8000-0000000000b2')`); *got != "failed" {
			t.Errorf("status of the bounced send = %s", *got)
		}
	})

	t.Run("bodies naming the person are cleared whole, recipient kept", func(t *testing.T) {
		for _, id := range []string{"50000000-0000-4000-8000-0000000000c1", "50000000-0000-4000-8000-0000000000d1", "50000000-0000-4000-8000-000000000101"} {
			var email, subject, body string
			var html *string
			if err := store.Conn.QueryRow(ctx, `SELECT recipient_email, subject, body, body_html FROM mail_queue WHERE id = $1`, id).
				Scan(&email, &subject, &body, &html); err != nil {
				t.Fatal(err)
			}
			if email == "" || subject != "" || body != "" || html != nil {
				t.Errorf("%s = %q %q %q %v", id, email, subject, body, html)
			}
		}
		if got := queryString(t, store, `SELECT error FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000d1'`); got == nil || *got != "timeout" {
			t.Errorf("another recipient's error = %v, want it kept", got)
		}
	})

	t.Run("variables naming the person, or of a send to them alone, are emptied", func(t *testing.T) {
		for _, id := range []string{"40000000-0000-4000-8000-00000000000b", "40000000-0000-4000-8000-0000000000b2", "40000000-0000-4000-8000-00000000000c", "40000000-0000-4000-8000-00000000000d"} {
			if got := queryString(t, store, `SELECT body_variables::text FROM mail_tasks WHERE id = $1`, id); *got != "{}" {
				t.Errorf("task %s variables = %s", id, *got)
			}
		}
		if got := queryString(t, store, `SELECT body_variables::text FROM mail_approvals WHERE id = '60000000-0000-4000-8000-000000000006'`); *got != "{}" {
			t.Errorf("approval variables = %s", *got)
		}
	})

	t.Run("notes and changes naming the person are cleared; a rejection keeps a reason", func(t *testing.T) {
		for id, want := range map[string]*string{
			"70000000-0000-4000-8000-000000000011": nil,
			"70000000-0000-4000-8000-000000000022": ptrTo(ErasedNote),
			"70000000-0000-4000-8000-000000000061": ptrTo(ErasedNote),
		} {
			got := queryString(t, store, `SELECT note FROM mail_approval_events WHERE id = $1`, id)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("note of %s = %s, want %s", id, shown(got), shown(want))
			}
		}
		for _, id := range []string{"70000000-0000-4000-8000-000000000032", "70000000-0000-4000-8000-000000000033"} {
			if got := queryString(t, store, `SELECT changes::text FROM mail_approval_events WHERE id = $1`, id); got != nil {
				t.Errorf("changes of %s = %s, want none", id, *got)
			}
		}
		// The approver's event on a request to Deniz keeps its actor.
		if got := queryString(t, store, `SELECT actor_sub FROM mail_approval_events WHERE id = '70000000-0000-4000-8000-000000000061'`); *got != approverSub {
			t.Errorf("approver replaced: %s", *got)
		}
	})

	t.Run("a request's people lose the person and keep its constraints", func(t *testing.T) {
		people := func(approval string) []string {
			rows, err := store.Conn.Query(ctx, `SELECT position || ':' || email || ':' || full_name FROM mail_approval_recipients
				WHERE approval_id = $1 ORDER BY position`, approval)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				out = append(out, s)
			}
			return out
		}
		sends := func(approval string) []string {
			rows, err := store.Conn.Query(ctx, `SELECT position || ':' || task_id FROM mail_approval_tasks
				WHERE approval_id = $1 ORDER BY position`, approval)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				out = append(out, s)
			}
			return out
		}
		placeholder := "1:" + DeletedUserEmail + ":" + DeletedUserName
		for approval, want := range map[string][]string{
			// The person was its only one: one placeholder keeps "a list or at least one person".
			"60000000-0000-4000-8000-000000000002": {placeholder},
			"60000000-0000-4000-8000-000000000003": {"2:ayse@example.com:Ayşe Kaya"},
			// Sent, one send per person: the placeholder keeps the position, so task_ids[i] stays recipients[i]'s send.
			"60000000-0000-4000-8000-000000000004": {placeholder, "2:ayse@example.com:Ayşe Kaya"},
			"60000000-0000-4000-8000-000000000005": {placeholder, "3:ayse@example.com:Ayşe Kaya"},
		} {
			if got := people(approval); !reflect.DeepEqual(got, want) {
				t.Errorf("people of %s = %v, want %v", approval, got, want)
			}
		}
		if got, want := sends("60000000-0000-4000-8000-000000000004"), []string{
			"1:40000000-0000-4000-8000-000000000021", "2:40000000-0000-4000-8000-000000000022",
		}; !reflect.DeepEqual(got, want) {
			t.Errorf("sends = %v, want %v", got, want)
		}
		if got, want := sends("60000000-0000-4000-8000-000000000005"), []string{
			"1:40000000-0000-4000-8000-000000000031", "3:40000000-0000-4000-8000-000000000033",
		}; !reflect.DeepEqual(got, want) {
			t.Errorf("sends = %v, want %v", got, want)
		}
		// The send itself stays.
		if n := queryCount(t, store, `SELECT count(*) FROM mail_tasks WHERE id = '40000000-0000-4000-8000-000000000032'`); n != 1 {
			t.Error("the send lost its row")
		}
		// Every CHECK and the deferred audience trigger still hold: touching each
		// request re-runs the trigger when the transaction commits.
		if _, err := store.Conn.Exec(ctx, `UPDATE mail_approvals SET updated_at = updated_at; UPDATE mail_approval_events SET note = note`); err != nil {
			t.Fatalf("constraints broken after erasure: %v", err)
		}
	})
}

func TestEraseAccountRepeatsTheFirstReceiptAndFindsNothingLeft(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	requestID := uuid.New()

	first, err := store.EraseAccount(ctx, eraseDeniz(requestID))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotErasureTables(t, store)

	again, err := store.EraseAccount(ctx, eraseDeniz(requestID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Errorf("repeat = %+v, want the first receipt %+v", again, first)
	}
	stored, err := store.FindAccountErasureReceipt(ctx, requestID)
	if err != nil || stored == nil || !reflect.DeepEqual(*stored, *first) {
		t.Errorf("stored receipt = %+v %v", stored, err)
	}

	// Even without the receipt, the work is done: another request finds nothing.
	other, err := store.EraseAccount(ctx, eraseDeniz(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	for key, n := range other.Counts {
		if n != 0 {
			t.Errorf("second erasure changed %s: %d", key, n)
		}
	}
	assertOnlyChanged(t, snapshot, snapshotErasureTables(t, store), nil)
	if n := queryCount(t, store, `SELECT count(*) FROM account_erasure_receipts`); n != 2 {
		t.Errorf("receipts = %d, want 2", n)
	}
}

func TestEraseAccountReceiptHoldsNoSubjectOrAddress(t *testing.T) {
	store := erasureStore(t)
	requestID := uuid.New()
	if _, err := store.EraseAccount(context.Background(), eraseDeniz(requestID)); err != nil {
		t.Fatal(err)
	}
	row := queryString(t, store, `SELECT row_to_json(r)::text FROM account_erasure_receipts r`)
	lower := strings.ToLower(*row)
	for _, secret := range []string{erasedSub, erasedSchool, erasedPersonal, "deniz"} {
		if strings.Contains(lower, secret) {
			t.Errorf("receipt %s holds %q", *row, secret)
		}
	}
	var columns []string
	rows, err := store.Conn.Query(context.Background(), `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'account_erasure_receipts' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		columns = append(columns, c)
	}
	rows.Close()
	if !reflect.DeepEqual(columns, []string{"request_id", "completed_at", "counts"}) {
		t.Errorf("columns = %v", columns)
	}
}

func TestConcurrentErasuresOfOneRequestDoTheWorkOnce(t *testing.T) {
	store := erasureStore(t)
	requestID := uuid.New()

	const callers = 4
	receipts := make([]*AccountErasureReceipt, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			receipts[i], errs[i] = store.EraseAccount(context.Background(), eraseDeniz(requestID))
		}()
	}
	close(start)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(receipts[i], receipts[0]) {
			t.Errorf("caller %d got %+v, caller 0 %+v", i, receipts[i], receipts[0])
		}
	}
	if receipts[0].Counts[AccountErasureRecipientsDeleted] != 2 {
		t.Errorf("counts = %v: the work was not done by the one that wrote the receipt", receipts[0].Counts)
	}
	if n := queryCount(t, store, `SELECT count(*) FROM account_erasure_receipts`); n != 1 {
		t.Errorf("receipts = %d, want 1", n)
	}
}

// A send going out to the person right now cannot be changed under the
// worker: the work done so far is committed, and the next call finishes.
func TestEraseAccountWaitsForASendInFlightToThePerson(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status)
		VALUES ('50000000-0000-4000-8000-0000000000f3', '40000000-0000-4000-8000-00000000000f', 'Deniz Yılmaz', 'DENIZ@example.com',
		        'Yeni dönem', 'Merhaba Deniz Yılmaz', '<p>Merhaba Deniz Yılmaz</p>', 'processing')`); err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()

	_, err := store.EraseAccount(ctx, eraseDeniz(requestID))
	if !errors.Is(err, ErrAccountErasureInProgress) {
		t.Fatalf("err = %v, want in progress", err)
	}
	if got, _ := store.FindAccountErasureReceipt(ctx, requestID); got != nil {
		t.Fatal("a receipt was written while a send was in flight")
	}
	// Committed: the pending row is gone, never to be sent, and the sender
	// and the bodies naming the person are cleared.
	if n := queryCount(t, store, `SELECT count(*) FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000f1'`); n != 0 {
		t.Error("the pending row to the person was not deleted")
	}
	if got := queryString(t, store, `SELECT sent_by FROM mail_tasks WHERE id = '40000000-0000-4000-8000-00000000000a'`); *got != DeletedUserSubject {
		t.Errorf("sent_by = %s", *got)
	}
	if got := queryString(t, store, `SELECT body FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000c1'`); *got != "" {
		t.Errorf("a body naming the person = %q", *got)
	}
	// The in-flight row is untouched.
	if got := queryString(t, store, `SELECT recipient_email FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000f3'`); *got != "DENIZ@example.com" {
		t.Errorf("in-flight row changed: %s", *got)
	}

	// Still in flight: still in progress.
	if _, err := store.EraseAccount(ctx, eraseDeniz(requestID)); !errors.Is(err, ErrAccountErasureInProgress) {
		t.Fatalf("second call err = %v, want in progress", err)
	}

	if _, err := store.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'sent' WHERE id = '50000000-0000-4000-8000-0000000000f3'`); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.EraseAccount(ctx, eraseDeniz(requestID))
	if err != nil {
		t.Fatal(err)
	}
	var name, email, body string
	if err := store.Conn.QueryRow(ctx, `SELECT recipient_full_name, recipient_email, body FROM mail_queue
		WHERE id = '50000000-0000-4000-8000-0000000000f3'`).Scan(&name, &email, &body); err != nil {
		t.Fatal(err)
	}
	if name != DeletedUserName || email != "" || body != "" {
		t.Errorf("sent row = %q %q %q", name, email, body)
	}
	if receipt.Counts[AccountErasureRecipientsDeleted] != 2 || receipt.Counts[AccountErasureQueueRowsCleared] != 4 {
		t.Errorf("counts = %v", receipt.Counts)
	}
}

// Another person's mail still queued or going out that names the person is
// left to go out as it was rendered; once it has, its body is cleared. The
// name to look for is still known then, though the pending row it came
// from is gone.
func TestEraseAccountWaitsForAnotherPersonsMailNamingThePerson(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status)
		VALUES ('50000000-0000-4000-8000-0000000000f4', '40000000-0000-4000-8000-00000000000f', 'Ayşe Kaya', 'ayse@example.com',
		        'Yeni dönem', 'Deniz Yılmaz sana yazdı', '<p>Deniz Yılmaz sana yazdı</p>', 'pending')`); err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()
	if _, err := store.EraseAccount(ctx, eraseDeniz(requestID)); !errors.Is(err, ErrAccountErasureInProgress) {
		t.Fatalf("err = %v, want in progress", err)
	}
	if got := queryString(t, store, `SELECT body FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000f4'`); *got != "Deniz Yılmaz sana yazdı" {
		t.Errorf("queued body = %q, want it to go out as rendered", *got)
	}
	if _, err := store.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'sent' WHERE id = '50000000-0000-4000-8000-0000000000f4'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseAccount(ctx, eraseDeniz(requestID)); err != nil {
		t.Fatal(err)
	}
	var email, body string
	var html *string
	if err := store.Conn.QueryRow(ctx, `SELECT recipient_email, body, body_html FROM mail_queue
		WHERE id = '50000000-0000-4000-8000-0000000000f4'`).Scan(&email, &body, &html); err != nil {
		t.Fatal(err)
	}
	if email != "ayse@example.com" || body != "" || html != nil {
		t.Errorf("sent row = %q %q %v", email, body, html)
	}
}

func TestEraseAccountWithoutAddressesStillReplacesTheSubject(t *testing.T) {
	store := erasureStore(t)
	// Without addresses the queued row to Deniz is anyone's row naming Deniz,
	// which would be waited for; let it have gone out.
	if _, err := store.Conn.Exec(context.Background(), `UPDATE mail_queue SET status = 'sent' WHERE id = '50000000-0000-4000-8000-0000000000f1'`); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.EraseAccount(context.Background(), AccountErasure{RequestID: uuid.New(), Subject: erasedSub})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Counts[AccountErasureActorColumnsReplaced] != 9 || receipt.Counts[AccountErasureRecipientsDeleted] != 0 {
		t.Errorf("counts = %v", receipt.Counts)
	}
	// The names the subject's own rows carry are still searched for.
	if got := queryString(t, store, `SELECT body FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000c1'`); *got != "" {
		t.Errorf("body naming the subject = %q", *got)
	}
	if !receipt.CompletedAt.Before(time.Now().Add(time.Minute)) {
		t.Errorf("completed_at = %v", receipt.CompletedAt)
	}
}

func ptrTo(s string) *string { return &s }

func shown(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}
