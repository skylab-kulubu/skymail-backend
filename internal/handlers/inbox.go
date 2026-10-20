package handlers

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	textt "text/template"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// The in-app inbox (hub ticket iletisim-servisi 05; ADR 0070): a notification
// type whose default channels hold inbox writes one item per recipient, from
// the type's inbox variant, in the notification's own transaction. A person
// reads and marks only their own items, through /v1/me/inbox with their own
// token (middlewares.AuthenticatePerson).

// ChannelInbox is the in-app inbox channel.
const ChannelInbox = "inbox"

// InboxRetention is how long an inbox item is kept at most (ADR 0070: 90
// days); the notification's expires_at can make it shorter.
const InboxRetention = 90 * 24 * time.Hour

// Inbox pages: 50 items unless the caller asks for up to 100.
const (
	inboxPageDefault = 50
	inboxPageMax     = 100
)

// The longest a rendered inbox text may be, in characters.
const (
	inboxTitleMax   = 200
	inboxSummaryMax = 1000
	inboxLinkMax    = 2048
)

var (
	errInboxVariantRequired = apperrors.New(
		"notification_type.inbox_variant_required",
		"A type whose default channels hold inbox needs its inbox text: inbox {title, summary, link}.",
		fiber.StatusBadRequest,
	)
	errInboxVariantUnused = apperrors.New(
		"notification_type.inbox_variant_unused",
		"inbox is the type's inbox text; its default channels do not hold inbox.",
		fiber.StatusBadRequest,
	)
	errInboxVariantInvalid = apperrors.New(
		"notification_type.inbox_variant_invalid",
		"The inbox text does not parse as a Go text/template, or its link does not start with a single / (an in-app path; no scheme or host).",
		fiber.StatusBadRequest,
	)
	errExpiresAtPast = apperrors.New(
		"notification.expires_at_past",
		"expires_at must be in the future.",
		fiber.StatusBadRequest,
	)
	errInboxCursor = apperrors.New(
		"inbox.invalid_cursor",
		"cursor is not one this inbox handed out (next_cursor).",
		fiber.StatusBadRequest,
	)
	errInboxQuery = apperrors.New(
		"inbox.invalid_query",
		"limit is 1 to 100; archived is exclude, include or only.",
		fiber.StatusBadRequest,
	)
)

// InboxVariant is a type's inbox text as the API serves it.
type InboxVariant struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Link    string `json:"link"`
}

// inboxTemplates is a type's inbox variant, parsed.
type inboxTemplates struct {
	title, summary, link *textt.Template
}

// parseInboxVariant parses the three texts as the notification will render
// them: plain text/template, a variable the notification does not carry an
// error rather than "<no value>". part names the text that did not parse.
func parseInboxVariant(title, summary, link string) (inboxTemplates, string, error) {
	var t inboxTemplates
	for _, part := range []struct {
		name string
		text string
		into **textt.Template
	}{{"title", title, &t.title}, {"summary", summary, &t.summary}, {"link", link, &t.link}} {
		parsed, err := textt.New(part.name).Option("missingkey=error").Parse(part.text)
		if err != nil {
			return inboxTemplates{}, part.name, err
		}
		*part.into = parsed
	}
	return t, "", nil
}

