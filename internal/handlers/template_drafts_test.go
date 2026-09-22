package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// servedDraft is one version whole, as the draft routes answer with it.
type servedDraft struct {
	servedVersion
	JSXSource        *string         `json:"jsx_source"`
	VisualSource     json.RawMessage `json:"visual_source"`
	HTMLSource       *string         `json:"html_source"`
	HTMLContent      string          `json:"html_content"`
	PlainTextContent string          `json:"plain_text_content"`
}

// saveDraft saves a draft of a template as the operator the headers name, and
// decodes the version it answers with when it answers with one.
func saveDraft(t *testing.T, app *fiber.App, templateID uuid.UUID, draft map[string]any, headers ...string) (*http.Response, servedDraft, []byte) {
	t.Helper()
	response, body := sendJSONAs(t, app, fiber.MethodPost, "/templates/"+templateID.String()+"/drafts", draft, headers...)
	var version servedDraft
	if response.StatusCode == fiber.StatusOK || response.StatusCode == fiber.StatusCreated {
		if err := json.Unmarshal(body, &version); err != nil {
			t.Fatalf("draft answer %s: %v", body, err)
		}
	}
	return response, version, body
}

// sendJSONAs is sendJSON with request headers, given as name, value pairs.
func sendJSONAs(t *testing.T, app *fiber.App, method, path string, body any, headers ...string) (*http.Response, []byte) {
	t.Helper()
	if len(headers) == 0 {
		return sendJSON(t, app, method, path, body)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	return response, raw
}

// templateRow is a template's row as the send path reads it.
func templateRow(t *testing.T, db *database.Store, id uuid.UUID) database.Template {
	t.Helper()
	row, err := db.GetTemplateByIdIncludingArchived(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// A template made in the old panel: one published operator version, JSX its
// Main source.
func panelTemplate(t *testing.T, app *fiber.App) database.Template {
	t.Helper()
	return createTemplate(t, app, map[string]any{
		"name": "Etkinlik duyurusu", "subject": "Merhaba {{.FullName}}",
		"html_content": "<p>Merhaba {{.FullName}}</p>", "plain_text_content": "Merhaba {{.FullName}}",
		"react_email_content": panelSource,
	})
}

// Saving writes a draft: a version that is sent to nobody until it is
// published. The row, which is what the send path reads, stays exactly as it
// was and still names the version it is a copy of.
func TestSavingADraftLeavesThePublishedTemplateAlone(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)
	before := templateRow(t, db, created.ID)

	edited := panelSource + "\n// taslak\n"
	response, draft, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Selam {{.FullName}}", "main_mode": "jsx", "jsx_source": edited,
		"html_content": "<p>Selam {{.FullName}}</p>", "plain_text_content": "Selam {{.FullName}}",
		"base_version_id": created.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("POST drafts = %d %s, want 201", response.StatusCode, body)
	}
	if draft.PublishedAt != nil || draft.Current || draft.Seq != 2 || draft.TemplateID != created.ID ||
		draft.BaseVersionID == nil || *draft.BaseVersionID != *created.PublishedVersionID {
		t.Fatalf("draft = %+v, want version 2 unpublished, started from the published one", draft.servedVersion)
	}
	if draft.Author.Kind != "operator" || !sameString(draft.Author.Sub, operatorSub) || !sameString(draft.Author.Name, operatorName) {
		t.Fatalf("draft author = %+v, want the operator by subject and name", draft.Author)
	}
	if draft.Subject != "Selam {{.FullName}}" || draft.MainMode != "jsx" || !sameString(draft.JSXSource, edited) ||
		draft.HTMLContent != "<p>Selam {{.FullName}}</p>" || draft.PlainTextContent != "Selam {{.FullName}}" {
		t.Fatalf("draft content = %+v, want what was saved", draft)
	}

	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("the row changed on a draft save:\nbefore %+v\nafter  %+v", before, after)
	}
	versions, published := storedVersions(t, db, created.ID)
	if len(versions) != 2 || versions[1].ID != draft.ID || versions[1].PublishedAt != nil ||
		published == nil || *published != *created.PublishedVersionID {
		t.Fatalf("versions = %d (row copy of %v), want the first still published and the draft beside it", len(versions), published)
	}
}

// publish publishes a version of a template, forced or not, and decodes the
// template it answers with when it answers with one.
func publish(t *testing.T, app *fiber.App, templateID, versionID uuid.UUID, force bool) (*http.Response, database.Template, []byte) {
	t.Helper()
	path := "/templates/" + templateID.String() + "/versions/" + versionID.String() + "/publish"
	var body any
	if force {
		body = map[string]any{"force": true}
	}
	response, raw := sendJSON(t, app, fiber.MethodPost, path, body)
	var template database.Template
	if response.StatusCode == fiber.StatusOK {
		if err := json.Unmarshal(raw, &template); err != nil {
			t.Fatalf("publish answer %s: %v", raw, err)
		}
	}
	return response, template, raw
}

// A Visual document as the block editor saves one.
const visualDocument = `{"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "Aramıza hoş geldin"}]}]}`

// seededTemplate is a System template the Template seed wrote: one published
// seed version, HTML its Main source.
func seededTemplate(t *testing.T, app *fiber.App, key string) database.Template {
	t.Helper()
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "Hoş Geldin", "subject": "SKY LAB'e hoş geldin",
		"html_content": "<p>Hoş geldin {{.FullName}}</p>", "plain_text_content": "Hoş geldin {{.FullName}}",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	var seeded database.Template
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatal(err)
	}
	return seeded
}

// queuedFor is what a single send queued for a recipient.
func queuedFor(t *testing.T, db *database.Store, email string) (subject, text, html string) {
	t.Helper()
	if err := db.Conn.QueryRow(context.Background(), `SELECT subject, body, body_html FROM mail_queue WHERE recipient_email = $1`, email).
		Scan(&subject, &text, &html); err != nil {
		t.Fatal(err)
	}
	return subject, text, html
}

