package database

import (
	"context"
	"testing"
)

// ADR-0045 moved Keycloak's system mail into SkyMail so that a wording change
// would stop costing a release. The seed runs often — twice on the day this was
// written — so an upsert that wrote the repo's subject over the row took that
// back silently: the operator's change was there until the next seed and then
// was not, with nothing said. The repo seeds the subject once and the row owns
// it from then on.
func TestUpsertTemplateByKeySeedsTheSubjectOnceThenLeavesItAlone(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()

	const key = "keycloak.personal-email-confirm"
	const repoSubject = "E-postanı doğrula"
	const operatorSubject = "E-posta adresini doğrula"

	seed := UpsertTemplateByKeyParams{
		Key:               key,
		Name:              "Kişisel e-posta doğrulama",
		Subject:           repoSubject,
		HtmlContent:       "<p>ilk gövde</p>",
		PlainTextContent:  "ilk gövde",
		ReactEmailContent: "// Kaynak: emails/keycloak.personal-email-confirm.tsx",
		System:            true,
	}

	created, err := db.UpsertTemplateByKey(ctx, seed)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if created.Subject != repoSubject {
		t.Fatalf("a new key takes the repo's subject: got %q, want %q", created.Subject, repoSubject)
	}

	// The operator rewords the subject in SkyMail, which is the whole point of
	// the template living here rather than in a Keycloak release.
	reworded, err := db.UpdateTemplate(ctx, UpdateTemplateParams{
		ID:                created.ID,
		Name:              created.Name,
		Subject:           operatorSubject,
		HtmlContent:       created.HtmlContent,
		PlainTextContent:  created.PlainTextContent,
		ReactEmailContent: created.ReactEmailContent,
		Key:               created.Key,
	})
	if err != nil {
		t.Fatalf("reword: %v", err)
	}
	if reworded.Subject != operatorSubject {
		t.Fatalf("the operator's subject did not stick: got %q", reworded.Subject)
	}

	// The seed runs again with a new body — a dark-theme fix, say — and the
	// repo's original subject still in its payload.
	seed.HtmlContent = "<p>ikinci gövde</p>"
	seed.PlainTextContent = "ikinci gövde"
	after, err := db.UpsertTemplateByKey(ctx, seed)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if after.Subject != operatorSubject {
		t.Errorf("the seed wrote over the operator's subject: got %q, want %q", after.Subject, operatorSubject)
	}
	// The structure is still the seed's to own, which is the other half of the
	// rule: a body fix in the repo has to reach production.
	if after.HtmlContent != "<p>ikinci gövde</p>" {
		t.Errorf("the seed should still own the body: got %q", after.HtmlContent)
	}
	if after.PlainTextContent != "ikinci gövde" {
		t.Errorf("the seed should still own the plain text: got %q", after.PlainTextContent)
	}
}
