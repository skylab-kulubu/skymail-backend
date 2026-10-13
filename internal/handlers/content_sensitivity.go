package handlers

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/ptr"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// The templates whose sends carry a credential — a login or verification
// code, a password reset or account link, a ticket's code — are locked
// sensitive: whoever holds skymail:system-mails:read would otherwise read
// everyone's codes and links. Their content is kept and shown masked (every
// code and one-time link replaced on the server), whatever their row says;
// the panel and the seed cannot make them shown unmasked, and a write that
// gives a template such a key marks it sensitive. This is the one list in Go;
// the database holds the same in template_key_content_locked() (migration
// 20261013140000), and a test keeps the two together.
//
// What sends to SkyMail today (origin/main of every repo, 2026-10-12):
//   - e-skylab-keycloak SPI: keycloak.verify-email, keycloak.reset-password,
//     keycloak.update-email, keycloak.idp-link, keycloak.personal-email-confirm
//     (a 6-digit code) and keycloak.generic (any other Keycloak mail, an
//     e-mail login code or link among them);
//   - account center (skymail-frontend emails/): account.security-alert,
//     account.primary-email-changed, account.deletion-requested (cancel
//     link), account.deletion-completed;
//   - core-backend: core.contact-consent-confirm (confirmation link), and the
//     event mails the repo holds for it: event.ticket-created and
//     event.reminder (ticket code, QR link).
//
// Shown, and so not here: core.welcome, core.certificate (its VerifyURL is
// public), club.team-membership, forms.* (form ids, no token), mail.approval-*
// (links need a sign-in), ops.send-failed, event.updated, event.cancelled,
// free.basic. YıldızPlace mails its login link over its own SMTP, not SkyMail;
// auth., login. and otp. are reserved for such flows when they move here.
var (
	lockedSensitiveKeyPrefixes = []string{"keycloak.", "account.", "auth.", "login.", "otp."}
	lockedSensitiveKeys        = []string{"core.contact-consent-confirm", "event.ticket-created", "event.reminder"}
	// A key that names one of these as a word (between dots and dashes), or as
	// two words written with a dash (sign-in, log-in, 2-fa), is taken for a
	// credential mail too, whoever sends it: place.login-link,
	// place.sign-in-link, forms.guest-verify-email, event.ticket-issued.
	lockedSensitiveKeyWords = []string{
		"login", "signin", "code", "otp", "passcode", "password", "reset", "verify", "verification",
		"confirm", "confirmation", "token", "magic", "invite", "activation", "activate", "2fa", "mfa",
		"ticket", "link",
		// Turkish, without its letters: giris, kod, dogrulama, sifre, parola,
		// baglanti, davet, onay.
		"giris", "kod", "kodu", "dogrulama", "sifre", "parola", "baglanti", "baglantisi", "davet", "onay",
	}
)

// ContentSensitiveLocked reports whether a template of this key is locked
// sensitive.
func ContentSensitiveLocked(key *string) bool {
	if key == nil {
		return false
	}
	for _, prefix := range lockedSensitiveKeyPrefixes {
		if strings.HasPrefix(*key, prefix) {
			return true
		}
	}
	for _, locked := range lockedSensitiveKeys {
		if *key == locked {
			return true
		}
	}
	words := strings.FieldsFunc(*key, func(r rune) bool { return r == '.' || r == '-' })
	for i, word := range words {
		if slices.Contains(lockedSensitiveKeyWords, word) {
			return true
		}
		if i+1 < len(words) && slices.Contains(lockedSensitiveKeyWords, word+words[i+1]) {
			return true
		}
	}
	return false
}

var errSensitiveTemplateKey = apperrors.New(
	"template.sensitive_key_immutable",
	"This template's mail carries a code or a one-time link: its key, which keeps its content hidden, cannot be changed or removed.",
	fiber.StatusConflict,
)

var errContentSensitiveLocked = apperrors.New(
	"template.content_sensitive_locked",
	"This template's mail carries a code or a one-time link, so its content stays hidden and cannot be shown.",
	fiber.StatusConflict,
)

// lockSensitive marks a template that has a locked key sensitive, recording
// the change, and returns the row as it is then. Other templates are returned
// as they are.
func lockSensitive(ctx context.Context, q *database.Queries, row database.Template, author database.VersionAuthor) (database.Template, error) {
	if !ContentSensitiveLocked(row.Key) || (row.ContentSensitive != nil && *row.ContentSensitive) {
		return row, nil
	}
	old := row.ContentSensitive
	updated, err := q.SetTemplateContentSensitive(ctx, database.SetTemplateContentSensitiveParams{ID: row.ID, ContentSensitive: true})
	if err != nil {
		return row, err
	}
	return updated, recordContentSensitiveChange(ctx, q, row.ID, author, old, updated.ContentSensitive)
}