func sendByKey(t *testing.T, app *fiber.App, key, email string) {
	t.Helper()
	response, body := sendJSON(t, app, fiber.MethodPost, "/mail_tasks/single", map[string]any{
		"template_key": key, "recipient_email": email, "recipient_full_name": "Deniz Kaya",
		"body_variables": map[string]string{},
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("send = %d %s", response.StatusCode, body)
	}
}

// Publishing a draft makes it the version that is sent: it is marked published
// and copied onto the row — its subject, its render, and its JSX source as
// react_email_content, which the old panel edits. A send then queues it, and a
// newer draft lying unpublished beside it is not what a send queues.
func TestPublishingADraftCopiesItOntoTheRowAndSendsIt(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "Aramıza hoş geldin, {{.FullName}}", "main_mode": "visual",
		"visual_source": json.RawMessage(visualDocument), "jsx_source": panelSource,
		"html_content": "<p>Aramıza hoş geldin {{.FullName}}</p>", "plain_text_content": "Aramıza hoş geldin {{.FullName}}",
		"base_version_id": seeded.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("save = %d %s", response.StatusCode, body)
	}

	response, published, body := publish(t, app, seeded.ID, draft.ID, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("publish = %d %s, want 200", response.StatusCode, body)
	}
	row := templateRow(t, db, seeded.ID)
	for name, got := range map[string]database.Template{"answer": published, "row": row} {
		if got.Subject != "Aramıza hoş geldin, {{.FullName}}" || got.HtmlContent != "<p>Aramıza hoş geldin {{.FullName}}</p>" ||
			got.PlainTextContent != "Aramıza hoş geldin {{.FullName}}" || got.ReactEmailContent != panelSource ||
			got.PublishedVersionID == nil || *got.PublishedVersionID != draft.ID {
			t.Fatalf("%s after publishing = %+v, want a copy of the draft, its JSX source as react_email_content", name, got)
		}
	}
	if row.Name != seeded.Name || row.Key == nil || *row.Key != key || !row.System || !row.UpdatedAt.After(seeded.UpdatedAt) {
		t.Fatalf("row after publishing = %+v, want only the version's fields copied and updated_at moved", row)
	}
	versions, _ := storedVersions(t, db, seeded.ID)
	if len(versions) != 2 || versions[1].PublishedAt == nil || versions[1].PublishedAt.Before(versions[1].CreatedAt) {
		t.Fatalf("versions = %+v, want the draft published in place", versions)
	}
	if versions[0].PublishedAt == nil {
		t.Fatalf("the seed's version lost its publish time: %+v", versions[0])
	}

	// A newer draft, not published, beside the published one.
	if response, _, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "YARIM", "main_mode": "html", "html_source": "<p>YARIM</p>",
		"html_content": "<p>YARIM</p>", "plain_text_content": "YARIM", "base_version_id": draft.ID,
	}); response.StatusCode != fiber.StatusCreated {
		t.Fatalf("second draft = %d %s", response.StatusCode, body)
	}
	sendByKey(t, app, key, "uye@yildizskylab.com")
	subject, text, html := queuedFor(t, db, "uye@yildizskylab.com")
	if subject != "Aramıza hoş geldin, Deniz Kaya" || text != "Aramıza hoş geldin Deniz Kaya" || html != "<p>Aramıza hoş geldin Deniz Kaya</p>" {
		t.Fatalf("queued %q / %q / %q, want the published draft rendered for the recipient", subject, text, html)
	}
}

// appError is the API's error body.
type appError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Params  map[string]any `json:"params"`
}

func decodeError(t *testing.T, body []byte) appError {
	t.Helper()
	var e appError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %s: %v", body, err)
	}
	return e
}

// A draft is stale when another version was published after it was started.
// Publishing it would quietly revert that version (ADR-0047, the other way
// round), so it is refused with a conflict naming both — the version the
// draft started from and the one published now — for the editor to show side
// by side. Forced, it goes through, and the version it replaces stays in the
// history.
func TestPublishingAStaleDraftIsRefusedUntilForced(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)
	started := *created.PublishedVersionID

	response, draft, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Taslak konu", "main_mode": "jsx", "jsx_source": panelSource + "\n// taslak\n",
		"html_content": "<p>Taslak</p>", "plain_text_content": "Taslak", "base_version_id": started,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("save = %d %s", response.StatusCode, body)
	}

	// Meanwhile another operator edits the template in the old panel, which
	// publishes at once.
	response, body = sendJSONAs(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
		"name": created.Name, "subject": "Başkasının konusu", "html_content": "<p>Başkası</p>",
		"plain_text_content": "Başkası", "react_email_content": panelSource,
	}, "X-Operator-Sub", "b7d1e7a2-3c1f-4c55-9d6e-0a1b2c3d4e5f", "X-Operator-Name", "Can Demir")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("other operator's edit = %d %s", response.StatusCode, body)
	}
	theirs := *templateRow(t, db, created.ID).PublishedVersionID
	before := templateRow(t, db, created.ID)

	response, _, body = publish(t, app, created.ID, draft.ID, false)
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("publishing a stale draft = %d %s, want 409", response.StatusCode, body)
	}
	conflict := decodeError(t, body)
	if conflict.Code != "template.stale_base" || conflict.Message == "" ||
		conflict.Params["version_id"] != draft.ID.String() ||
		conflict.Params["base_version_id"] != started.String() ||
		conflict.Params["published_version_id"] != theirs.String() {
		t.Fatalf("conflict = %+v, want template.stale_base naming the draft, its base %s and the published %s", conflict, started, theirs)
	}
	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused publish changed the row: %+v", after)
	}
	if versions, _ := storedVersions(t, db, created.ID); versions[1].ID != draft.ID || versions[1].PublishedAt != nil {
		t.Fatalf("a refused publish published the draft: %+v", versions[1])
	}

	response, published, body := publish(t, app, created.ID, draft.ID, true)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("forced publish = %d %s, want 200", response.StatusCode, body)
	}
	if published.PublishedVersionID == nil || *published.PublishedVersionID != draft.ID || published.Subject != "Taslak konu" {
		t.Fatalf("after a forced publish the row = %+v, want a copy of the draft", published)
	}
	versions, _ := storedVersions(t, db, created.ID)
	replaced := versions[2]
	if replaced.ID != theirs || replaced.PublishedAt == nil || replaced.Subject != "Başkasının konusu" || !sameString(replaced.AuthorName, "Can Demir") {
		t.Fatalf("the replaced version = %+v, want it kept in the history as it was", replaced)
	}
}

