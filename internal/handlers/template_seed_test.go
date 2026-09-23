package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The Template seed and SkyMail's editor both write Mail templates, and
// neither silently overwrites the other (ADR-0047). These tests reach the
// seed's by-key upsert as the seed does.

// welcomeSeed is the Template seed's payload for core.welcome with the given
// body, sent as the repo's seed sends it today: the pointer comment where the
// JSX source goes.
func welcomeSeed(html string) map[string]any {
	return map[string]any{
		"name": "Hoş Geldin", "subject": "SKY LAB'e hoş geldin",
		"html_content": html, "plain_text_content": "Hoş geldin {{.FullName}}",
		"react_email_content": seedPointerComment("core.welcome"), "system": true,
	}
}

// seedAs runs the seed for one key, forced when force is true, and decodes the
// template it answers with when it answers with one.
func seedAs(t *testing.T, app *fiber.App, key string, payload map[string]any, force bool) (*http.Response, database.Template, []byte) {
	t.Helper()
	path := "/templates/by-key/" + key
	if force {
		path += "?force=true"
	}
	response, body := sendJSON(t, app, fiber.MethodPut, path, payload)
	var template database.Template
	if response.StatusCode == fiber.StatusOK {
		if err := json.Unmarshal(body, &template); err != nil {
			t.Fatalf("seed answer %s: %v", body, err)
		}
	}
	return response, template, body
}

// conflictVersion is a version as a seed conflict names it: its summary, as
// the history serves it.
type conflictVersion struct {
	ID      uuid.UUID `json:"id"`
	Seq     int       `json:"seq"`
	Subject string    `json:"subject"`
	Current bool      `json:"current"`
	Author  struct {
		Kind string  `json:"kind"`
		Sub  *string `json:"sub"`
		Name *string `json:"name"`
	} `json:"author"`
	PublishedAt *time.Time `json:"published_at"`
}

// seedConflict is the body of a refused seed.
type seedConflict struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Params  struct {
		Key              string            `json:"key"`
		TemplateID       uuid.UUID         `json:"template_id"`
		Rules            []string          `json:"rules"`
		PublishedVersion *conflictVersion  `json:"published_version"`
		LastSeedVersion  *conflictVersion  `json:"last_seed_version"`
		OperatorVersions []conflictVersion `json:"operator_versions"`
		Subject          string            `json:"subject"`
		RequestedSubject string            `json:"requested_subject"`
	} `json:"params"`
}

// refusedSeed runs the seed for one key and requires it to be refused.
func refusedSeed(t *testing.T, app *fiber.App, key string, payload map[string]any) seedConflict {
	t.Helper()
	response, _, body := seedAs(t, app, key, payload, false)
	var conflict seedConflict
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatalf("seed answer %s: %v", body, err)
	}
	if response.StatusCode != fiber.StatusConflict || conflict.Code != "template.seed_conflict" {
		t.Fatalf("seed = %d %s, want 409 template.seed_conflict", response.StatusCode, body)
	}
	return conflict
}

// withoutField is a JSON object without one of its fields.
func withoutField(t *testing.T, body []byte, field string) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	delete(object, field)
	trimmed, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return trimmed
}

