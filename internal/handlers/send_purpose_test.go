package handlers

import (
	"errors"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// The rules of a send's purpose (ADR 0072 decision 2; Yusuf, 2026-10-21):
// islem only to people or a Forms source, with the statement, without a
// category; duyuru with one of core's categories; no commercial content
// declared, whatever the purpose; a body that names none gets its default
// from the caller; off, nothing is read. Tracking is not the purpose's.
func TestChoosePurpose(t *testing.T) {
	islem := requests.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(true)}
	team := "team:6F1D2C3B-0A9E-4F8D-8C7B-6A5E4D3C2B1A:haber"
	for _, tc := range []struct {
		name   string
		on     bool
		asked  requests.SendPurpose
		target sendTarget
		want   mailer.SendPurpose
		err    error
	}{
		{"off: everything ignored", false, requests.SendPurpose{Purpose: strp("islem")}, targetAudience, mailer.SendPurpose{}, nil},
		{"none named: the caller's default", true, requests.SendPurpose{}, targetAudience, mailer.SendPurpose{}, nil},
		{"a category without a purpose", true, requests.SendPurpose{Category: strp("club:duyuru")}, targetPeople, mailer.SendPurpose{}, errPurposeRequired},
		{"a statement without a purpose", true, requests.SendPurpose{TransactionalStatement: boolp(true)}, targetPeople, mailer.SendPurpose{}, errPurposeRequired},
		{"an unknown purpose", true, requests.SendPurpose{Purpose: strp("ticari")}, targetPeople, mailer.SendPurpose{}, errPurposeInvalid},
		{"islem to a list or a group", true, islem, targetAudience, mailer.SendPurpose{}, errPurposeNotAllowed},
		{"islem without the statement", true, requests.SendPurpose{Purpose: strp("islem")}, targetPeople, mailer.SendPurpose{}, errTransactionalStatementRequired},
		{"islem with the statement false", true, requests.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(false)}, targetFormSource, mailer.SendPurpose{}, errTransactionalStatementRequired},
		{"commercial content declared, duyuru", true, requests.SendPurpose{Purpose: strp("duyuru"), Category: strp("club:duyuru"), CommercialContentAbsent: boolp(false)}, targetAudience, mailer.SendPurpose{}, errCommercialContent},
		{"commercial content declared, islem", true, requests.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(true), CommercialContentAbsent: boolp(false)}, targetPeople, mailer.SendPurpose{}, errCommercialContent},
		{"commercial content declared without a purpose", true, requests.SendPurpose{CommercialContentAbsent: boolp(false)}, targetPeople, mailer.SendPurpose{}, errPurposeRequired},
		{"islem with a category", true, requests.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(true), Category: strp("club:duyuru")}, targetPeople, mailer.SendPurpose{}, errCategoryNotAllowed},
		{"islem to people", true, islem, targetPeople, mailer.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(true)}, nil},
		{"islem to a Forms source, its commercial statement dropped", true, requests.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(true), CommercialContentAbsent: boolp(true)}, targetFormSource, mailer.SendPurpose{Purpose: strp("islem"), TransactionalStatement: boolp(true)}, nil},
		{"duyuru without a category", true, requests.SendPurpose{Purpose: strp("duyuru")}, targetAudience, mailer.SendPurpose{}, errCategoryRequired},
		{"duyuru with a blank category", true, requests.SendPurpose{Purpose: strp("duyuru"), Category: strp(" ")}, targetAudience, mailer.SendPurpose{}, errCategoryRequired},
		{"duyuru with a bad category", true, requests.SendPurpose{Purpose: strp("duyuru"), Category: strp("club:bulten")}, targetAudience, mailer.SendPurpose{}, errCategoryInvalid},
		{"duyuru, a team lower-cased, its transactional statement dropped", true, requests.SendPurpose{Purpose: strp("duyuru"), Category: strp(team), TransactionalStatement: boolp(true), CommercialContentAbsent: boolp(true)}, targetAudience,
			mailer.SendPurpose{Purpose: strp("duyuru"), Category: strp("team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:haber"), CommercialContentAbsent: boolp(true)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := choosePurpose(tc.on, tc.asked, tc.target)
			if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("error %v, want %v", err, tc.err)
			}
			if !samePurpose(got, tc.want) {
				t.Errorf("purpose %s, want %s", show(got), show(tc.want))
			}
		})
	}
}

// core's category forms; a team is a Keycloak group id, a UUID.
func TestCanonicalCategory(t *testing.T) {
	for raw, want := range map[string]string{
		"club:duyuru": "club:duyuru",
		"club:haber":  "club:haber",
		" team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:duyuru ": "team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:duyuru",
		"all":                  "",
		"club":                 "",
		"club:bulten":          "",
		"kulup:duyuru":         "",
		"team::duyuru":         "",
		"team:g-airlab:duyuru": "",
		"team:6f1d2c3b0a9e4f8d8c7b6a5e4d3c2b1a:duyuru":       "",
		"team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:duyuru:x": "",
	} {
		got, ok := canonicalCategory(raw)
		if (want == "") == ok || got != want {
			t.Errorf("%q: %q %v, want %q", raw, got, ok, want)
		}
	}
}

func samePurpose(a, b mailer.SendPurpose) bool {
	eqS := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqB := func(x, y *bool) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eqS(a.Purpose, b.Purpose) && eqS(a.Category, b.Category) &&
		eqB(a.TransactionalStatement, b.TransactionalStatement) && eqB(a.CommercialContentAbsent, b.CommercialContentAbsent)
}

func show(p mailer.SendPurpose) string {
	s := func(x *string) string {
		if x == nil {
			return "nil"
		}
		return *x
	}
	b := func(x *bool) string {
		if x == nil {
			return "nil"
		}
		if *x {
			return "true"
		}
		return "false"
	}
	return "{" + s(p.Purpose) + " " + s(p.Category) + " " + b(p.TransactionalStatement) + " " + b(p.CommercialContentAbsent) + "}"
}