// Only a Template seed that changes the template makes a draft stale: an
// unchanged seed records no version (ticket 04), so it leaves the base
// published and the draft publishes without force.
func TestOnlyAChangingSeedMakesADraftStale(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	draftOf := func(subject string, base uuid.UUID) servedDraft {
		t.Helper()
		response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
			"subject": subject, "main_mode": "html", "html_source": "<p>" + subject + "</p>",
			"html_content": "<p>" + subject + "</p>", "plain_text_content": subject, "base_version_id": base,
		})
		if response.StatusCode != fiber.StatusCreated {
			t.Fatalf("save = %d %s", response.StatusCode, body)
		}
		return draft
	}

	first := draftOf("Birinci taslak", *seeded.PublishedVersionID)
	seededTemplate(t, app, key) // the same seed again
	if response, _, body := publish(t, app, seeded.ID, first.ID, false); response.StatusCode != fiber.StatusOK {
		t.Fatalf("publishing after an unchanged seed = %d %s, want 200", response.StatusCode, body)
	}

	second := draftOf("İkinci taslak", first.ID)
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "Hoş Geldin", "subject": "SKY LAB'e hoş geldin",
		"html_content": "<p>Koyu temalı hoş geldin {{.FullName}}</p>", "plain_text_content": "Hoş geldin {{.FullName}}",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("changed seed = %d %s", response.StatusCode, body)
	}
	response, _, body = publish(t, app, seeded.ID, second.ID, false)
	if response.StatusCode != fiber.StatusConflict || decodeError(t, body).Code != "template.stale_base" {
		t.Fatalf("publishing over a changed seed = %d %s, want 409 template.stale_base", response.StatusCode, body)
	}
}

// restore restores a version of a template as a draft, as the operator the
// headers name, and decodes the version it answers with when it answers with
// one.
func restore(t *testing.T, app *fiber.App, templateID, versionID uuid.UUID, headers ...string) (*http.Response, servedDraft, []byte) {
	t.Helper()
	path := "/templates/" + templateID.String() + "/versions/" + versionID.String() + "/restore"
	response, body := sendJSONAs(t, app, fiber.MethodPost, path, nil, headers...)
	var version servedDraft
	if response.StatusCode == fiber.StatusOK || response.StatusCode == fiber.StatusCreated {
		if err := json.Unmarshal(body, &version); err != nil {
			t.Fatalf("restore answer %s: %v", body, err)
		}
	}
	return response, version, body
}

// Any version can be restored as a new draft: its subject, its sources, which
// one is main and its render are copied into a draft that starts from the
// version published now. Restoring is undoing through publishing, so nothing
// that is sent changes. Another operator's draft can be restored too.
func TestRestoringAVersionWritesADraft(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	template, history := templateWithHistory(t, db, app) // seed, edit (published), Can Demir's draft
	seed, edit, theirs := history[0], history[1], history[2]
	before := templateRow(t, db, template.ID)

	response, restored, body := restore(t, app, template.ID, seed.ID)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("restoring the seed's version = %d %s, want 201", response.StatusCode, body)
	}
	if restored.Seq != 4 || restored.PublishedAt != nil || restored.Current || restored.BaseVersionID == nil || *restored.BaseVersionID != edit.ID ||
		restored.Author.Kind != "operator" || !sameString(restored.Author.Sub, operatorSub) || restored.RequestedSubject != nil {
		t.Fatalf("restored = %+v, want version 4, my draft, started from the published edit", restored.servedVersion)
	}
	if restored.Subject != seed.Subject || restored.MainMode != seed.MainMode || restored.JSXSource != nil || !absent(restored.VisualSource) ||
		!sameString(restored.HTMLSource, *seed.HTMLSource) || restored.HTMLContent != seed.HTMLContent || restored.PlainTextContent != seed.PlainTextContent {
		t.Fatalf("restored content = %+v, want the seed's version's", restored)
	}
	if after := templateRow(t, db, template.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("restoring changed the row: %+v", after)
	}

	response, taken, body := restore(t, app, template.ID, theirs.ID)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("restoring another operator's draft = %d %s, want 201", response.StatusCode, body)
	}
	if taken.Seq != 5 || !sameString(taken.Author.Sub, operatorSub) || taken.MainMode != "visual" || !sameString(taken.JSXSource, *theirs.JSXSource) ||
		!jsonEqual(t, taken.VisualSource, theirs.VisualSource) || taken.Subject != theirs.Subject || taken.PublishedAt != nil {
		t.Fatalf("restored draft = %+v, want Can Demir's content as my draft", taken)
	}

	other := createTemplate(t, app, map[string]any{
		"name": "Başka", "subject": "Başka", "html_content": "<p>Başka</p>", "plain_text_content": "Başka",
		"react_email_content": panelSource,
	})
	for name, path := range map[string][2]uuid.UUID{
		"another template's version": {other.ID, edit.ID},
		"an unknown version":         {template.ID, uuid.New()},
		"an unknown template":        {uuid.New(), edit.ID},
	} {
		if response, _, body := restore(t, app, path[0], path[1]); response.StatusCode != fiber.StatusNotFound {
			t.Errorf("restoring %s = %d %s, want 404", name, response.StatusCode, body)
		}
	}
	if versions, _ := storedVersions(t, db, template.ID); len(versions) != 5 {
		t.Fatalf("versions = %d, want the two restores and nothing else", len(versions))
	}
}