// recordContentSensitiveChange keeps who changed a template's
// content_sensitive, and from what to what; a write that changed nothing
// records nothing.
func recordContentSensitiveChange(ctx context.Context, q *database.Queries, templateID uuid.UUID, author database.VersionAuthor, old, new *bool) error {
	if ptr.Equal(old, new) {
		return nil
	}
	return q.RecordTemplateContentSensitiveEvent(ctx, database.RecordTemplateContentSensitiveEventParams{
		TemplateID: templateID,
		ActorKind:  author.Kind,
		ActorSub:   author.Sub,
		ActorName:  author.Name,
		OldValue:   old,
		NewValue:   new,
	})
}

// SetContentSensitive godoc
//
//	@Summary		Set whether a template's sends show their content
//	@Description	Sets the template's content_sensitive, as the panel's switch does: true keeps the content of its sends masked (every code and one-time link replaced on the server once a send is done, for good), false lets those who may read a send's content see it as sent — for sends from then on: a send sensitive when it was queued stays so. A template whose mail carries a credential (keycloak.*, account.*, core.contact-consent-confirm, event.ticket-created, event.reminder) is locked sensitive and answers 409 template.content_sensitive_locked to false. Each change is recorded with who made it and the value before and after (GET /templates/{id}/content-sensitive/events); setting the value the template has records nothing. No version is written and updated_at stays. An archived template is 404.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string							true	"Template ID"
//	@Param			body	body		requests.SetContentSensitive	true	"The new value"
//	@Success		200		{object}	handlers.Template
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"Not Found"
//	@Failure		409		{object}	apperrors.AppError	"template.content_sensitive_locked: the template carries a credential and stays hidden; params.key"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/content-sensitive [put]
func (h *templateHandlerImpl) SetContentSensitive(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}
	var params requests.SetContentSensitive
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	value := *params.ContentSensitive

	var template database.Template
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		existing, err := q.LockTemplate(c.Context(), id)
		if err != nil {
			return err
		}
		if !value && ContentSensitiveLocked(existing.Key) {
			return errContentSensitiveLocked.WithParams(map[string]interface{}{"key": *existing.Key})
		}
		if existing.ContentSensitive != nil && *existing.ContentSensitive == value {
			template = existing
			return nil
		}
		template, err = q.SetTemplateContentSensitive(c.Context(), database.SetTemplateContentSensitiveParams{ID: id, ContentSensitive: value})
		if err != nil {
			return err
		}
		return recordContentSensitiveChange(c.Context(), q, id, versionAuthor(c, database.TemplateAuthorKindOperator), existing.ContentSensitive, template.ContentSensitive)
	})
	if err != nil {
		return err
	}
	return h.sendTemplate(c, fiber.StatusOK, template)
}

// ContentSensitiveEvent is one change of a template's content_sensitive.
type ContentSensitiveEvent struct {
	ID uuid.UUID `json:"id"`
	// Who: an operator in the panel, or the Template seed; sub and name as their token carried them.
	Actor database.VersionAuthor `json:"actor"`
	// The value before and after; null is "left to the kind of send".
	OldValue  *bool     `json:"old_value"`
	NewValue  *bool     `json:"new_value"`
	CreatedAt time.Time `json:"created_at"`
}

// ListContentSensitiveEvents godoc
//
//	@Summary		List the changes of a template's content_sensitive
//	@Description	Every change of whether the template's sends show their content, newest first: who (an operator or the Template seed), when, before and after. Archived templates included.
//	@Tags			Templates
//	@Produce		json
//	@Param			id		path		string	true	"Template ID"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		handlers.ContentSensitiveEvent
//	@Header			200		{integer}	X-Total-Count		"Number of changes"
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		404		{object}	apperrors.AppError	"Not Found"
//	@Router			/templates/{id}/content-sensitive/events [get]
func (h *templateHandlerImpl) ListContentSensitiveEvents(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}
	if _, err := h.db.GetTemplateByIdIncludingArchived(c.Context(), id); err != nil {
		return err
	}
	limit, offset := getPaginationParams(c)
	rows, err := h.db.ListTemplateContentSensitiveEvents(c.Context(), database.ListTemplateContentSensitiveEventsParams{
		TemplateID: id, RowLimit: limit, RowOffset: offset,
	})
	if err != nil {
		return err
	}
	count, err := h.db.CountTemplateContentSensitiveEvents(c.Context(), id)
	if err != nil {
		return err
	}
	events := make([]ContentSensitiveEvent, len(rows))
	for i, row := range rows {
		events[i] = ContentSensitiveEvent{
			ID:        row.ID,
			Actor:     database.VersionAuthor{Kind: row.ActorKind, Sub: row.ActorSub, Name: row.ActorName},
			OldValue:  row.OldValue,
			NewValue:  row.NewValue,
			CreatedAt: row.CreatedAt,
		}
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(events)
}