func sameRules(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// An operator's draft newer than the last seed is their work in progress.
// A seed that would change the template is refused, naming the template, the
// rule and the draft, and writes nothing: the row, which is what is sent, and
// the history stay as they were.
func TestASeedIsRefusedWhileAnOperatorsDraftIsNewerThanTheLastSeed(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "Aramıza hoş geldin", "main_mode": "html", "html_source": "<p>Aramıza hoş geldin {{.FullName}}</p>",
		"html_content": "<p>Aramıza hoş geldin {{.FullName}}</p>", "plain_text_content": "Aramıza hoş geldin {{.FullName}}",
		"base_version_id": seeded.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	before := templateRow(t, db, seeded.ID)
	versionsBefore, _ := storedVersions(t, db, seeded.ID)

	conflict := refusedSeed(t, app, key, welcomeSeed("<p>Koyu temalı hoş geldin {{.FullName}}</p>"))

	p := conflict.Params
	if p.Key != key || p.TemplateID != seeded.ID || !sameRules(p.Rules, "newer_operator_version") {
		t.Fatalf("conflict names %s %s %v, want %s %s [newer_operator_version]", p.Key, p.TemplateID, p.Rules, key, seeded.ID)
	}
	if len(p.OperatorVersions) != 1 || p.OperatorVersions[0].ID != draft.ID || p.OperatorVersions[0].Seq != 2 ||
		p.OperatorVersions[0].PublishedAt != nil || p.OperatorVersions[0].Author.Kind != "operator" ||
		p.OperatorVersions[0].Subject != "Aramıza hoş geldin" || p.OperatorVersions[0].Current ||
		!sameString(p.OperatorVersions[0].Author.Name, operatorName) {
		t.Fatalf("conflict operator_versions = %+v, want the draft, version 2 by %s, unpublished", p.OperatorVersions, operatorName)
	}
	if p.LastSeedVersion == nil || p.LastSeedVersion.ID != *seeded.PublishedVersionID || p.LastSeedVersion.Author.Kind != "template_seed" ||
		p.PublishedVersion == nil || p.PublishedVersion.ID != *seeded.PublishedVersionID {
		t.Fatalf("conflict last seed %+v, published %+v; want both the seed's version 1", p.LastSeedVersion, p.PublishedVersion)
	}

	after := templateRow(t, db, seeded.ID)
	if after.HtmlContent != before.HtmlContent || after.Subject != before.Subject || after.UpdatedAt != before.UpdatedAt ||
		*after.PublishedVersionID != *before.PublishedVersionID {
		t.Fatalf("a refused seed changed the row: %+v", after)
	}
	if versions, _ := storedVersions(t, db, seeded.ID); len(versions) != len(versionsBefore) {
		t.Fatalf("a refused seed recorded a version: %d versions, want %d", len(versions), len(versionsBefore))
	}
}

// Forcing a seed writes it over an operator's change, published at once like
// any seed. The operator's work is not lost: their published version and their
// draft stay in the history, and either can be restored as a new draft.
func TestAForcedSeedIsPublishedAndTheOperatorsWorkStaysRestorable(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	// An operator rewords the mail in the old panel, which publishes at once…
	response, body := oldPanelSave(t, app, templateRow(t, db, seeded.ID), "Aramıza hoş geldin")
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("old panel edit = %d %s", response.StatusCode, body)
	}
	edited := templateRow(t, db, seeded.ID)
	// …and starts a draft on top.
	response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "Aramıza hoş geldin, {{.FullName}}", "main_mode": "html", "html_source": "<p>Taslak {{.FullName}}</p>",
		"html_content": "<p>Taslak {{.FullName}}</p>", "plain_text_content": "Taslak {{.FullName}}",
		"base_version_id": edited.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}

	payload := welcomeSeed("<p>Koyu temalı hoş geldin {{.FullName}}</p>")
	refused := refusedSeed(t, app, key, payload)
	response, forced, body := seedAs(t, app, key, payload, true)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("forced seed = %d %s, want 200", response.StatusCode, body)
	}
	// It says what it wrote over — what the refusal named — and is otherwise
	// the template.
	assertTemplateShape(t, withoutField(t, body, "overrode"))
	var answer struct {
		Overrode *struct {
			Rules            []string          `json:"rules"`
			PublishedVersion *conflictVersion  `json:"published_version"`
			OperatorVersions []conflictVersion `json:"operator_versions"`
		} `json:"overrode"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	o := answer.Overrode
	if o == nil || !sameRules(o.Rules, refused.Params.Rules...) || !sameRules(o.Rules, "published_by_operator", "newer_operator_version", "operator_subject") ||
		o.PublishedVersion == nil || o.PublishedVersion.ID != *edited.PublishedVersionID || o.PublishedVersion.Current ||
		len(o.OperatorVersions) != 2 || o.OperatorVersions[0].ID != *edited.PublishedVersionID || o.OperatorVersions[1].ID != draft.ID {
		t.Fatalf("forced seed overrode = %+v, want the rules, the operator's published edit and both their versions", o)
	}

	versions, published := storedVersions(t, db, seeded.ID)
	if len(versions) != 4 {
		t.Fatalf("versions = %d, want seed, edit, draft, forced seed", len(versions))
	}
	seed := versions[3]
	if seed.AuthorKind != "template_seed" || seed.PublishedAt == nil || published == nil || *published != seed.ID ||
		forced.PublishedVersionID == nil || *forced.PublishedVersionID != seed.ID {
		t.Fatalf("forced seed version = %+v (row copy of %v), want a published Template seed version, sent", seed, published)
	}
	if seed.BaseVersionID == nil || *seed.BaseVersionID != *edited.PublishedVersionID {
		t.Fatalf("forced seed base = %v, want the operator's version it replaced, %v", seed.BaseVersionID, edited.PublishedVersionID)
	}
	row := templateRow(t, db, seeded.ID)
	if row.HtmlContent != payload["html_content"] || row.Subject != payload["subject"] {
		t.Fatalf("row after the forced seed = %q %q, want the seed's", row.Subject, row.HtmlContent)
	}
	if versions[1].ID != *edited.PublishedVersionID || versions[1].PublishedAt == nil || versions[2].ID != draft.ID || versions[2].PublishedAt != nil {
		t.Fatalf("the operator's versions changed: %+v", versions[1:3])
	}

	// Forced again with nothing of an operator's in the way, it overrides
	// nothing and says nothing of it.
	for name, forcedAgain := range map[string]map[string]any{
		"the same content": payload,
		"new content":      welcomeSeed("<p>Koyu tema ve logo {{.FullName}}</p>"),
	} {
		response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key+"?force=true", forcedAgain)
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("forcing %s again = %d %s", name, response.StatusCode, body)
		}
		assertTemplateShape(t, body)
	}
	versions, _ = storedVersions(t, db, seeded.ID)

	for name, version := range map[string]uuid.UUID{"their published edit": versions[1].ID, "their draft": draft.ID} {
		response, restored, body := restore(t, app, seeded.ID, version)
		if response.StatusCode != fiber.StatusCreated {
			t.Fatalf("restoring %s = %d %s, want 201", name, response.StatusCode, body)
		}
		var original storedVersion
		for _, v := range versions {
			if v.ID == version {
				original = v
			}
		}
		if restored.Subject != original.Subject || restored.HTMLContent != original.HTMLContent || restored.PublishedAt != nil ||
			restored.BaseVersionID == nil || *restored.BaseVersionID != versions[len(versions)-1].ID {
			t.Fatalf("restored %s = %+v, want its content as a draft on the forced seed", name, restored.servedVersion)
		}
	}
}

// What a template sends is the operator's when they published after the last
// seed. That holds even when the version they published was written before
// that seed — a draft started earlier and published over the seed with force,
// since publishing does not renumber a draft — so the rule looks at what is
// sent, not at version numbers or publish times.
func TestASeedIsRefusedWhenAnOperatorPublishedAfterTheLastSeed(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	// Version 2: a draft, started on the first seed.
	response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "Aramıza hoş geldin", "main_mode": "html", "html_source": "<p>Aramıza hoş geldin {{.FullName}}</p>",
		"html_content": "<p>Aramıza hoş geldin {{.FullName}}</p>", "plain_text_content": "Aramıza hoş geldin {{.FullName}}",
		"base_version_id": seeded.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	// Version 3: a seed forced over it.
	if response, _, body := seedAs(t, app, key, welcomeSeed("<p>Koyu tema {{.FullName}}</p>"), true); response.StatusCode != fiber.StatusOK {
		t.Fatalf("forced seed = %d %s", response.StatusCode, body)
	}
	lastSeed := *templateRow(t, db, seeded.ID).PublishedVersionID
	// The operator publishes their draft over it, having seen it.
	if response, _, body := publish(t, app, seeded.ID, draft.ID, &lastSeed); response.StatusCode != fiber.StatusOK {
		t.Fatalf("forced publish = %d %s", response.StatusCode, body)
	}

	// Their draft reworded the subject too, and the seed would put the repo's
	// back: that is named as well.
	conflict := refusedSeed(t, app, key, welcomeSeed("<p>Koyu tema ve logo {{.FullName}}</p>"))
	p := conflict.Params
	if !sameRules(p.Rules, "published_by_operator", "operator_subject") || len(p.OperatorVersions) != 0 {
		t.Fatalf("conflict rules %v, operator versions %+v; want [published_by_operator operator_subject] and none newer than the seed", p.Rules, p.OperatorVersions)
	}
	if p.PublishedVersion == nil || p.PublishedVersion.ID != draft.ID || p.PublishedVersion.Seq != 2 || p.PublishedVersion.PublishedAt == nil ||
		p.PublishedVersion.Author.Kind != "operator" || p.LastSeedVersion == nil || p.LastSeedVersion.ID != lastSeed || p.LastSeedVersion.Seq != 3 {
		t.Fatalf("conflict published %+v, last seed %+v; want the operator's version 2 sent over the seed's version 3", p.PublishedVersion, p.LastSeedVersion)
	}

	// An edit in the old panel publishes at once, and it is a newer version too.
	other := seededTemplate(t, app, "core.certificate")
	if response, body := oldPanelSave(t, app, templateRow(t, db, other.ID), "Sertifikan hazır"); response.StatusCode != fiber.StatusOK {
		t.Fatalf("old panel edit = %d %s", response.StatusCode, body)
	}
	payload := welcomeSeed("<p>Koyu tema {{.FullName}}</p>")
	conflict = refusedSeed(t, app, "core.certificate", payload)
	if !sameRules(conflict.Params.Rules, "published_by_operator", "newer_operator_version", "operator_subject") {
		t.Fatalf("after an old panel edit of the subject, rules = %v, want all three", conflict.Params.Rules)
	}
}

// Until this rule, a seed kept the subject a template already had (#18) and
// recorded the one it asked for beside it. A last seed version whose subject is
// not the one it asked for kept an operator's wording, and the subject is
// written again now, so the next seed would overwrite that wording: it is
// refused, naming the subject sent and the one the seed asks for.
func TestASeedIsRefusedWhenTheLastSeedKeptAnOperatorsSubject(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	ctx := context.Background()
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	// An operator rewords the subject in the old panel (version 2)…
	if response, body := oldPanelSave(t, app, templateRow(t, db, seeded.ID), "Aramıza hoş geldin"); response.StatusCode != fiber.StatusOK {
		t.Fatalf("old panel edit = %d %s", response.StatusCode, body)
	}
	// …and a seed from before this rule brings a body fix, keeping their
	// subject (version 3): the row write and the version it recorded.
	if _, err := db.Conn.Exec(ctx, `UPDATE templates SET html_content = '<p>Koyu tema {{.FullName}}</p>', updated_at = NOW() WHERE id = $1`, seeded.ID); err != nil {
		t.Fatal(err)
	}
	repoSubject := "SKY LAB'e hoş geldin"
	if _, err := db.RecordTemplateRowAsVersion(ctx, database.RecordTemplateRowAsVersionParams{
		TemplateID: seeded.ID, AuthorKind: database.TemplateAuthorKindTemplateSeed, RequestedSubject: &repoSubject,
	}); err != nil {
		t.Fatal(err)
	}
	versions, _ := storedVersions(t, db, seeded.ID)
	if len(versions) != 3 || versions[2].Subject != "Aramıza hoş geldin" || !sameString(versions[2].RequestedSubject, repoSubject) {
		t.Fatalf("history = %+v, want a last seed version that kept the operator's subject", versions)
	}

	conflict := refusedSeed(t, app, key, welcomeSeed("<p>Koyu tema ve logo {{.FullName}}</p>"))
	p := conflict.Params
	if !sameRules(p.Rules, "operator_subject") {
		t.Fatalf("rules = %v, want [operator_subject]", p.Rules)
	}
	if p.Subject != "Aramıza hoş geldin" || p.RequestedSubject != repoSubject || p.LastSeedVersion == nil || p.LastSeedVersion.ID != versions[2].ID {
		t.Fatalf("conflict subject %q, requested %q, last seed %+v; want the operator's subject, the repo's and version 3", p.Subject, p.RequestedSubject, p.LastSeedVersion)
	}
	if row := templateRow(t, db, seeded.ID); row.Subject != "Aramıza hoş geldin" {
		t.Fatalf("subject after a refused seed = %q, want the operator's", row.Subject)
	}

	// The repo takes up the operator's wording: nothing of theirs is left for
	// the seed to overwrite, so its body fix goes through.
	adopted := welcomeSeed("<p>Koyu tema ve logo {{.FullName}}</p>")
	adopted["subject"] = "Aramıza hoş geldin"
	if response, reseeded, body := seedAs(t, app, key, adopted, false); response.StatusCode != fiber.StatusOK ||
		reseeded.HtmlContent != adopted["html_content"] || reseeded.Subject != "Aramıza hoş geldin" {
		t.Fatalf("a seed asking for the operator's subject = %d %s, want it written", response.StatusCode, body)
	}
}

// The migration that started keeping versions (20260923120000) gave every
// template one published first version from its row: a Template seed's when
// the row had a key. What the seed asked for then is not known, so the version
// has no requested_subject.
const templateVersionsMigration = "20260923120000"

// migratedTemplates is a store whose templates were written before versions
// were kept — a seeded one per key, with the subject given — and whose history
// therefore starts with the migration's first version.
func migratedTemplates(t *testing.T, subjects map[string]string) (*database.Store, map[string]uuid.UUID) {
	t.Helper()
	pool := testpostgres.Start(t)
	applyMigrationFilesWhere(t, pool, func(file string) bool { return file < templateVersionsMigration })
	ids := make(map[string]uuid.UUID, len(subjects))
	for key, subject := range subjects {
		var id uuid.UUID
		if err := pool.QueryRow(context.Background(), `
			INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, system)
			VALUES ($1, 'Hoş Geldin', $2, '<p>Eski gövde {{.FullName}}</p>', 'Eski gövde {{.FullName}}', $3, true)
			RETURNING id`, key, subject, seedPointerComment(key)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[key] = id
	}
	applyMigrationFilesWhere(t, pool, func(file string) bool { return file >= templateVersionsMigration })
	return database.NewStore(pool), ids
}

// A template whose history is the migration's first version only was last
// written by the seed, so the first seed after the migration brings the repo's
// changes to it without a conflict, as it does to any template no operator has
// touched.
func TestATemplateWithOnlyTheMigrationsFirstVersionSeedsWithoutConflict(t *testing.T) {
	db, ids := migratedTemplates(t, map[string]string{"core.welcome": "SKY LAB'e hoş geldin"})
	app := templateVersionsApp(t, db)

	payload := welcomeSeed("<p>Koyu tema {{.FullName}}</p>")
	response, seeded, body := seedAs(t, app, "core.welcome", payload, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("first seed after the migration = %d %s, want 200", response.StatusCode, body)
	}
	versions, published := storedVersions(t, db, ids["core.welcome"])
	if len(versions) != 2 || versions[0].AuthorSub != nil || versions[0].RequestedSubject != nil ||
		versions[1].AuthorKind != "template_seed" || published == nil || *published != versions[1].ID || seeded.HtmlContent != payload["html_content"] {
		t.Fatalf("history = %+v, want the migration's first version and the seed's, published", versions)
	}
}

// The subject a migrated template sends may be an operator's: before versions
// were kept, a seed left the subject alone (#18), and nothing recorded whose it
// is. A seed that would change it is refused, as if the migration's version had
// kept it; one that leaves it as it is goes through.
func TestASeedIsRefusedWhenItWouldChangeAMigratedTemplatesSubject(t *testing.T) {
	db, ids := migratedTemplates(t, map[string]string{"core.welcome": "Aramıza hoş geldin"})
	app := templateVersionsApp(t, db)

	conflict := refusedSeed(t, app, "core.welcome", welcomeSeed("<p>Koyu tema {{.FullName}}</p>"))
	p := conflict.Params
	if !sameRules(p.Rules, "operator_subject") || p.Subject != "Aramıza hoş geldin" || p.RequestedSubject != "SKY LAB'e hoş geldin" {
		t.Fatalf("conflict rules %v, subject %q, requested %q; want [operator_subject] with the row's subject and the repo's", p.Rules, p.Subject, p.RequestedSubject)
	}
	if versions, _ := storedVersions(t, db, ids["core.welcome"]); len(versions) != 1 {
		t.Fatalf("a refused seed recorded a version: %d", len(versions))
	}

	payload := welcomeSeed("<p>Koyu tema {{.FullName}}</p>")
	payload["subject"] = "Aramıza hoş geldin"
	if response, _, body := seedAs(t, app, "core.welcome", payload, false); response.StatusCode != fiber.StatusOK {
		t.Fatalf("a seed keeping the subject = %d %s, want 200", response.StatusCode, body)
	}
}

// A discarded draft is nobody's work in progress: it stays in the history, but
// it does not hold a seed back, or a draft someone walked away from would stop
// every seed for good.
func TestADiscardedDraftDoesNotHoldASeedBack(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "Yarım kalan", "main_mode": "html", "html_source": "<p>Yarım {{.FullName}}</p>",
		"html_content": "<p>Yarım {{.FullName}}</p>", "plain_text_content": "Yarım {{.FullName}}",
		"base_version_id": seeded.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	if response, _, body := discard(t, app, seeded.ID, draft.ID); response.StatusCode != fiber.StatusOK {
		t.Fatalf("discard = %d %s", response.StatusCode, body)
	}

	payload := welcomeSeed("<p>Koyu tema {{.FullName}}</p>")
	response, reseeded, body := seedAs(t, app, key, payload, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed after the draft was discarded = %d %s, want 200", response.StatusCode, body)
	}
	versions, _ := storedVersions(t, db, seeded.ID)
	if len(versions) != 3 || versions[1].ID != draft.ID || reseeded.PublishedVersionID == nil || *reseeded.PublishedVersionID != versions[2].ID {
		t.Fatalf("history = %+v, want the discarded draft kept and the seed published after it", versions)
	}
}

// A seed that would leave a template as its published version already is
// overwrites nothing, so it is no conflict whatever operators did: it answers
// 200 and records nothing, and a draft in progress stays fresh. That is also
// how the repo catches up with an operator's change: once it holds the same
// content, the seed goes through again.
func TestASeedThatChangesNothingIsNoConflict(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
		"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak {{.FullName}}</p>",
		"html_content": "<p>Taslak {{.FullName}}</p>", "plain_text_content": "Taslak {{.FullName}}",
		"base_version_id": seeded.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	unchanged := map[string]any{
		"name": "Hoş Geldin", "subject": "SKY LAB'e hoş geldin",
		"html_content": "<p>Hoş geldin {{.FullName}}</p>", "plain_text_content": "Hoş geldin {{.FullName}}",
		"react_email_content": seedPointerComment(key), "system": true,
	}
	response, answered, body := seedAs(t, app, key, unchanged, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("unchanged seed beside a draft = %d %s, want 200", response.StatusCode, body)
	}
	if versions, _ := storedVersions(t, db, seeded.ID); len(versions) != 2 || answered.PublishedVersionID == nil || *answered.PublishedVersionID != *seeded.PublishedVersionID {
		t.Fatalf("unchanged seed: %d versions, published %v; want nothing recorded", len(versions), answered.PublishedVersionID)
	}
	if response, published, body := publish(t, app, seeded.ID, draft.ID, nil); response.StatusCode != fiber.StatusOK || *published.PublishedVersionID != draft.ID {
		t.Fatalf("publishing the draft after an unchanged seed = %d %s, want it published: the seed did not make it stale", response.StatusCode, body)
	}

	// The repo takes up the operator's wording: the seed holds what they
	// published and goes through, writing nothing.
	unchanged["subject"] = "Taslak"
	unchanged["html_content"] = "<p>Taslak {{.FullName}}</p>"
	unchanged["plain_text_content"] = "Taslak {{.FullName}}"
	response, answered, body = seedAs(t, app, key, unchanged, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("a seed holding the operator's published content = %d %s, want 200", response.StatusCode, body)
	}
	if versions, _ := storedVersions(t, db, seeded.ID); len(versions) != 2 || *answered.PublishedVersionID != draft.ID {
		t.Fatalf("seed of the published content: %d versions, published %v; want nothing recorded", len(versions), answered.PublishedVersionID)
	}
}

// A refused seed writes no version, but the template keeps that it was
// refused, so the panel can say a repo change is waiting on a decision: when
// the content was first refused, the rules that held, and a hash of what the
// seed asked to write. Refusing the same content again keeps the date; other
// content restarts it. The next seed that goes through clears it, whether
// nothing conflicts any more or it was forced.
func TestARefusedSeedIsKeptOnTheTemplateUntilASeedGoesThrough(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)
	path := "/templates/" + seeded.ID.String()
	if refusal := getTemplate(t, app, path).SeedRefusal; refusal != nil {
		t.Fatalf("a template never refused is served with seed_refusal %+v", refusal)
	}

	draftOn := func(base *uuid.UUID) servedDraft {
		t.Helper()
		response, draft, body := saveDraft(t, app, seeded.ID, map[string]any{
			"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak {{.FullName}}</p>",
			"html_content": "<p>Taslak {{.FullName}}</p>", "plain_text_content": "Taslak {{.FullName}}", "base_version_id": base,
		})
		if response.StatusCode != fiber.StatusCreated {
			t.Fatalf("draft = %d %s", response.StatusCode, body)
		}
		return draft
	}
	draft := draftOn(seeded.PublishedVersionID)

	darkTheme := welcomeSeed("<p>Koyu tema {{.FullName}}</p>")
	refusedSeed(t, app, key, darkTheme)
	first := getTemplate(t, app, path).SeedRefusal
	if first == nil || !sameRules(first.Rules, "newer_operator_version") || len(first.PayloadSHA256) != 64 || first.RefusedAt.IsZero() {
		t.Fatalf("seed_refusal after a refused seed = %+v, want its time, [newer_operator_version] and a sha256", first)
	}
	if byKey := getTemplate(t, app, "/templates/by-key/"+key).SeedRefusal; byKey == nil || !byKey.RefusedAt.Equal(first.RefusedAt) {
		t.Fatalf("the template by key is served with seed_refusal %+v, want %+v", byKey, first)
	}
	row := templateRow(t, db, seeded.ID)
	if row.UpdatedAt != seeded.UpdatedAt {
		t.Fatalf("recording a refusal moved updated_at: %v, was %v", row.UpdatedAt, seeded.UpdatedAt)
	}

	refusedSeed(t, app, key, darkTheme)
	if again := getTemplate(t, app, path).SeedRefusal; again == nil || !again.RefusedAt.Equal(first.RefusedAt) || again.PayloadSHA256 != first.PayloadSHA256 {
		t.Fatalf("the same content refused again = %+v, want the first refusal's time and hash, %+v", again, first)
	}
	refusedSeed(t, app, key, welcomeSeed("<p>Koyu tema ve logo {{.FullName}}</p>"))
	other := getTemplate(t, app, path).SeedRefusal
	if other == nil || !other.RefusedAt.After(first.RefusedAt) || other.PayloadSHA256 == first.PayloadSHA256 {
		t.Fatalf("other content refused = %+v, want a later time and another hash than %+v", other, first)
	}

	// The operator gives the draft up; nothing conflicts, and the seed goes through.
	if response, _, body := discard(t, app, seeded.ID, draft.ID); response.StatusCode != fiber.StatusOK {
		t.Fatalf("discard = %d %s", response.StatusCode, body)
	}
	if response, _, body := seedAs(t, app, key, darkTheme, false); response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	if refusal := getTemplate(t, app, path).SeedRefusal; refusal != nil {
		t.Fatalf("seed_refusal after a seed went through = %+v, want null", refusal)
	}

	// Refused again, then forced.
	draftOn(templateRow(t, db, seeded.ID).PublishedVersionID)
	refusedSeed(t, app, key, welcomeSeed("<p>Logo {{.FullName}}</p>"))
	if response, _, body := seedAs(t, app, key, welcomeSeed("<p>Logo {{.FullName}}</p>"), true); response.StatusCode != fiber.StatusOK {
		t.Fatalf("forced seed = %d %s", response.StatusCode, body)
	}
	if refusal := getTemplate(t, app, path).SeedRefusal; refusal != nil {
		t.Fatalf("seed_refusal after a forced seed = %+v, want null", refusal)
	}
}

// The subject is part of what the seed writes again (#18's exception is gone):
// with no operator change in the way, a subject fixed in the repo reaches the
// template like a body fix does, and the version records it as both the
// subject sent and the one asked for.
func TestASeedWritesTheSubjectWhenNothingConflicts(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	payload := welcomeSeed("<p>Hoş geldin {{.FullName}}</p>")
	payload["subject"] = "SKY LAB ekosistemine hoş geldin"
	response, reseeded, body := seedAs(t, app, key, payload, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed with a new subject = %d %s", response.StatusCode, body)
	}
	if reseeded.Subject != "SKY LAB ekosistemine hoş geldin" || templateRow(t, db, seeded.ID).Subject != "SKY LAB ekosistemine hoş geldin" {
		t.Fatalf("subject after the seed = %q, want the repo's new one", reseeded.Subject)
	}
	versions, published := storedVersions(t, db, seeded.ID)
	latest := versions[len(versions)-1]
	if len(versions) != 2 || *published != latest.ID || latest.Subject != "SKY LAB ekosistemine hoş geldin" ||
		!sameString(latest.RequestedSubject, "SKY LAB ekosistemine hoş geldin") {
		t.Fatalf("history = %+v, want a seed version with the new subject, asked for and sent", versions)
	}
}

// The seed sends each template's real .tsx source, so the panel's JSX mode can
// open it: the version keeps it as the JSX source and JSX is its Main source.
// The seed on main still sends a pointer comment there, which is no source, so
// its body stays the HTML Main source — and a pointer after a real source keeps
// that source beside it.
func TestTheSeedsJSXSourceIsTheMainSource(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	source := `import { Text } from "@react-email/components";
import { v } from "./go";