// jsonEqual reports whether two JSON documents hold the same value.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// absent reports whether a served JSON field is null.
func absent(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// Making another source the Main source writes a new version, and the other
// sources stay exactly as they were (ADR-0046): a switch changes what is sent,
// once published, never what is kept. A save need not send the sources it
// leaves alone: one left out, or null, is kept from the version the save
// continues, so a save never drops a source.
func TestChangingTheMainSourceKeepsTheOtherSources(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app) // JSX is the Main source

	// A draft adds a Visual and an HTML source beside the JSX one; the JSX
	// source is not sent, and is kept.
	response, withSources, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Merhaba {{.FullName}}", "main_mode": "jsx",
		"visual_source": json.RawMessage(visualDocument), "html_source": "<p>HTML kaynağı</p>",
		"html_content": "<p>Merhaba {{.FullName}}</p>", "plain_text_content": "Merhaba {{.FullName}}",
		"base_version_id": created.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("adding sources = %d %s", response.StatusCode, body)
	}
	if !sameString(withSources.JSXSource, panelSource) || withSources.MainMode != "jsx" {
		t.Fatalf("draft = %+v, want the JSX source kept as the Main source", withSources)
	}
	if response, _, body := publish(t, app, created.ID, withSources.ID, false); response.StatusCode != fiber.StatusOK {
		t.Fatalf("publish = %d %s", response.StatusCode, body)
	}

	// Visual becomes the Main source: the switch sends the mode, the render the
	// Visual source gives, and nothing else — an explicit null included.
	response, switched, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Merhaba {{.FullName}}", "main_mode": "visual", "html_source": nil,
		"html_content": "<p>Görsel merhaba {{.FullName}}</p>", "plain_text_content": "Görsel merhaba {{.FullName}}",
		"base_version_id": withSources.ID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("switching the Main source = %d %s, want 201", response.StatusCode, body)
	}
	versions, _ := storedVersions(t, db, created.ID)
	if len(versions) != 3 || versions[2].ID != switched.ID {
		t.Fatalf("versions = %d, want the switch recorded as a third", len(versions))
	}
	before, after := versions[1], versions[2]
	if after.MainMode != "visual" || after.HTMLContent != "<p>Görsel merhaba {{.FullName}}</p>" || after.PublishedAt != nil {
		t.Fatalf("switch = %+v, want a Visual Main source draft with its render", after)
	}
	if !sameString(after.JSXSource, *before.JSXSource) || !sameString(after.HTMLSource, *before.HTMLSource) ||
		string(after.VisualSource) != string(before.VisualSource) {
		t.Fatalf("sources after the switch = jsx %v html %v visual %s, want them exactly as before", after.JSXSource, after.HTMLSource, after.VisualSource)
	}

	// Published, the switch sends the Visual render; the JSX source stays where
	// the old panel reads it.
	response, row, body := publish(t, app, created.ID, switched.ID, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("publish the switch = %d %s", response.StatusCode, body)
	}
	if row.HtmlContent != "<p>Görsel merhaba {{.FullName}}</p>" || row.ReactEmailContent != panelSource {
		t.Fatalf("row = %+v, want the Visual render sent and the JSX source kept for the old panel", row)
	}

	// The old panel rewording it sends that body back, and the Visual source
	// stays the Main source.
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
		"name": row.Name, "subject": "Selam {{.FullName}}", "html_content": row.HtmlContent,
		"plain_text_content": row.PlainTextContent, "react_email_content": row.ReactEmailContent,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("old panel reword = %d %s", response.StatusCode, body)
	}
	versions, _ = storedVersions(t, db, created.ID)
	reworded := versions[len(versions)-1]
	if reworded.Subject != "Selam {{.FullName}}" || reworded.MainMode != "visual" || string(reworded.VisualSource) != string(after.VisualSource) {
		t.Fatalf("old panel reword = %+v, want the Visual Main source kept", reworded)
	}
}

// Another operator, as the test app takes one from the request headers.
var canDemir = []string{"X-Operator-Sub", "b7d1e7a2-3c1f-4c55-9d6e-0a1b2c3d4e5f", "X-Operator-Name", "Can Demir"}

