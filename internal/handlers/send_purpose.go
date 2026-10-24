package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// A send's purpose and category (ADR 0072 decision 2; Yusuf, 2026-10-21;
// hub ticket abonelik-cikisi 03; README, "Gönderim amacı ve kategori"). While
// SKYMAIL_MAIL_PURPOSE is on, every send a person queues or submits is
// written with a purpose, which picks its stream (mailer.LaneOf):
//
//   - a list or a Keycloak group is always duyuru;
//   - people one by one (POST /mail_tasks/single, a Mail onayı request to
//     people) and a Forms source's respondents may be islem, with the
//     sender's statement that the mail is about the recipient's own
//     application or transaction; it is tracked as any person's send is
//     (Yusuf: every mail is tracked; SKYMAIL_TRACKING_OPERATOR);
//   - a duyuru send may say it is promotional (commercial_content_absent
//     false: its main purpose is promoting a product or a service) only
//     while SKYMAIL_PROMOTIONAL is on; it then goes only to those who
//     consented to promotional mail (KVKK aydınlatma s6 §5.10; mailer,
//     consent_gate.go). No islem send is promotional;
//   - a duyuru send names core's category;
//   - a service's single mail is always islem, whatever it says;
//   - a person's send whose body names no purpose (an older client) is
//     duyuru, club:duyuru: a list, a group, a Forms source, a Mail onayı
//     request and a single mail, tracked or not (ADR 0072 decision 2: "Seçmezse
//     posta duyurudur").
//
// While it is off the fields are ignored and nothing is written: every send
// goes as before.

// sendTarget is whom a send goes to, which decides the purposes it may have.
type sendTarget int

const (
	// targetAudience: a list or a Keycloak group, always duyuru.
	targetAudience sendTarget = iota
	// targetPeople: people one by one.
	targetPeople
	// targetFormSource: a Forms source's respondents.
	targetFormSource
)

var (
	errPurposeRequired = apperrors.New("mail_task.purpose_required",
		"category, transactional_statement and commercial_content_absent come with a purpose.", fiber.StatusBadRequest)
	errPurposeInvalid = apperrors.New("mail_task.purpose_invalid",
		"purpose is islem or duyuru.", fiber.StatusBadRequest)
	errPurposeNotAllowed = apperrors.New("mail_task.purpose_not_allowed",
		"A send to a list or a Keycloak group is duyuru: islem is only for people one by one or a Forms source's respondents.", fiber.StatusBadRequest)
	errTransactionalStatementRequired = apperrors.New("mail_task.transactional_statement_required",
		"An islem send needs transactional_statement true: the mail is about the recipient's own application or transaction.", fiber.StatusBadRequest)
	errCommercialContent = apperrors.New("mail_task.commercial_content_not_allowed",
		"commercial_content_absent false makes a mail promotional (its main purpose is promoting a product or a service: a discount code, a company's job posting, a sale, a raffle), which goes only to those who consented to promotional mail: it is allowed on a duyuru send while SkyMail's promotional sends are on (tracking-mode promotional_available), and never on an islem send.", fiber.StatusBadRequest)
	errCategoryRequired = apperrors.New("mail_task.category_required",
		"A duyuru send needs a category: club:duyuru, club:haber, team:<group id>:duyuru or team:<group id>:haber.", fiber.StatusBadRequest)
	errCategoryInvalid = apperrors.New("mail_task.category_invalid",
		"category is club:duyuru, club:haber, team:<group id>:duyuru or team:<group id>:haber.", fiber.StatusBadRequest)
	errCategoryNotAllowed = apperrors.New("mail_task.category_not_allowed",
		"An islem send has no category.", fiber.StatusBadRequest)
)

// clubAnnouncements is the category a send that names none has always been
// in: club-wide duyuru.
const clubAnnouncements = "club:duyuru"

// duyuruByDefault is the purpose of a send whose body names none and that has
// always been an announcement.
func duyuruByDefault() mailer.SendPurpose {
	purpose, category := mailer.PurposeDuyuru, clubAnnouncements
	return mailer.SendPurpose{Purpose: &purpose, Category: &category}
}

// islemByDefault is the purpose of a service's mail: transactional, without
// a statement (none was asked for).
func islemByDefault() mailer.SendPurpose {
	purpose := mailer.PurposeIslem
	return mailer.SendPurpose{Purpose: &purpose}
}

