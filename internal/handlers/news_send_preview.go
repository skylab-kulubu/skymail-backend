package handlers

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/news"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// NewsTestsPerMinute bounds a person's test mails (hub ticket duyuru-haber
// 07): at most this many in a minute.
const NewsTestsPerMinute = 3

// newsTestPrefix opens a test mail's subject.
const newsTestPrefix = "[Deneme] "

var (
	errNewsNoTestAddress = apperrors.New("news_send.no_test_address",
		"A test mail goes only to your own verified primary e-mail, and your token carries none.", fiber.StatusConflict)
	errNewsTestRate = apperrors.New("news_send.test_rate_limited",
		"At most 3 test mails a minute: wait a minute and send again.", fiber.StatusTooManyRequests)
)

// NewsSendAvailability answers GET /v1/news_sends/availability.
type NewsSendAvailability struct {
	// True while SKYMAIL_NEWS_SEND=on: a News record may be previewed, tested and sent.
	Available bool `json:"available"`
	// Where the mail's "Sitede oku" opens: home (the site's home page) or pages (the record's own page).
	ReadMore string `json:"read_more" enums:"home,pages"`
}

// Availability godoc
//
//	@Summary		Whether News records may be mailed
//	@Description	available while SKYMAIL_NEWS_SEND=on; the send form shows "Haberden gönder" only then. A setting of SkyMail, not of any send.
//	@Tags			News sends
//	@Produce		json
//	@Success		200	{object}	handlers.NewsSendAvailability
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Router			/news_sends/availability [get]
func (h *newsSendHandlerImpl) Availability(c fiber.Ctx) error {
	return c.JSON(NewsSendAvailability{Available: h.options.Enabled, ReadMore: h.options.ReadMore})
}

// newsMail is a News send's mail before any recipient: the record, its
// template and the send's variables.
type newsMail struct {
	record    news.Record
	template  database.Template
	variables map[string]any
}

func (h *newsSendHandlerImpl) mailOf(c fiber.Ctx, params requests.SendNews) (newsMail, error) {
	if !h.options.Enabled {
		return newsMail{}, errNewsSendOff
	}
	if (params.Audience.Type == news.AudienceTypeMembers) != (len(params.Audience.Teams) == 0) {
		return newsMail{}, errNewsTeams
	}
	record, template, err := h.load(c.Context(), params.Slug)
	if err != nil {
		return newsMail{}, err
	}
	return newsMail{record: record, template: template,
		variables: news.Variables(record, params.Format, params.Subject, params.Preheader, h.options.mail())}, nil
}

// sampleRecipient is the caller as a preview's and a test mail's reader: their
// name and verified address from the token, the sample unsubscribe link and
// core's kind of words for why they got it.
func sampleRecipient(c fiber.Ctx, params requests.SendNews, kind string) mailer.RecipientInfo {
	name, _ := c.Locals("user_name").(string)
	email, _ := c.Locals("user_email").(string)
	reason, label := news.SampleAudience(params.Audience.Type, params.Audience.Teams, kind)
	return mailer.RecipientInfo{FullName: strings.TrimSpace(name), Email: strings.TrimSpace(email), UnsubscribeURL: news.SampleUnsubscribeURL,
		Variables: map[string]string{"AudienceReason": reason, "CategoryLabel": label}}
}

// Preview godoc
//
//	@Summary		Render a News send's mail
//	@Description	The subject, the HTML and the plain text a send of these fields would queue, rendered on the server with the club.news template exactly as the send renders (desktop and mobile previews show the same HTML), for the caller as its reader: your name and address, the sample unsubscribe link (the preferences page, which changes nothing) and words for why you got it in core's shape. A real recipient's are core's. Nothing is queued; commercial_content_absent is not asked here.
//	@Tags			News sends
//	@Accept			json
//	@Produce		json
//	@Param			send	body		requests.SendNews	true	"The record and the mail, as POST /news_sends takes them"
//	@Success		200		{object}	mailer.Rendered
//	@Failure		400		{object}	apperrors.AppError	"validation.error; news_send.teams_invalid"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"news_send.not_found"
//	@Failure		409		{object}	apperrors.AppError	"news_send.private_record; news_send.template_missing"
//	@Failure		503		{object}	apperrors.AppError	"news_send.not_enabled; news_send.cms_unavailable"
//	@Router			/news_sends/preview [post]
func (h *newsSendHandlerImpl) Preview(c fiber.Ctx) error {
	var params requests.SendNews
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	mail, err := h.mailOf(c, params)
	if err != nil {
		return err
	}
	variables, err := json.Marshal(mail.variables)
	if err != nil {
		return err
	}
	rendered, err := mailer.Render(mail.template.Subject, mail.template.PlainTextContent, mail.template.HtmlContent, variables,
		sampleRecipient(c, params, mail.record.Kind))
	if err != nil {
		return err
	}
	return c.JSON(rendered)
}