// Each save writes a new version, and an operator's newest one, while it is
// unpublished, is their draft in progress: the next save continues it. A save
// that changes nothing records nothing — compared with the operator's draft in
// progress, or with the version it starts from when they have none — and
// answers with that version, 200 instead of 201.
func TestASaveContinuesTheOperatorsDraftAndRecordsNothingUnchanged(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)
	published := *created.PublishedVersionID
	unchanged := map[string]any{
		"subject": created.Subject, "main_mode": "jsx", "jsx_source": panelSource,
		"html_content": created.HtmlContent, "plain_text_content": created.PlainTextContent, "base_version_id": published,
	}
	count := func() int {
		t.Helper()
		versions, _ := storedVersions(t, db, created.ID)
		return len(versions)
	}

	response, answered, body := saveDraft(t, app, created.ID, unchanged)
	if response.StatusCode != fiber.StatusOK || answered.ID != published || answered.PublishedAt == nil || count() != 1 {
		t.Fatalf("saving the published content = %d %s (%d versions), want 200 answering the published version and nothing recorded", response.StatusCode, body, count())
	}

	withVisual := map[string]any{
		"subject": created.Subject, "main_mode": "jsx", "visual_source": json.RawMessage(visualDocument),
		"html_content": created.HtmlContent, "plain_text_content": created.PlainTextContent, "base_version_id": published,
	}
	response, draft, body := saveDraft(t, app, created.ID, withVisual)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	// The same document written differently is the same document.
	withVisual["visual_source"] = json.RawMessage(`{"content":[{"content":[{"text":"Aramıza hoş geldin","type":"text"}],"type":"paragraph"}],"type":"doc"}`)
	response, answered, body = saveDraft(t, app, created.ID, withVisual)
	if response.StatusCode != fiber.StatusOK || answered.ID != draft.ID || count() != 2 {
		t.Fatalf("saving the draft again = %d %s (%d versions), want 200 answering the draft and nothing recorded", response.StatusCode, body, count())
	}

	// The next save continues the draft: making Visual the Main source finds
	// the Visual source in the draft, which the published version does not hold.
	response, continued, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": created.Subject, "main_mode": "visual",
		"html_content": "<p>Görsel</p>", "plain_text_content": "Görsel", "base_version_id": published,
	})
	if response.StatusCode != fiber.StatusCreated || continued.MainMode != "visual" || !jsonEqual(t, continued.VisualSource, []byte(visualDocument)) ||
		!sameString(continued.JSXSource, panelSource) {
		t.Fatalf("continuing the draft = %d %s, want a Visual Main source draft keeping both sources", response.StatusCode, body)
	}

	// Another operator's identical save is their own draft, not mine.
	response, theirs, body := saveDraft(t, app, created.ID, withVisual, canDemir...)
	if response.StatusCode != fiber.StatusCreated || theirs.ID == draft.ID || !sameString(theirs.Author.Name, "Can Demir") {
		t.Fatalf("another operator's save = %d %s, want their own draft", response.StatusCode, body)
	}

	// Restoring records nothing either when it would change nothing: an
	// operator with no draft restoring what is published, or one restoring
	// what their draft already holds.
	response, answered, body = restore(t, app, created.ID, published, "X-Operator-Sub", "0c9e5f1a-7c1e-4a8e-8f55-3b1c9d2e4f60", "X-Operator-Name", "Ece Ak")
	if response.StatusCode != fiber.StatusOK || answered.ID != published {
		t.Fatalf("restoring the published version with no draft = %d %s, want 200 answering it", response.StatusCode, body)
	}
	response, answered, body = restore(t, app, created.ID, theirs.ID, canDemir...)
	if response.StatusCode != fiber.StatusOK || answered.ID != theirs.ID {
		t.Fatalf("restoring one's own draft = %d %s, want 200 answering it", response.StatusCode, body)
	}
	if count() != 4 {
		t.Fatalf("versions = %d, want the published one and three drafts", count())
	}
}

// What the server checks of a draft it cannot render. The editor renders the
// Main source; the server stores the render it is given, and refuses what it
// can tell is wrong: a missing or blank field, a Main source whose Authoring
// mode has no source, a base that is not a published version of this
// template, a Visual source that is not a JSON object, a JSX source with no
// code in it, and a subject, plain text or HTML the mailer could not parse —
// every send of it would fail. A refused save records nothing.
func TestDraftSavesTheServerRefuses(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app) // one published version, JSX its only source
	published := *created.PublishedVersionID
	other := createTemplate(t, app, map[string]any{
		"name": "Başka", "subject": "Başka", "html_content": "<p>Başka</p>", "plain_text_content": "Başka",
		"react_email_content": panelSource,
	})
	response, draft, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Taslak", "main_mode": "jsx", "html_content": "<p>Taslak</p>", "plain_text_content": "Taslak",
		"base_version_id": published,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	before := templateRow(t, db, created.ID)

	valid := func(change map[string]any) map[string]any {
		body := map[string]any{
			"subject": "Merhaba {{.FullName}}", "main_mode": "jsx",
			"html_content": "<p>Merhaba {{.FullName}}</p>", "plain_text_content": "Merhaba {{.FullName}}",
			"base_version_id": published,
		}
		for field, value := range change {
			if value == nil {
				delete(body, field)
				continue
			}
			body[field] = value
		}
		return body
	}
	for name, tc := range map[string]struct {
		body  map[string]any
		code  string
		field string
	}{
		"no subject":                       {valid(map[string]any{"subject": nil}), "validation.error", "subject"},
		"a blank subject":                  {valid(map[string]any{"subject": "  "}), "validation.error", "subject"},
		"no Main source mode":              {valid(map[string]any{"main_mode": nil}), "validation.error", "main_mode"},
		"an unknown Main source mode":      {valid(map[string]any{"main_mode": "markdown"}), "validation.error", "main_mode"},
		"no HTML render":                   {valid(map[string]any{"html_content": nil}), "validation.error", "html_content"},
		"a blank HTML render":              {valid(map[string]any{"html_content": "\n  "}), "validation.error", "html_content"},
		"a blank plain text render":        {valid(map[string]any{"plain_text_content": " "}), "validation.error", "plain_text_content"},
		"a Visual source not a object":     {valid(map[string]any{"visual_source": json.RawMessage(`["paragraph"]`)}), "validation.error", "visual_source"},
		"a JSX source with no code":        {valid(map[string]any{"jsx_source": seedPointerComment("core.welcome")}), "validation.error", "jsx_source"},
		"a blank HTML source":              {valid(map[string]any{"html_source": " "}), "validation.error", "html_source"},
		"a base that is not a UUID":        {valid(map[string]any{"base_version_id": "sürüm-3"}), "validation.error", ""},
		"a Main source with no source":     {valid(map[string]any{"main_mode": "html"}), "template.main_source_missing", ""},
		"an unknown base":                  {valid(map[string]any{"base_version_id": uuid.New()}), "template.invalid_base", ""},
		"no base, though one is published": {valid(map[string]any{"base_version_id": nil}), "template.invalid_base", ""},
		"another template's base":          {valid(map[string]any{"base_version_id": other.PublishedVersionID}), "template.invalid_base", ""},
		"a draft as the base":              {valid(map[string]any{"base_version_id": draft.ID}), "template.invalid_base", ""},
		"a subject that does not parse":    {valid(map[string]any{"subject": "{{if .FullName}}Merhaba"}), "template.invalid_body", "subject"},
		"plain text that does not parse":   {valid(map[string]any{"plain_text_content": "{{upper .FullName}}"}), "template.invalid_body", "plain_text_content"},
		"HTML that does not parse":         {valid(map[string]any{"html_content": "<p>{{.FullName</p>"}), "template.invalid_body", "html_content"},
	} {
		t.Run(name, func(t *testing.T) {
			response, _, body := saveDraft(t, app, created.ID, tc.body)
			if response.StatusCode != fiber.StatusBadRequest {
				t.Fatalf("save = %d %s, want 400", response.StatusCode, body)
			}
			refusal := decodeError(t, body)
			if refusal.Code != tc.code || refusal.Message == "" {
				t.Fatalf("refusal = %+v, want %s", refusal, tc.code)
			}
			if tc.field != "" && !namesField(refusal, tc.field) {
				t.Fatalf("refusal = %+v, want it to name %s", refusal, tc.field)
			}
		})
	}

	// A body that is not JSON is the caller's mistake too.
	request := httptest.NewRequest(fiber.MethodPost, "/templates/"+created.ID.String()+"/drafts", bytes.NewReader([]byte(`{"subject": `)))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusBadRequest || decodeError(t, raw).Code != "validation.error" {
		t.Fatalf("a malformed body = %d %s, want 400 validation.error", response.StatusCode, raw)
	}

	for name, id := range map[string]string{"an unknown template": uuid.NewString(), "a malformed template id": "not-a-uuid"} {
		response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+id+"/drafts", valid(nil))
		if response.StatusCode != fiber.StatusNotFound {
			t.Errorf("saving a draft of %s = %d %s, want 404", name, response.StatusCode, body)
		}
	}

	if versions, _ := storedVersions(t, db, created.ID); len(versions) != 2 {
		t.Fatalf("versions = %d, want the refused saves to record nothing", len(versions))
	}
	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused save changed the row: %+v", after)
	}
}