// inAppLinkPrefix reports whether a link stays in the app: empty, or a path
// that starts with exactly one slash (/events/..., never //host or a scheme).
func inAppLinkPrefix(link string) bool {
	return link == "" || (strings.HasPrefix(link, "/") && !strings.HasPrefix(link, "//") && !strings.HasPrefix(link, `/\`))
}

// validInAppLink is inAppLinkPrefix for a rendered link, which also holds no
// space, control character or backslash.
func validInAppLink(link string) bool {
	if !inAppLinkPrefix(link) || len(link) > inboxLinkMax {
		return false
	}
	for _, r := range link {
		if r <= ' ' || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

// checkInboxVariant is PUT /notification_types/{key}'s rule for the inbox
// text: there exactly when the channels hold inbox, parsing, its link an
// in-app path.
func checkInboxVariant(channels []string, variant *requests.InboxVariant) error {
	hasInbox := false
	for _, channel := range channels {
		hasInbox = hasInbox || channel == ChannelInbox
	}
	switch {
	case hasInbox && variant == nil:
		return errInboxVariantRequired
	case !hasInbox && variant != nil:
		return errInboxVariantUnused
	case variant == nil:
		return nil
	}
	if _, part, err := parseInboxVariant(variant.Title, variant.Summary, variant.Link); err != nil {
		return errInboxVariantInvalid.WithParams(map[string]interface{}{"part": part})
	}
	if !inAppLinkPrefix(variant.Link) {
		return errInboxVariantInvalid.WithParams(map[string]interface{}{"part": "link"})
	}
	return nil
}

// renderedInbox is one notification's inbox text.
type renderedInbox struct {
	title, summary string
	link           *string
}

// render renders the inbox text with the notification's variables. failure
// names the part that did not render or was out of bounds; it is the class
// only, never a value.
func (t inboxTemplates) render(vars map[string]interface{}) (renderedInbox, string) {
	if vars == nil {
		vars = map[string]interface{}{}
	}
	out := make(map[string]string, 3)
	for name, tmpl := range map[string]*textt.Template{"title": t.title, "summary": t.summary, "link": t.link} {
		var b bytes.Buffer
		if err := tmpl.Execute(&b, vars); err != nil {
			return renderedInbox{}, name
		}
		out[name] = strings.TrimSpace(b.String())
	}
	switch {
	case out["title"] == "" || utf8.RuneCountInString(out["title"]) > inboxTitleMax || !utf8.ValidString(out["title"]):
		return renderedInbox{}, "title"
	case utf8.RuneCountInString(out["summary"]) > inboxSummaryMax || !utf8.ValidString(out["summary"]):
		return renderedInbox{}, "summary"
	case !validInAppLink(out["link"]):
		return renderedInbox{}, "link"
	}
	r := renderedInbox{title: out["title"], summary: out["summary"]}
	if out["link"] != "" {
		link := out["link"]
		r.link = &link
	}
	return r, ""
}

// InboxItem is one item of a person's inbox.
type InboxItem struct {
	ID uuid.UUID `json:"id"`
	// The notification type's key.
	Type    string `json:"type" example:"event.cancelled"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// The in-app path the item opens; null when none.
	Link       *string    `json:"link" example:"/events/6f1c2d3e"`
	CreatedAt  time.Time  `json:"created_at"`
	SeenAt     *time.Time `json:"seen_at"`
	ReadAt     *time.Time `json:"read_at"`
	ArchivedAt *time.Time `json:"archived_at"`
}

func inboxItemOf(row database.InboxItem) InboxItem {
	return InboxItem{ID: row.ID, Type: row.NotificationType, Title: row.Title, Summary: row.Summary, Link: row.Link,
		CreatedAt: row.CreatedAt, SeenAt: row.SeenAt, ReadAt: row.ReadAt, ArchivedAt: row.ArchivedAt}
}

// InboxPage is a page of a person's inbox, newest first.
type InboxPage struct {
	Items []InboxItem `json:"items"`
	// The next page's cursor; null on the last page.
	NextCursor *string `json:"next_cursor"`
}

// InboxCounts is what a badge shows.
type InboxCounts struct {
	// Items not yet seen (archived ones left out).
	Unseen int64 `json:"unseen"`
	// Items not yet read (archived ones left out).
	Unread int64 `json:"unread"`
}

// InboxUpdated is how many items a bulk mark changed.
type InboxUpdated struct {
	Updated int64 `json:"updated"`
}

type InboxHandler interface {
	List(c fiber.Ctx) error
	Counts(c fiber.Ctx) error
	Seen(c fiber.Ctx) error
	Read(c fiber.Ctx) error
	Unread(c fiber.Ctx) error
	Archive(c fiber.Ctx) error
	ReadAll(c fiber.Ctx) error
	SeenAll(c fiber.Ctx) error
}

type inboxHandlerImpl struct {
	db *database.Store
}

func NewInboxHandler(db *database.Store) InboxHandler {
	return &inboxHandlerImpl{db: db}
}

// owner is the person the token is: AuthenticatePerson put their sub there.
func owner(c fiber.Ctx) (uuid.UUID, error) {
	sub, _ := c.Locals("user_id").(string)
	id, err := uuid.Parse(sub)
	if err != nil {
		// A Keycloak user's sub is a UUID; anything else has no inbox.
		return uuid.Nil, apperrors.ErrForbidden
	}
	return id, nil
}

// An inbox cursor is the last item's (created_at, id), opaque to the caller.
func encodeInboxCursor(item database.InboxItem) string {
	raw := strconv.FormatInt(item.CreatedAt.UnixMicro(), 10) + "_" + item.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeInboxCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	micros, id, ok := strings.Cut(string(raw), "_")
	if !ok {
		return time.Time{}, uuid.Nil, errors.New("no separator")
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return time.UnixMicro(n).UTC(), parsed, nil
}

// List godoc
//
//	@Summary		List my inbox
//	@Description	The caller's own inbox items, newest first, a page at a time: next_cursor goes into cursor for the next page, null on the last. Archived items are left out unless archived says include or only; an item past its expiry (90 days, or sooner) is never served. Only a person's own token issued for SkyMail (aud skymail) may call it; no role is needed.
//	@Tags			Inbox
//	@Produce		json
//	@Param			cursor		query		string	false	"The next_cursor of the page before"
//	@Param			limit		query		int		false	"Items per page, 1 to 100"	default(50)
//	@Param			archived	query		string	false	"exclude, include or only"	default(exclude)
//	@Success		200			{object}	handlers.InboxPage
//	@Failure		400			{object}	apperrors.AppError	"inbox.invalid_query; inbox.invalid_cursor"
//	@Failure		401			{object}	apperrors.AppError	"server.unauthorized: no live token, or one not issued for SkyMail"
//	@Failure		403			{object}	apperrors.AppError	"auth.person_required: a service's token"
//	@Router			/me/inbox [get]
func (h *inboxHandlerImpl) List(c fiber.Ctx) error {
	me, err := owner(c)
	if err != nil {
		return err
	}
	limit := inboxPageDefault
	if raw := c.Query("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > inboxPageMax {
			return errInboxQuery
		}
	}
	archived := c.Query("archived", "exclude")
	if archived != "exclude" && archived != "include" && archived != "only" {
		return errInboxQuery
	}
	params := database.ListInboxItemsParams{UserID: me, Archived: archived, PageSize: limit + 1}
	if cursor := c.Query("cursor"); cursor != "" {
		at, id, err := decodeInboxCursor(cursor)
		if err != nil {
			return errInboxCursor
		}
		params.BeforeCreatedAt, params.BeforeID = &at, &id
	}
	rows, err := h.db.ListInboxItems(c.Context(), params)
	if err != nil {
		return err
	}
	page := InboxPage{Items: make([]InboxItem, 0, min(len(rows), limit))}
	for i, row := range rows {
		if i == limit {
			next := encodeInboxCursor(rows[limit-1])
			page.NextCursor = &next
			break
		}
		page.Items = append(page.Items, inboxItemOf(row))
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(page)
}

// Counts godoc
//
//	@Summary		Count my unseen and unread items
//	@Description	What the caller's badge shows: their items not yet seen and not yet read, archived and expired ones left out.
//	@Tags			Inbox
//	@Produce		json
//	@Success		200	{object}	handlers.InboxCounts
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Router			/me/inbox/counts [get]
func (h *inboxHandlerImpl) Counts(c fiber.Ctx) error {
	me, err := owner(c)
	if err != nil {
		return err
	}
	counts, err := h.db.CountInboxItems(c.Context(), me)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(InboxCounts{Unseen: counts.Unseen, Unread: counts.Unread})
}

// mark marks one of the caller's items. Another person's item, one that does
// not exist and one past its expiry are all 404: whether an item exists is
// not told to anyone but its owner.
func (h *inboxHandlerImpl) mark(c fiber.Ctx, mark string) error {
	me, err := owner(c)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	row, err := h.db.MarkInboxItem(c.Context(), database.MarkInboxItemParams{Mark: mark, ID: id, UserID: me})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperrors.ErrStatusNotFound
	}
	if err != nil {
		return err
	}
	return c.JSON(inboxItemOf(row))
}

// Seen godoc
//
//	@Summary		Mark one of my items seen
//	@Description	The item was shown to the caller. A mark already there keeps its time.
//	@Tags			Inbox
//	@Produce		json
//	@Param			id	path		string	true	"The item"
//	@Success		200	{object}	handlers.InboxItem
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Failure		404	{object}	apperrors.AppError	"server.not_found: not the caller's, not there, or expired"
//	@Router			/me/inbox/{id}/seen [post]
func (h *inboxHandlerImpl) Seen(c fiber.Ctx) error { return h.mark(c, "seen") }

// Read godoc
//
//	@Summary		Mark one of my items read
//	@Description	The caller opened the item; read is seen too. A mark already there keeps its time.
//	@Tags			Inbox
//	@Produce		json
//	@Param			id	path		string	true	"The item"
//	@Success		200	{object}	handlers.InboxItem
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Failure		404	{object}	apperrors.AppError	"server.not_found: not the caller's, not there, or expired"
//	@Router			/me/inbox/{id}/read [post]
func (h *inboxHandlerImpl) Read(c fiber.Ctx) error { return h.mark(c, "read") }

// Unread godoc
//
//	@Summary		Mark one of my items unread
//	@Description	Takes the read mark off; seen stays.
//	@Tags			Inbox
//	@Produce		json
//	@Param			id	path		string	true	"The item"
//	@Success		200	{object}	handlers.InboxItem
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Failure		404	{object}	apperrors.AppError	"server.not_found: not the caller's, not there, or expired"
//	@Router			/me/inbox/{id}/unread [post]
func (h *inboxHandlerImpl) Unread(c fiber.Ctx) error { return h.mark(c, "unread") }

// Archive godoc
//
//	@Summary		Archive one of my items
//	@Description	The item leaves the inbox's default list and its counts (archived=only lists it). It still goes at its expiry.
//	@Tags			Inbox
//	@Produce		json
//	@Param			id	path		string	true	"The item"
//	@Success		200	{object}	handlers.InboxItem
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Failure		404	{object}	apperrors.AppError	"server.not_found: not the caller's, not there, or expired"
//	@Router			/me/inbox/{id}/archive [post]
func (h *inboxHandlerImpl) Archive(c fiber.Ctx) error { return h.mark(c, "archived") }

func (h *inboxHandlerImpl) markAll(c fiber.Ctx, read bool) error {
	me, err := owner(c)
	if err != nil {
		return err
	}
	n, err := h.db.MarkAllInboxItems(c.Context(), database.MarkAllInboxItemsParams{Read: read, UserID: me})
	if err != nil {
		return err
	}
	return c.JSON(InboxUpdated{Updated: n})
}

// ReadAll godoc
//
//	@Summary		Mark all my items read
//	@Description	Every item of the caller's inbox not yet read (archived ones left out) is marked read, and seen. Nobody else's item changes.
//	@Tags			Inbox
//	@Produce		json
//	@Success		200	{object}	handlers.InboxUpdated
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Router			/me/inbox/read-all [post]
func (h *inboxHandlerImpl) ReadAll(c fiber.Ctx) error { return h.markAll(c, true) }

// SeenAll godoc
//
//	@Summary		Mark all my items seen
//	@Description	Every item of the caller's inbox not yet seen (archived ones left out) is marked seen: the badge goes to 0. Nobody else's item changes.
//	@Tags			Inbox
//	@Produce		json
//	@Success		200	{object}	handlers.InboxUpdated
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required"
//	@Router			/me/inbox/seen-all [post]
func (h *inboxHandlerImpl) SeenAll(c fiber.Ctx) error { return h.markAll(c, false) }

// inboxVariantOf is a stored inbox variant as the API serves it.
func inboxVariantOf(row database.NotificationVariant) *InboxVariant {
	return &InboxVariant{Title: row.Title, Summary: row.Summary, Link: row.Link}
}