export const meta = { key: "core.welcome" };

export default function CoreWelcome() {
  return <Text>Hoş geldin {v("FullName")}</Text>;
}
`
	payload := welcomeSeed("<p>Hoş geldin {{.FullName}}</p>")
	payload["react_email_content"] = source
	response, seeded, body := seedAs(t, app, key, payload, false)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed with the real source = %d %s", response.StatusCode, body)
	}
	if seeded.ReactEmailContent != source {
		t.Fatalf("react_email_content = %q, want the source", seeded.ReactEmailContent)
	}
	versions, _ := storedVersions(t, db, seeded.ID)
	if len(versions) != 1 || versions[0].MainMode != "jsx" || !sameString(versions[0].JSXSource, source) || versions[0].HTMLSource != nil ||
		versions[0].HTMLContent != payload["html_content"] {
		t.Fatalf("seed version = %+v, want the source as the JSX Main source and its render", versions)
	}
	if served := getTemplate(t, app, "/templates/"+seeded.ID.String()); served.MainMode == nil || *served.MainMode != "jsx" {
		t.Fatalf("served main_mode = %v, want jsx", served.MainMode)
	}

	// main's seed runs with a new body and its pointer.
	pointer := welcomeSeed("<p>Koyu tema {{.FullName}}</p>")
	if response, _, body := seedAs(t, app, key, pointer, false); response.StatusCode != fiber.StatusOK {
		t.Fatalf("pointer seed = %d %s", response.StatusCode, body)
	}
	versions, _ = storedVersions(t, db, seeded.ID)
	if latest := versions[len(versions)-1]; len(versions) != 2 || latest.MainMode != "html" || !sameString(latest.HTMLSource, "<p>Koyu tema {{.FullName}}</p>") ||
		!sameString(latest.JSXSource, source) {
		t.Fatalf("pointer seed version = %+v, want the body as the HTML Main source and the JSX source kept", latest)
	}
}

// force is a boolean; anything else is refused before anything is read.
func TestASeedsForceMustBeABoolean(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/core.welcome?force=evet", welcomeSeed("<p>Hoş geldin {{.FullName}}</p>"))
	if e := decodeError(t, body); response.StatusCode != fiber.StatusBadRequest || e.Code != "validation.error" || !namesField(e, "force") {
		t.Fatalf("force=evet = %d %s, want 400 validation.error on force", response.StatusCode, body)
	}
	if _, err := db.GetTemplateByKey(context.Background(), ptrTo("core.welcome")); err == nil {
		t.Fatal("a refused request wrote the template")
	}
}

func ptrTo(s string) *string { return &s }

// A seed and an operator's edit that meet take the template's lock in turn,
// and the seed judges the template only once it holds it. Whichever comes
// first, the operator's edit is what the template ends up sending: after the
// seed, it simply replaces it; before it, the seed sees it and is refused. A
// seed that judged the template before the edit and wrote after it would
// overwrite the edit with nothing said.
func TestASeedAndAnOperatorsEditThatMeetTakeTurns(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	const rounds = 12
	keys := make([]string, rounds)
	rows := make([]database.Template, rounds)
	for i := range keys {
		keys[i] = fmt.Sprintf("event.round-%02d", i)
		payload := welcomeSeed("<p>İlk {{.FullName}}</p>")
		response, seeded, body := seedAs(t, app, keys[i], payload, false)
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("seed = %d %s", response.StatusCode, body)
		}
		rows[i] = seeded
	}

	calls := make([]call, 0, 2*rounds)
	for i, row := range rows {
		calls = append(calls,
			call{"seed " + keys[i], fiber.MethodPut, "/templates/by-key/" + keys[i], welcomeSeed("<p>Koyu tema {{.FullName}}</p>"), nil},
			call{"edit " + keys[i], fiber.MethodPatch, "/templates/" + row.ID.String(), map[string]any{
				"name": row.Name, "subject": "Operatörün konusu", "html_content": "<p>Operatörün gövdesi {{.FullName}}</p>",
				"plain_text_content": row.PlainTextContent, "react_email_content": row.ReactEmailContent, "key": keys[i],
			}, nil},
		)
	}
	answers := concurrently(app, calls...)
	for i, row := range rows {
		seed, edit := answers[2*i], answers[2*i+1]
		if seed.err != nil || edit.err != nil || edit.status != fiber.StatusOK {
			t.Fatalf("%s = %d %v, %s = %d %s %v", seed.name, seed.status, seed.err, edit.name, edit.status, edit.body, edit.err)
		}
		if seed.status != fiber.StatusOK && seed.status != fiber.StatusConflict {
			t.Fatalf("%s = %d %s, want 200 or 409", seed.name, seed.status, seed.body)
		}
		if sent := templateRow(t, db, row.ID); sent.Subject != "Operatörün konusu" || sent.HtmlContent != "<p>Operatörün gövdesi {{.FullName}}</p>" {
			t.Fatalf("%s (%d) and %s met, and the template sends %q %q: the operator's edit was overwritten", seed.name, seed.status, edit.name, sent.Subject, sent.HtmlContent)
		}
	}
}

// A template's name is part of its versions, so renaming one in the old panel
// is an operator's change like any other: a seed after it is refused rather
// than quietly putting the repo's name back, and forced, it leaves the rename
// in the history, where restoring and publishing it brings the name back.
func TestAnOperatorsRenameHoldsASeedBackAndStaysRestorable(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "core.welcome"
	seeded := seededTemplate(t, app, key)

	row := templateRow(t, db, seeded.ID)
	response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), map[string]any{
		"name": "Karşılama (Core)", "subject": row.Subject, "key": key,
		"html_content": row.HtmlContent, "plain_text_content": row.PlainTextContent, "react_email_content": row.ReactEmailContent,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("rename = %d %s", response.StatusCode, body)
	}
	versions, _ := storedVersions(t, db, seeded.ID)
	if len(versions) != 2 || versions[1].AuthorKind != "operator" || versions[1].Name != "Karşılama (Core)" || versions[0].Name != "Hoş Geldin" {
		t.Fatalf("history after a rename = %+v, want the seed's version and the operator's, each with its name", versions)
	}
	rename := versions[1]

	unchanged := welcomeSeed("<p>Hoş geldin {{.FullName}}</p>")
	conflict := refusedSeed(t, app, key, unchanged)
	if !sameRules(conflict.Params.Rules, "published_by_operator", "newer_operator_version") ||
		conflict.Params.PublishedVersion == nil || conflict.Params.PublishedVersion.ID != rename.ID {
		t.Fatalf("seed after a rename: rules %v, published %+v; want the rename named, published by an operator", conflict.Params.Rules, conflict.Params.PublishedVersion)
	}
	if name := templateRow(t, db, seeded.ID).Name; name != "Karşılama (Core)" {
		t.Fatalf("name after a refused seed = %q, want the operator's", name)
	}

	if response, _, body := seedAs(t, app, key, unchanged, true); response.StatusCode != fiber.StatusOK {
		t.Fatalf("forced seed = %d %s", response.StatusCode, body)
	}
	if name := templateRow(t, db, seeded.ID).Name; name != "Hoş Geldin" {
		t.Fatalf("name after a forced seed = %q, want the repo's", name)
	}
	response, restored, body := restore(t, app, seeded.ID, rename.ID)
	if response.StatusCode != fiber.StatusCreated || restored.Name != "Karşılama (Core)" {
		t.Fatalf("restoring the rename = %d %s, want a draft with the operator's name", response.StatusCode, body)
	}
	if response, published, body := publish(t, app, seeded.ID, restored.ID, nil); response.StatusCode != fiber.StatusOK || published.Name != "Karşılama (Core)" {
		t.Fatalf("publishing the restored rename = %d %s, want the operator's name back on the template", response.StatusCode, body)
	}
}

// A keyed template an operator made in the old panel has no Template seed
// version at all: everything it holds is an operator's. The first seed of that
// key is refused, naming no last seed version and the operator's version.
func TestASeedIsRefusedForAKeyedTemplateMadeInThePanel(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	const key = "event.reminder"
	created := createTemplate(t, app, map[string]any{
		"name": "Hatırlatma", "subject": "Yarın: {{.EventName}}", "key": key,
		"html_content": "<p>Yarın {{.EventName}}</p>", "plain_text_content": "Yarın {{.EventName}}",
		"react_email_content": panelSource,
	})

	payload := map[string]any{
		"name": "Etkinlik · Hatırlatma", "subject": "Yarın: {{.EventName}}",
		"html_content": "<p>Yarın {{.EventName}} başlıyor</p>", "plain_text_content": "Yarın {{.EventName}} başlıyor",
		"react_email_content": seedPointerComment(key), "system": false,
	}
	conflict := refusedSeed(t, app, key, payload)
	p := conflict.Params
	if p.TemplateID != created.ID || !sameRules(p.Rules, "published_by_operator", "newer_operator_version") || p.LastSeedVersion != nil ||
		p.PublishedVersion == nil || p.PublishedVersion.ID != *created.PublishedVersionID || p.PublishedVersion.Author.Kind != "operator" ||
		len(p.OperatorVersions) != 1 || p.OperatorVersions[0].ID != *created.PublishedVersionID {
		t.Fatalf("seed of a panel-made key = %+v, want both rules, no last seed version and the operator's first version", p)
	}
	if row := templateRow(t, db, created.ID); row.Name != "Hatırlatma" || row.HtmlContent != "<p>Yarın {{.EventName}}</p>" {
		t.Fatalf("a refused seed changed the panel's template: %+v", row)
	}
}