// namesField reports whether an API error names a request field: in the field
// list of a validation error, or as the field it is about.
func namesField(e appError, field string) bool {
	if e.Params["field"] == field {
		return true
	}
	errs, _ := e.Params["errors"].([]any)
	for _, item := range errs {
		if entry, ok := item.(map[string]any); ok && entry["field"] == field {
			return true
		}
	}
	return false
}

// What publishing refuses. Only a draft of this template is published; the
// version already sent may be published again, which changes nothing, so a
// repeated request is safe. A draft the mailer could not parse is refused too:
// it can only be a copy of a version written before anything checked. A
// refused publish leaves the row as it was.
func TestPublishesTheServerRefuses(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)
	first := *created.PublishedVersionID
	edit := func(html string) uuid.UUID {
		t.Helper()
		response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
			"name": created.Name, "subject": created.Subject, "html_content": html,
			"plain_text_content": "Merhaba", "react_email_content": panelSource,
		})
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("old panel edit = %d %s", response.StatusCode, body)
		}
		return *templateRow(t, db, created.ID).PublishedVersionID
	}
	current := edit("<p>İkinci</p>")
	before := templateRow(t, db, created.ID)

	response, again, body := publish(t, app, created.ID, current, false)
	if response.StatusCode != fiber.StatusOK || again.PublishedVersionID == nil || *again.PublishedVersionID != current {
		t.Fatalf("publishing the version already sent = %d %s, want 200 and nothing changed", response.StatusCode, body)
	}
	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("publishing the version already sent changed the row: %+v", after)
	}

	response, _, body = publish(t, app, created.ID, first, false)
	if response.StatusCode != fiber.StatusConflict || decodeError(t, body).Code != "template.not_a_draft" {
		t.Fatalf("publishing an earlier published version = %d %s, want 409 template.not_a_draft", response.StatusCode, body)
	}

	other := panelTemplate(t, app)
	response, theirs, body := saveDraft(t, app, other.ID, map[string]any{
		"subject": "Başka", "main_mode": "jsx", "html_content": "<p>Başka</p>", "plain_text_content": "Başka",
		"base_version_id": other.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft of another template = %d %s", response.StatusCode, body)
	}
	for name, path := range map[string]string{
		"another template's draft": created.ID.String() + "/versions/" + theirs.ID.String(),
		"an unknown version":       created.ID.String() + "/versions/" + uuid.NewString(),
		"an unknown template":      uuid.NewString() + "/versions/" + theirs.ID.String(),
		"a malformed version id":   created.ID.String() + "/versions/sürüm-2",
		"a malformed template id":  "şablon/versions/" + theirs.ID.String(),
	} {
		if response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+path+"/publish", nil); response.StatusCode != fiber.StatusNotFound {
			t.Errorf("publishing %s = %d %s, want 404", name, response.StatusCode, body)
		}
	}
	if response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+other.ID.String()+"/versions/"+theirs.ID.String()+"/publish",
		map[string]any{"force": "evet"}); response.StatusCode != fiber.StatusBadRequest || decodeError(t, body).Code != "validation.error" {
		t.Errorf("publishing with a force that is not a boolean = %d %s, want 400 validation.error", response.StatusCode, body)
	}

	// The old panel wrote a body the mailer cannot parse before anything
	// checked it; it was fixed, and then restored.
	broken := edit("<p>{{.FullName</p>")
	edit("<p>Düzeldi</p>")
	response, restored, body := restore(t, app, created.ID, broken)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("restore = %d %s", response.StatusCode, body)
	}
	before = templateRow(t, db, created.ID)
	response, _, body = publish(t, app, created.ID, restored.ID, false)
	if refusal := decodeError(t, body); response.StatusCode != fiber.StatusBadRequest || refusal.Code != "template.invalid_body" || !namesField(refusal, "html_content") {
		t.Fatalf("publishing a body the mailer cannot parse = %d %s, want 400 template.invalid_body naming html_content", response.StatusCode, body)
	}
	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused publish changed the row: %+v", after)
	}
}