// isDuyuru says p is a duyuru send.
func isDuyuru(p mailer.SendPurpose) bool {
	return p.Purpose != nil && *p.Purpose == mailer.PurposeDuyuru
}

// choosePurpose checks the purpose a body names for a send to target: the
// zero SendPurpose while purposes are off (on false) or when the body names
// none, which the caller then gives its default; otherwise the purpose the
// send is written with, or why it is refused. promotional is
// SKYMAIL_PROMOTIONAL: a duyuru send may then be promotional. listConsent is
// SKYMAIL_LIST_CONSENT: a duyuru send to a list or a group then keeps its
// own event statement. Tracking is the
// tracking rules' (SKYMAIL_TRACKING, SKYMAIL_TRACKING_OPERATOR), whatever the
// purpose.
func choosePurpose(on, promotional, listConsent bool, asked requests.SendPurpose, target sendTarget) (mailer.SendPurpose, error) {
	if !on {
		return mailer.SendPurpose{}, nil
	}
	if asked.Purpose == nil {
		if asked.Category != nil || asked.TransactionalStatement != nil || asked.CommercialContentAbsent != nil || asked.OwnEventStatement != nil {
			return mailer.SendPurpose{}, errPurposeRequired
		}
		return mailer.SendPurpose{}, nil
	}
	if asked.CommercialContentAbsent != nil && !*asked.CommercialContentAbsent &&
		(!promotional || *asked.Purpose != mailer.PurposeDuyuru) {
		return mailer.SendPurpose{}, errCommercialContent
	}
	switch *asked.Purpose {
	case mailer.PurposeIslem:
		switch {
		case target == targetAudience:
			return mailer.SendPurpose{}, errPurposeNotAllowed
		case asked.Category != nil:
			return mailer.SendPurpose{}, errCategoryNotAllowed
		case asked.TransactionalStatement == nil || !*asked.TransactionalStatement:
			return mailer.SendPurpose{}, errTransactionalStatementRequired
		}
		purpose, statement := mailer.PurposeIslem, true
		return mailer.SendPurpose{Purpose: &purpose, TransactionalStatement: &statement}, nil
	case mailer.PurposeDuyuru:
		if asked.Category == nil || strings.TrimSpace(*asked.Category) == "" {
			return mailer.SendPurpose{}, errCategoryRequired
		}
		category, ok := canonicalCategory(*asked.Category)
		if !ok {
			return mailer.SendPurpose{}, errCategoryInvalid.WithParams(map[string]interface{}{"category": *asked.Category})
		}
		purpose := mailer.PurposeDuyuru
		chosen := mailer.SendPurpose{Purpose: &purpose, Category: &category, CommercialContentAbsent: asked.CommercialContentAbsent}
		// The list's own event statement is a list's or a group's, and only
		// while list consents are on (SKYMAIL_LIST_CONSENT).
		if listConsent && target == targetAudience {
			chosen.OwnEventStatement = asked.OwnEventStatement
		}
		return chosen, nil
	default:
		return mailer.SendPurpose{}, errPurposeInvalid
	}
}

// canonicalCategory is a category in the form mail_tasks keeps it, or false
// when raw is not one of core's: club:<kind>, or team:<Keycloak group id>:<kind>
// with the group id a UUID (written lower-case), kind duyuru or haber. Whether
// the group is a team is core's to say.
func canonicalCategory(raw string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	kindOK := func(kind string) bool { return kind == "duyuru" || kind == "haber" }
	switch {
	case len(parts) == 2 && parts[0] == "club" && kindOK(parts[1]):
		return "club:" + parts[1], true
	case len(parts) == 3 && parts[0] == "team" && kindOK(parts[2]):
		group, err := uuid.Parse(parts[1])
		if err != nil || len(parts[1]) != 36 {
			return "", false
		}
		return "team:" + group.String() + ":" + parts[2], true
	}
	return "", false
}

// purposeOrDefault is chosen, or — purposes on, and the body named none —
// fallback, the purpose the send has always had.
func purposeOrDefault(on bool, chosen mailer.SendPurpose, fallback func() mailer.SendPurpose) mailer.SendPurpose {
	if !on || chosen.Purpose != nil {
		return chosen
	}
	return fallback()
}
