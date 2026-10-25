package handlers

import (
	"context"
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

// AnnouncementsSendRole lets a person mail a News record to an audience
// directly (Karar D4: ADMIN and YK; bilet 04). Others will submit it to Mail
// onayı (a later slice of bilet 06).
const AnnouncementsSendRole = "skymail:announcements:send"

// NewsSendEnv turns POST /v1/news_sends on: off (the default, also when
// unset) or on. On needs SKYMAIL_NEWS_CMS_URL and CORE_URL.
const NewsSendEnv = "SKYMAIL_NEWS_SEND"

// NewsReadMoreEnv says where the mail's "Devamını oku" opens: home (the
// default: the site's home page, until the site has its News pages, bilet
// 11) or pages (/haberler/<slug>, /duyurular/<slug>).
const NewsReadMoreEnv = "SKYMAIL_NEWS_READ_MORE"

// maxNewsSlug is the longest slug a send keeps (mail_tasks.source_ref is
// news:<slug> of at most 200; requests.SendNews asks the same).
const maxNewsSlug = 200

// Why a person core answered is not queued, beside core's own skips.
const skipNoUnsubscribeURL = "no_unsubscribe_url"

var (
	errNewsSendOff = apperrors.New("news_send.not_enabled",
		"Sending a News record is off on this SkyMail (SKYMAIL_NEWS_SEND).", fiber.StatusServiceUnavailable)
	errNewsCommercialStatement = apperrors.New("news_send.commercial_statement_required",
		"A News send needs commercial_content_absent true: the mail's main purpose is not promotion (a short thanks to a sponsor or its logo is allowed).", fiber.StatusBadRequest)
	errNewsTeams = apperrors.New("news_send.teams_invalid",
		"audience teams names 1 to 32 teams; audience members names none.", fiber.StatusBadRequest)
	errNewsNotFound = apperrors.New("news_send.not_found",
		"There is no published News record of this slug: it is unknown or archived.", fiber.StatusNotFound)
	errNewsCMSUnavailable = apperrors.New("news_send.cms_unavailable",
		"The CMS did not answer the record. Nothing was queued; retry.", fiber.StatusServiceUnavailable)
	errNewsPrivate = apperrors.New("news_send.private_record",
		"The record is not public. Only public records are mailed yet (ADR 0067, Faz 1).", fiber.StatusConflict)
	errNewsTemplateMissing = apperrors.New("news_send.template_missing",
		"The club.news template is not in use: the Template seed has not written it.", fiber.StatusConflict)
	errNewsAlreadySent = apperrors.New("news_send.already_sent",
		"This record was already sent to this audience: params.task_id is that send.", fiber.StatusConflict)
	errNewsTeamUnknown = apperrors.New("news_send.team_unknown",
		"A named team is no team (core: an area, a leaders' or Privileged group, or an unknown one).", fiber.StatusBadRequest)
	errNewsAudienceUnavailable = apperrors.New("news_send.audience_unavailable",
		"core did not answer who the record goes to. Nothing was queued; retry.", fiber.StatusServiceUnavailable)
	errNewsSlugTooLong = apperrors.New("news_send.slug_too_long",
		"The record's slug is longer than 200 characters: SkyMail keeps a News send's source as news:<slug> of at most 200.", fiber.StatusBadRequest)
	errNewsNoRecipients = apperrors.New("news_send.no_recipients",
		"Nobody is left to send the record to: params says who was left out and why.", fiber.StatusUnprocessableEntity)
)

// NewsSources reads a published News record (news.CMS).
type NewsSources interface {
	Get(ctx context.Context, slug string) (news.Record, error)
}

// NewsAudiences asks core who a record goes to (news.Core).
type NewsAudiences interface {
	Resolve(ctx context.Context, asked news.AudienceRequest) (news.Audience, error)
	// Count is core's numbers only (countOnly): no address is read.
	Count(ctx context.Context, asked news.AudienceRequest) (news.Counts, news.Skipped, error)
}

// NewsSendOptions configure NewsSendHandler.
type NewsSendOptions struct {
	// Enabled is SKYMAIL_NEWS_SEND=on.
	Enabled bool
	// ReadMore is SKYMAIL_NEWS_READ_MORE: news.ReadMoreHome or ReadMorePages.
	ReadMore string
	// ImageHosts are SKYMAIL_NEWS_IMAGE_HOSTS: the hosts a record's cover
	// may come from; a cover from anywhere else is left out.
	ImageHosts []string
}

func (o NewsSendOptions) mail() news.MailOptions {
	return news.MailOptions{ReadMore: o.ReadMore, ImageHosts: o.ImageHosts}
}

type NewsSendHandler interface {
	Send(c fiber.Ctx) error
	Availability(c fiber.Ctx) error
	Preview(c fiber.Ctx) error
	Test(c fiber.Ctx) error
	Count(c fiber.Ctx) error
}

type newsSendHandlerImpl struct {
	db        *database.Store
	mailer    mailer.Transactional
	sources   NewsSources
	audiences NewsAudiences
	options   NewsSendOptions
}

func NewNewsSendHandler(db *database.Store, m mailer.Transactional, sources NewsSources, audiences NewsAudiences, options NewsSendOptions) NewsSendHandler {
	return &newsSendHandlerImpl{db: db, mailer: m, sources: sources, audiences: audiences, options: options}
}

// NewsSendSkipped are who of the audience got no row, by why: core's skips
// (core docs/news-audience.md, "Filters"), and no_unsubscribe_url, a person
// core answered without a link (never mailed without one).
type NewsSendSkipped struct {
	NotMember        int `json:"not_member"`
	NotVisible       int `json:"not_visible"`
	Disabled         int `json:"disabled"`
	NoEmail          int `json:"no_email"`
	Unverified       int `json:"unverified"`
	OptedOut         int `json:"opted_out"`
	DuplicateEmail   int `json:"duplicate_email"`
	NoUnsubscribeURL int `json:"no_unsubscribe_url"`
}

// NewsSendCounts are a News send's numbers, as mail_tasks.source_counts keeps
// them.
type NewsSendCounts struct {
	// core's: the audience's members and followers, total their sum.
	Members   int `json:"members"`
	Followers int `json:"followers"`
	Total     int `json:"total"`
	// Recipients left out because a row of this record already went to their mailbox (from an earlier send of it to another audience).
	AlreadyReceived int             `json:"already_received"`
	Skipped         NewsSendSkipped `json:"skipped"`
}

// NewsSendSource is the record a send was made of.
type NewsSendSource struct {
	// news:<slug>, the record's canonical slug.
	Ref     string `json:"ref" example:"news:bahar-donemi-kayitlari"`
	Slug    string `json:"slug"`
	Version int    `json:"version"`
	Kind    string `json:"kind" enums:"haber,duyuru"`
}

// NewsSendResult answers POST /v1/news_sends.
type NewsSendResult struct {
	CreatedMailTask
	Source NewsSendSource `json:"source"`
	// Rows queued to go, and rows written failed at queueing (a suppressed or unsubscribed address, a mail that did not render).
	Queued int            `json:"queued"`
	Failed int            `json:"failed"`
	Counts NewsSendCounts `json:"counts"`
}

// Send godoc
//
//	@Summary		Mail a published News record to an audience
//	@Description	Reads the record of slug from the CMS (inscribed, anonymously: only public records in Faz 1), asks core who it goes to (every Member, or some teams with their sub-teams and followers; core leaves out who turned the category off, a disabled, unverified or address-less account, and answers each person's own unsubscribe link and why they get it), and queues the club.news template to each in the bulk lane, as a duyuru of core's category with List-Unsubscribe from that link. The send keeps the record as it was (source.version; a later edit changes the web, never the mail). A record's mail goes to a mailbox once: an address a row of this record already went to (an earlier send to another audience) is left out and counted already_received; the same record to the same audience again is 409 news_send.already_sent with that send's id. Only a person with skymail:announcements:send sends directly; Mail onayı for the others comes with a later version. Off unless SKYMAIL_NEWS_SEND=on (503 news_send.not_enabled).
//	@Tags			News sends
//	@Accept			json
//	@Produce		json
//	@Param			send	body		requests.SendNews	true	"The record, the audience and the mail"
//	@Success		201		{object}	handlers.NewsSendResult
//	@Failure		400		{object}	apperrors.AppError	"validation.error; news_send.commercial_statement_required; news_send.teams_invalid; news_send.team_unknown; news_send.slug_too_long"
//	@Failure		403		{object}	apperrors.AppError	"server.forbidden: no skymail:announcements:send"
//	@Failure		404		{object}	apperrors.AppError	"news_send.not_found: unknown or archived"
//	@Failure		409		{object}	apperrors.AppError	"news_send.already_sent (params.task_id); news_send.private_record; news_send.template_missing"
//	@Failure		422		{object}	apperrors.AppError	"news_send.no_recipients (params: the counts)"
//	@Failure		503		{object}	apperrors.AppError	"news_send.not_enabled; news_send.cms_unavailable; news_send.audience_unavailable: nothing was queued"
//	@Router			/news_sends [post]
func (h *newsSendHandlerImpl) Send(c fiber.Ctx) error {
	if !h.options.Enabled {
		return errNewsSendOff
	}
	var params requests.SendNews
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	if params.CommercialContentAbsent == nil || !*params.CommercialContentAbsent {
		return errNewsCommercialStatement
	}
	teams := params.Audience.Teams
	if (params.Audience.Type == news.AudienceTypeMembers) != (len(teams) == 0) {
		return errNewsTeams
	}
	sentBy, ok := c.Locals("user_id").(string)
	if !ok || sentBy == "" {
		return apperrors.ErrForbidden
	}
	ctx := c.Context()

	record, template, err := h.load(ctx, params.Slug)
	if err != nil {
		return err
	}
	sourceRef := "news:" + record.Slug
	audienceKey := news.AudienceKey(params.Audience.Type, teams)
	if existing, err := h.db.GetNewsSendByAudience(ctx, database.GetNewsSendByAudienceParams{SourceRef: sourceRef, SourceAudience: audienceKey}); err == nil {
		return errNewsAlreadySent.WithParams(map[string]interface{}{"task_id": existing})
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	// core is asked before the transaction: SkyMail never waits for it
	// holding one.
	audience, err := h.audiences.Resolve(ctx, news.AudienceRequest{
		Type: params.Audience.Type, Teams: teams, NewsKind: record.Kind, Visibility: record.Audience,
	})
	switch {
	case errors.Is(err, news.ErrTeamUnknown):
		return errNewsTeamUnknown
	case errors.Is(err, news.ErrVisibilityNotEnabled):
		return errNewsPrivate
	case err != nil:
		log.Error().Err(err).Msg("news send: core did not answer the audience")
		return errNewsAudienceUnavailable
	}
	counts := countsOf(audience)
	recipients, category := newsRecipients(audience.People, record.Kind, &counts)

	variables, err := json.Marshal(news.Variables(record, params.Format, params.Subject, params.Preheader, h.options.mail()))
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(record)
	if err != nil {
		return err
	}
	var purpose mailer.SendPurpose
	if h.mailer.PurposeAvailable() {
		duyuru, statement := mailer.PurposeDuyuru, true
		purpose = mailer.SendPurpose{Purpose: &duyuru, Category: &category, CommercialContentAbsent: &statement}
	}
	tracking := operatorTracking(ctx, h.mailer, params.Tracking, template.ID, nil)
	var preferences *mailer.TrackingPreferences
	if tracking && len(recipients) > 0 {
		preferences = h.mailer.ResolveTracking(ctx, template.ID, nil, recipients)
	}

	result := NewsSendResult{Source: NewsSendSource{Ref: sourceRef, Slug: record.Slug, Version: record.Version, Kind: record.Kind}}
	err = h.db.InTx(ctx, func(q *database.Queries) error {
		if err := q.LockNewsSource(ctx, sourceRef); err != nil {
			return err
		}
		existing, err := q.GetNewsSendByAudience(ctx, database.GetNewsSendByAudienceParams{SourceRef: sourceRef, SourceAudience: audienceKey})
		if err == nil {
			return errNewsAlreadySent.WithParams(map[string]interface{}{"task_id": existing})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		received, err := q.ListNewsSourceAddresses(ctx, sourceRef)
		if err != nil {
			return err
		}
		pending := withoutReceived(recipients, received, &counts)
		if len(pending) == 0 {
			return errNewsNoRecipients.WithParams(map[string]interface{}{"counts": counts})
		}
		taskID, err := h.mailer.Queue(q, mailer.PriorityBulk).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: sentBy, TemplateID: template.ID, BodyVariables: variables, Recipients: pending,
			Tracking: tracking, Preferences: preferences, Purpose: purpose, SourceRef: &sourceRef,
		})
		if err != nil {
			return err
		}
		stored, err := json.Marshal(counts)
		if err != nil {
			return err
		}
		if err := q.SetMailTaskNewsSource(ctx, database.SetMailTaskNewsSourceParams{
			ID: taskID, SourceRef: sourceRef, SourceVersion: record.Version, SourceSnapshot: snapshot,
			SourceAudience: audienceKey, SourceCounts: stored,
		}); err != nil {
			return err
		}
		rows, err := q.CountMailTaskRows(ctx, taskID)
		if err != nil {
			return err
		}
		result.CreatedMailTask = createdMailTask(taskID, tracking, purpose)
		result.Queued, result.Failed = int(rows.Queued), int(rows.Failed)
		return nil
	})
	if err != nil {
		return err
	}
	result.Counts = counts
	if result.Queued > 0 {
		h.mailer.Wake()
	}
	logNewsSend(result, params.Audience.Type)
	return c.Status(fiber.StatusCreated).JSON(result)
}

// load reads the published record of slug and the club.news template: what
// every News route starts from.
func (h *newsSendHandlerImpl) load(ctx context.Context, slug string) (news.Record, database.Template, error) {
	record, err := h.sources.Get(ctx, strings.TrimSpace(slug))
	if errors.Is(err, news.ErrNotFound) {
		return news.Record{}, database.Template{}, errNewsNotFound
	}
	if err != nil {
		log.Error().Err(err).Msg("news send: the CMS did not answer the record")
		return news.Record{}, database.Template{}, errNewsCMSUnavailable
	}
	if len(record.Slug) > maxNewsSlug {
		return news.Record{}, database.Template{}, errNewsSlugTooLong
	}
	if record.Audience != news.AudiencePublic {
		return news.Record{}, database.Template{}, errNewsPrivate
	}
	key := news.TemplateKey
	template, err := h.db.GetTemplateByKey(ctx, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return news.Record{}, database.Template{}, errNewsTemplateMissing
	}
	if err != nil {
		return news.Record{}, database.Template{}, err
	}
	return record, template, nil
}

// countsOf are core's numbers for an audience, as a send keeps them.
func countsOf(audience news.Audience) NewsSendCounts {
	return NewsSendCounts{Members: audience.Counts.Members, Followers: audience.Counts.Followers, Total: audience.Counts.Total,
		Skipped: NewsSendSkipped{NotMember: audience.Skipped.NotMember, NotVisible: audience.Skipped.NotVisible,
			Disabled: audience.Skipped.Disabled, NoEmail: audience.Skipped.NoEmail, Unverified: audience.Skipped.Unverified,
			OptedOut: audience.Skipped.OptedOut, DuplicateEmail: audience.Skipped.DuplicateEmail}}
}

// newsRecipients are core's people as the queue takes them, each mailbox
// once, each with their own unsubscribe link and core's words for why they
// get the mail; and the send's category: core's for every Member
// (club:<kind>), or the first team category core answered. A person without
// a valid link is never queued (counts.skipped.no_unsubscribe_url).
func newsRecipients(people []news.Person, kind string, counts *NewsSendCounts) ([]mailer.RecipientInfo, string) {
	category := "club:" + kind
	teamCategory := ""
	recipients := make([]mailer.RecipientInfo, 0, len(people))
	seen := make(map[string]bool, len(people))
	for _, p := range people {
		address := strings.ToLower(strings.TrimSpace(p.Email))
		switch {
		case address == "":
			counts.Skipped.NoEmail++
			continue
		case seen[address]:
			// core leaves a shared address to the lower user id within one
			// snapshot; pages read across two snapshots may not.
			counts.Skipped.DuplicateEmail++
			continue
		}
		if !mailer.ValidUnsubscribeURL(p.UnsubscribeURL) {
			counts.Skipped.NoUnsubscribeURL++
			continue
		}
		seen[address] = true
		info := mailer.RecipientInfo{FullName: strings.TrimSpace(p.Name), Email: strings.TrimSpace(p.Email), UnsubscribeURL: p.UnsubscribeURL,
			Variables: map[string]string{"AudienceReason": p.Reason, "CategoryLabel": p.CategoryLabel}}
		if sub, err := uuid.Parse(p.UserID); err == nil {
			info.Sub = &sub
		}
		recipients = append(recipients, info)
		for _, c := range p.Categories {
			if canonical, ok := canonicalCategory(c); ok && teamCategory == "" && strings.HasPrefix(canonical, "team:") {
				teamCategory = canonical
			}
		}
	}
	if teamCategory != "" {
		category = teamCategory
	}
	return recipients, category
}

// withoutReceived leaves out the recipients whose mailbox a row of the
// record already went to, counting them.
func withoutReceived(recipients []mailer.RecipientInfo, received []string, counts *NewsSendCounts) []mailer.RecipientInfo {
	if len(received) == 0 {
		return recipients
	}
	got := make(map[string]bool, len(received))
	for _, address := range received {
		got[address] = true
	}
	pending := make([]mailer.RecipientInfo, 0, len(recipients))
	for _, r := range recipients {
		if got[strings.ToLower(r.Email)] {
			counts.AlreadyReceived++
			continue
		}
		pending = append(pending, r)
	}
	return pending
}

// logNewsSend writes the send's numbers: no person, address, link or text
// of the record.
func logNewsSend(result NewsSendResult, audienceType string) {
	log.Info().
		Str("event", "news_send_queued").
		Str("task_id", result.ID.String()).
		Str("source", result.Source.Ref).
		Int("version", result.Source.Version).
		Str("audience", audienceType).
		Int("queued", result.Queued).
		Int("failed", result.Failed).
		Int("already_received", result.Counts.AlreadyReceived).
		Int("opted_out", result.Counts.Skipped.OptedOut).
		Int("no_unsubscribe_url", result.Counts.Skipped.NoUnsubscribeURL).
		Msg("news send queued")
}