// An archived template takes no drafts, restores or publishes until it is
// restored itself, and says so in the API's error shape. Its history stays
// readable.
func TestAnArchivedTemplateTakesNoDraftsOrPublishes(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)
	response, draft, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Taslak", "main_mode": "jsx", "html_content": "<p>Taslak</p>", "plain_text_content": "Taslak",
		"base_version_id": created.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	if response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+created.ID.String(), nil)); err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("archive: %v %v", response, err)
	}
	before := templateRow(t, db, created.ID)

	refused := func(name string, response *http.Response, body []byte) {
		t.Helper()
		if refusal := decodeError(t, body); response.StatusCode != fiber.StatusConflict || refusal.Code != "template.archived" || refusal.Message == "" {
			t.Errorf("%s of an archived template = %d %s, want 409 template.archived", name, response.StatusCode, body)
		}
	}
	response, _, body = saveDraft(t, app, created.ID, map[string]any{
		"subject": "Yeni", "main_mode": "jsx", "html_content": "<p>Yeni</p>", "plain_text_content": "Yeni",
		"base_version_id": created.PublishedVersionID,
	})
	refused("a draft", response, body)
	response, _, body = restore(t, app, created.ID, *created.PublishedVersionID)
	refused("a restore", response, body)
	response, _, body = publish(t, app, created.ID, draft.ID, true)
	refused("a publish", response, body)

	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused writes changed the archived row: %+v", after)
	}
	if versions, _ := storedVersions(t, db, created.ID); len(versions) != 2 || versions[1].PublishedAt != nil {
		t.Fatalf("versions = %+v, want the first and the unpublished draft only", versions)
	}
}

// Every writer of a template takes its row's lock before it numbers a version,
// so saves that meet — drafts of several operators and old panel edits beside
// them — number their versions one after another: none lost, none twice, and
// the row a copy of the last version published.
func TestConcurrentSavesNumberVersionsInTurn(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)

	type result struct {
		what   string
		status int
		body   string
		err    error
	}
	request := func(what, method, path string, body any, headers ...string) result {
		payload, err := json.Marshal(body)
		if err != nil {
			return result{what: what, err: err}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		response, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			return result{what: what, err: err}
		}
		raw, _ := io.ReadAll(response.Body)
		return result{what: what, status: response.StatusCode, body: string(raw)}
	}

	const drafts, edits = 8, 4
	results := make(chan result, drafts+edits)
	for i := 0; i < drafts; i++ {
		go func(i int) {
			subject := fmt.Sprintf("Taslak %d", i)
			results <- request("draft "+subject, fiber.MethodPost, "/templates/"+created.ID.String()+"/drafts", map[string]any{
				"subject": subject, "main_mode": "jsx", "html_content": "<p>" + subject + "</p>", "plain_text_content": subject,
				"base_version_id": created.PublishedVersionID,
			}, "X-Operator-Sub", uuid.NewString(), "X-Operator-Name", "Operatör "+fmt.Sprint(i))
		}(i)
	}
	for i := 0; i < edits; i++ {
		go func(i int) {
			subject := fmt.Sprintf("Düzenleme %d", i)
			results <- request("edit "+subject, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
				"name": created.Name, "subject": subject, "html_content": "<p>" + subject + "</p>",
				"plain_text_content": subject, "react_email_content": panelSource,
			})
		}(i)
	}
	for i := 0; i < drafts+edits; i++ {
		r := <-results
		if r.err != nil || (r.status != fiber.StatusCreated && r.status != fiber.StatusOK) {
			t.Errorf("%s = %d %s %v", r.what, r.status, r.body, r.err)
		}
	}

	versions, published := storedVersions(t, db, created.ID)
	if len(versions) != 1+drafts+edits {
		t.Fatalf("versions = %d, want %d", len(versions), 1+drafts+edits)
	}
	var unpublished int
	var lastPublished storedVersion
	for i, v := range versions {
		if v.Seq != i+1 {
			t.Fatalf("version %d is numbered %d; want 1…%d in turn", i+1, v.Seq, len(versions))
		}
		if v.PublishedAt == nil {
			unpublished++
		} else {
			lastPublished = v
		}
	}
	if unpublished != drafts {
		t.Fatalf("drafts = %d, want %d", unpublished, drafts)
	}
	row := templateRow(t, db, created.ID)
	if published == nil || *published != lastPublished.ID || row.Subject != lastPublished.Subject {
		t.Fatalf("row = %q, copy of %v; want a copy of the last published version %q", row.Subject, published, lastPublished.Subject)
	}
}

// servedTemplate is a template as the template routes serve it.
type servedTemplate struct {
	database.Template
	MainMode *string         `json:"main_mode"`
	Drafts   []servedVersion `json:"drafts"`
}

func getTemplate(t *testing.T, app *fiber.App, path string) servedTemplate {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET %s = %d %s", path, response.StatusCode, body)
	}
	assertTemplateShape(t, body)
	var template servedTemplate
	if err := json.Unmarshal(body, &template); err != nil {
		t.Fatal(err)
	}
	return template
}