// Test godoc
//
//	@Summary		Send a News send's mail to yourself
//	@Description	Queues the mail a send of these fields would make to your own verified primary e-mail (the token's) and to nobody else: the body has no address field. The subject opens with "[Deneme]"; the unsubscribe link is the sample one (the preferences page, which changes nothing). It goes as a duyuru from the duyuru mailbox, as the send would, but is no News send: it has no source, counts in no report of the record and is no mailbox's "already received". At most 3 a minute per person (429 news_send.test_rate_limited, Retry-After). commercial_content_absent is not asked here.
//	@Tags			News sends
//	@Accept			json
//	@Produce		json
//	@Param			send	body		requests.SendNews	true	"The record and the mail, as POST /news_sends takes them"
//	@Success		201		{object}	handlers.CreatedMailTask
//	@Failure		400		{object}	apperrors.AppError	"validation.error; news_send.teams_invalid"
//	@Failure		403		{object}	apperrors.AppError	"server.forbidden: no skymail:announcements:send or skymail:mails:write"
//	@Failure		404		{object}	apperrors.AppError	"news_send.not_found"
//	@Failure		409		{object}	apperrors.AppError	"news_send.no_test_address; news_send.private_record; news_send.template_missing"
//	@Failure		429		{object}	apperrors.AppError	"news_send.test_rate_limited"
//	@Failure		503		{object}	apperrors.AppError	"news_send.not_enabled; news_send.cms_unavailable"
//	@Router			/news_sends/test [post]
func (h *newsSendHandlerImpl) Test(c fiber.Ctx) error {
	var params requests.SendNews
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	sentBy, ok := c.Locals("user_id").(string)
	if !ok || sentBy == "" {
		return apperrors.ErrForbidden
	}
	mail, err := h.mailOf(c, params)
	if err != nil {
		return err
	}
	recipient := sampleRecipient(c, params, mail.record.Kind)
	if recipient.Email == "" {
		return errNewsNoTestAddress
	}
	// The sample link shows in the body only: a test mail carries no
	// List-Unsubscribe, so a mail client offers no "unsubscribe" that would
	// tell its sender they left.
	recipient.UnsubscribeInBodyOnly = true
	mail.variables["Subject"] = newsTestPrefix + mail.variables["Subject"].(string)
	variables, err := json.Marshal(mail.variables)
	if err != nil {
		return err
	}
	var purpose mailer.SendPurpose
	if h.mailer.PurposeAvailable() {
		duyuru, category, statement := mailer.PurposeDuyuru, "club:"+mail.record.Kind, true
		purpose = mailer.SendPurpose{Purpose: &duyuru, Category: &category, CommercialContentAbsent: &statement}
	}
	ctx := c.Context()
	var taskID uuid.UUID
	err = h.db.InTx(ctx, func(q *database.Queries) error {
		// One person's tests one at a time, so the count holds.
		if err := q.LockNewsSource(ctx, "test:"+sentBy); err != nil {
			return err
		}
		recent, err := q.CountRecentNewsTests(ctx, database.CountRecentNewsTestsParams{SentBy: sentBy, TemplateID: mail.template.ID})
		if err != nil {
			return err
		}
		if recent >= NewsTestsPerMinute {
			c.Set(fiber.HeaderRetryAfter, "60")
			return errNewsTestRate
		}
		taskID, err = h.mailer.Queue(q, mailer.PriorityBulk).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: sentBy, TemplateID: mail.template.ID, BodyVariables: variables,
			Recipients: []mailer.RecipientInfo{recipient}, Purpose: purpose,
			// To the caller's own verified address alone: the bulk preference
			// check leaves it be (abonelik-cikisi 09).
			SelfTest: true,
		})
		return err
	})
	if err != nil {
		return err
	}
	h.mailer.Wake()
	log.Info().Str("event", "news_send_test_queued").Str("task_id", taskID.String()).Str("source", "news:"+mail.record.Slug).Msg("news send test queued")
	return c.Status(fiber.StatusCreated).JSON(createdMailTask(taskID, false, purpose))
}