func draftIDs(drafts []servedVersion) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(drafts))
	for _, d := range drafts {
		ids = append(ids, d.ID)
	}
	return ids
}

// A template is served with what the template list and the editor need beside
// the row: the Authoring mode of the Main source it sends, and each operator's
// draft in progress, newest first — who is editing it, since when, and from
// which version. An operator's later version supersedes their earlier drafts;
// a published draft is no longer in progress; a draft that someone else's
// publish made stale still is, its base no longer the published version.
func TestTemplatesAreServedWithTheirMainModeAndDraftsInProgress(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)
	path := "/templates/" + created.ID.String()

	served := getTemplate(t, app, path)
	if served.MainMode == nil || *served.MainMode != "jsx" || served.Drafts == nil || len(served.Drafts) != 0 {
		t.Fatalf("template = main %v drafts %v, want jsx and an empty list", served.MainMode, served.Drafts)
	}

	save := func(subject, mode string, headers ...string) servedDraft {
		t.Helper()
		response, draft, body := saveDraft(t, app, created.ID, map[string]any{
			"subject": subject, "main_mode": mode, "html_source": "<p>" + subject + "</p>",
			"html_content": "<p>" + subject + "</p>", "plain_text_content": subject, "base_version_id": created.PublishedVersionID,
		}, headers...)
		if response.StatusCode != fiber.StatusCreated {
			t.Fatalf("save = %d %s", response.StatusCode, body)
		}
		return draft
	}
	save("Benim ilk taslağım", "jsx")
	mine := save("Benim ikinci taslağım", "html")
	theirs := save("Can'ın taslağı", "jsx", canDemir...)

	served = getTemplate(t, app, path)
	if ids := draftIDs(served.Drafts); fmt.Sprint(ids) != fmt.Sprint([]uuid.UUID{theirs.ID, mine.ID}) {
		t.Fatalf("drafts = %v, want Can's (%s) then my latest (%s)", ids, theirs.ID, mine.ID)
	}
	if d := served.Drafts[0]; !sameString(d.Author.Name, "Can Demir") || d.PublishedAt != nil || d.Current ||
		d.BaseVersionID == nil || *d.BaseVersionID != *created.PublishedVersionID || d.Subject != "Can'ın taslağı" {
		t.Fatalf("Can's draft = %+v", d)
	}
	if served.PublishedVersionID == nil || *served.PublishedVersionID != *created.PublishedVersionID || *served.MainMode != "jsx" {
		t.Fatalf("drafts changed what the template sends: %+v", served)
	}

	// My draft is published: it is sent, no longer in progress, and Can's is
	// now stale.
	response, body := sendJSON(t, app, fiber.MethodPost, path+"/versions/"+mine.ID.String()+"/publish", nil)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("publish = %d %s", response.StatusCode, body)
	}
	assertTemplateShape(t, body)
	var afterPublish servedTemplate
	if err := json.Unmarshal(body, &afterPublish); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]servedTemplate{"publish answer": afterPublish, "read": getTemplate(t, app, path)} {
		if got.MainMode == nil || *got.MainMode != "html" || fmt.Sprint(draftIDs(got.Drafts)) != fmt.Sprint([]uuid.UUID{theirs.ID}) ||
			*got.Drafts[0].BaseVersionID == *got.PublishedVersionID {
			t.Fatalf("%s after publishing = main %v drafts %+v, want html and only Can's draft, stale", name, got.MainMode, got.Drafts)
		}
	}

	// The list serves every template the same way.
	other := panelTemplate(t, app)
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates", nil))
	if err != nil {
		t.Fatal(err)
	}
	var list []servedTemplate
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]servedTemplate{}
	for _, template := range list {
		byID[template.ID] = template
	}
	if got := byID[created.ID]; got.MainMode == nil || *got.MainMode != "html" || fmt.Sprint(draftIDs(got.Drafts)) != fmt.Sprint([]uuid.UUID{theirs.ID}) {
		t.Fatalf("listed template = main %v drafts %v", got.MainMode, draftIDs(got.Drafts))
	}
	if got := byID[other.ID]; got.MainMode == nil || *got.MainMode != "jsx" || got.Drafts == nil || len(got.Drafts) != 0 {
		t.Fatalf("listed other template = main %v drafts %v, want jsx and none", got.MainMode, got.Drafts)
	}
}

// fieldsOf is the top-level fields of a JSON object, sorted.
func fieldsOf(t *testing.T, body []byte) []string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

// The draft routes answer with what /docs/openapi.json says they do.
func TestDraftResponsesAreServedAsDocumented(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	created := panelTemplate(t, app)

	response, draft, saved := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Taslak", "main_mode": "jsx", "html_content": "<p>Taslak</p>", "plain_text_content": "Taslak",
		"base_version_id": created.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("save = %d %s", response.StatusCode, saved)
	}
	_, _, unchanged := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Taslak", "main_mode": "jsx", "html_content": "<p>Taslak</p>", "plain_text_content": "Taslak",
		"base_version_id": created.PublishedVersionID,
	})
	_, _, restored := restore(t, app, created.ID, *created.PublishedVersionID)
	_, _, published := publish(t, app, created.ID, draft.ID, false)

	for _, tc := range []struct {
		path, status string
		served       []byte
	}{
		{"/templates/{id}/drafts", "201", saved},
		{"/templates/{id}/drafts", "200", unchanged},
		{"/templates/{id}/versions/{versionId}/restore", "201", restored},
		{"/templates/{id}/versions/{versionId}/publish", "200", published},
	} {
		documented, served := documentedResponseFields(t, "post", tc.path, tc.status), fieldsOf(t, tc.served)
		if fmt.Sprint(documented) != fmt.Sprint(served) {
			t.Errorf("POST %s %s documents %v but serves %v", tc.path, tc.status, documented, served)
		}
	}
}