// NewsSendCount answers POST /v1/news_sends/count.
type NewsSendCount struct {
	// core's numbers for the audience (countOnly: no address is read). already_received is how many mailboxes this record's mail already went to, by any send of it: a send to this audience leaves out those among its people, so at most this many. no_unsubscribe_url is known only when sending (0 here).
	Counts NewsSendCounts `json:"counts"`
	// The send of this record to this audience, when there is one: a send now would be 409 news_send.already_sent.
	AlreadySentTaskID *uuid.UUID     `json:"already_sent_task_id"`
	Source            NewsSendSource `json:"source"`
}

// Count godoc
//
//	@Summary		Count who a News send would reach
//	@Description	The live count of the send form: core counts the audience as the send would resolve it (members, followers, and who it leaves out: turned off, disabled, unverified, without an address) with countOnly, so no address is read and the form may ask often; already_received is how many mailboxes this record's mail already reached by any send of it. The exact number left out is the send's (counts.already_received of POST /news_sends). Nothing is queued.
//	@Tags			News sends
//	@Accept			json
//	@Produce		json
//	@Param			count	body		requests.CountNewsSend	true	"The record and the audience"
//	@Success		200		{object}	handlers.NewsSendCount
//	@Failure		400		{object}	apperrors.AppError	"validation.error; news_send.teams_invalid; news_send.team_unknown"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"news_send.not_found"
//	@Failure		409		{object}	apperrors.AppError	"news_send.private_record; news_send.template_missing"
//	@Failure		503		{object}	apperrors.AppError	"news_send.not_enabled; news_send.cms_unavailable; news_send.audience_unavailable"
//	@Router			/news_sends/count [post]
func (h *newsSendHandlerImpl) Count(c fiber.Ctx) error {
	var params requests.CountNewsSend
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	if !h.options.Enabled {
		return errNewsSendOff
	}
	teams := params.Audience.Teams
	if (params.Audience.Type == news.AudienceTypeMembers) != (len(teams) == 0) {
		return errNewsTeams
	}
	ctx := c.Context()
	record, _, err := h.load(ctx, params.Slug)
	if err != nil {
		return err
	}
	sourceRef := "news:" + record.Slug
	answer := NewsSendCount{Source: NewsSendSource{Ref: sourceRef, Slug: record.Slug, Version: record.Version, Kind: record.Kind}}
	existing, err := h.db.GetNewsSendByAudience(ctx, database.GetNewsSendByAudienceParams{SourceRef: sourceRef, SourceAudience: news.AudienceKey(params.Audience.Type, teams)})
	switch {
	case err == nil:
		answer.AlreadySentTaskID = &existing
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	counts, skipped, err := h.audiences.Count(ctx, news.AudienceRequest{Type: params.Audience.Type, Teams: teams, NewsKind: record.Kind, Visibility: record.Audience})
	switch {
	case errors.Is(err, news.ErrTeamUnknown):
		return errNewsTeamUnknown
	case errors.Is(err, news.ErrVisibilityNotEnabled):
		return errNewsPrivate
	case err != nil:
		log.Error().Err(err).Msg("news send count: core did not answer the audience")
		return errNewsAudienceUnavailable
	}
	answer.Counts = countsOf(news.Audience{Counts: counts, Skipped: skipped})
	received, err := h.db.ListNewsSourceAddresses(ctx, sourceRef)
	if err != nil {
		return err
	}
	answer.Counts.AlreadyReceived = len(received)
	return c.JSON(answer)
}
