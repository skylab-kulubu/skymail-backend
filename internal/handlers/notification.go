package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// NotificationsSendRole lets a service send POST /v1/notifications (hub
// ticket iletisim-servisi 04 gives it to core's service account).
const NotificationsSendRole = "skymail:notifications:send"

// IdempotencyKeyHeader names the header every POST /v1/notifications carries.
const IdempotencyKeyHeader = "Idempotency-Key"

// The channels a notification type may go by. Only e-mail works today; the
// others come with their tickets (05 inbox, 12 push, 13 sms).
const (
	ChannelEmail = "email"
)

// Why a recipient got nothing on a channel.
const (
	// An optional type, and the person's allowed_channels do not hold the channel.
	SkipNotAllowed = "not_allowed"
	// Keycloak has no user of that sub.
	SkipUnknownPerson = "unknown_person"
	// The Keycloak account is disabled.
	SkipDisabled = "disabled"
	// No address on the channel: none in the request and none in Keycloak.
	SkipNoAddress = "no_address"
	// The address Keycloak holds is not verified.
	SkipUnverified = "unverified"
)

var (
	errIdempotencyKeyRequired = apperrors.New(
		"notification.idempotency_key_required",
		"POST /v1/notifications needs an Idempotency-Key header: 1 to 255 visible ASCII characters, the same for every retry of one notification.",
		fiber.StatusBadRequest,
	)
	errIdempotencyKeyReused = apperrors.New(
		"notification.idempotency_key_reused",
		"This Idempotency-Key was already used for a different request. Use a new key for a new notification.",
		fiber.StatusUnprocessableEntity,
	)
	errNotificationTypeUnknown = apperrors.New(
		"notification.type_unknown",
		"There is no notification type of this key in use (GET /notification_types).",
		fiber.StatusUnprocessableEntity,
	)
	errAllowedChannelsRequired = apperrors.New(
		"notification.allowed_channels_required",
		"The type is optional: every recipient needs allowed_channels, the channels their preferences allow ([] for none).",
		fiber.StatusBadRequest,
	)
	errEmailVariantMissing = apperrors.New(
		"notification.email_variant_missing",
		"The type goes by e-mail but has no template of its key in use.",
		fiber.StatusConflict,
	)
	errDirectoryUnavailable = apperrors.New(
		"notification.directory_unavailable",
		"Keycloak could not be asked for a recipient's address. Nothing was queued; retry with the same Idempotency-Key.",
		fiber.StatusServiceUnavailable,
	)
	errNotificationCategory = apperrors.New(
		"notification_type.invalid_category",
		"A category is lower-case letters, digits, dots, dashes and underscores, 2 to 64 characters, starting and ending with a letter or a digit.",
		fiber.StatusBadRequest,
	)
)

var notificationCategory = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}[a-z0-9]$`)

// PersonDirectory reads a person by their Keycloak sub; nil, nil when there
// is none.
type PersonDirectory interface {
	GetUser(ctx context.Context, id string) (*gocloak.User, error)
}

type NotificationHandler interface {
	Send(c fiber.Ctx) error
	ListTypes(c fiber.Ctx) error
	GetType(c fiber.Ctx) error
	PutType(c fiber.Ctx) error
}

type notificationHandlerImpl struct {
	db     *database.Store
	mailer mailer.Transactional
	people PersonDirectory
}

func NewNotificationHandler(db *database.Store, m mailer.Transactional, people PersonDirectory) NotificationHandler {
	return &notificationHandlerImpl{db: db, mailer: m, people: people}
}

// NotificationType is a notification type as the API serves it.
type NotificationType struct {
	Key string `json:"key"`
	// The preference category; null when none.
	Category *string `json:"category"`
	// A required type does not look at preferences.
	Required        bool     `json:"required"`
	DefaultChannels []string `json:"default_channels"`
	Lane            string   `json:"lane" enums:"system,bulk"`
	// Set when the type is out of use: POST /notifications refuses it.
	ArchivedAt *time.Time `json:"archived_at"`
}

func notificationTypeOf(row database.NotificationType) NotificationType {
	t := NotificationType{Key: row.Key, Category: row.Category, Required: row.Required, DefaultChannels: row.DefaultChannels, Lane: row.Lane, ArchivedAt: row.ArchivedAt}
	if t.DefaultChannels == nil {
		t.DefaultChannels = []string{}
	}
	return t
}

// ChannelResult is what a notification did on one channel.
type ChannelResult struct {
	// The send its deliveries on this channel belong to (GET /mail_tasks/{id} for e-mail); null when nobody was queued.
	TaskID *uuid.UUID `json:"task_id"`
	// Recipients queued on the channel.
	Queued int `json:"queued"`
	// Recipients whose message did not render: written failed on the send.
	Failed int `json:"failed"`
	// Recipients left out, by reason: not_allowed, unknown_person, disabled, no_address, unverified.
	Skipped map[string]int `json:"skipped"`
}

// NotificationResult is the answer to POST /v1/notifications, and the same
// answer again for a retry with its Idempotency-Key.
type NotificationResult struct {
	// The notification.
	ID   uuid.UUID `json:"id"`
	Type string    `json:"type"`
	// The distinct people named.
	Recipients int `json:"recipients"`
	// Recipients named again (the same sub), left out.
	Duplicates int `json:"duplicates"`
	// By channel, each of the type's default channels.
	Channels map[string]ChannelResult `json:"channels"`
}

// person is one recipient as the channels see them: the request's fields,
// and Keycloak's when the request left the address out.
type person struct {
	recipient requests.NotificationRecipient
	looked    bool
	user      *gocloak.User
}

// emailOf is the person's address and name for e-mail, or why there is none:
// the request's address when it has one; else Keycloak's primary e-mail,
// for an enabled account whose address is verified.
func (h *notificationHandlerImpl) emailOf(ctx context.Context, p *person) (mailer.RecipientInfo, string, error) {
	sub := p.recipient.Sub
	info := mailer.RecipientInfo{FullName: strings.TrimSpace(p.recipient.Name), Email: strings.TrimSpace(p.recipient.Email), Sub: &sub}
	if info.Email == "" && !p.looked {
		user, err := h.people.GetUser(ctx, sub.String())
		if err != nil {
			return info, "", err
		}
		p.looked, p.user = true, user
	}
	if info.Email == "" {
		user := p.user
		switch {
		case user == nil:
			return info, SkipUnknownPerson, nil
		case !gocloak.PBool(user.Enabled):
			return info, SkipDisabled, nil
		case strings.TrimSpace(gocloak.PString(user.Email)) == "":
			return info, SkipNoAddress, nil
		case !gocloak.PBool(user.EmailVerified):
			return info, SkipUnverified, nil
		}
		info.Email = strings.TrimSpace(gocloak.PString(user.Email))
	}
	if info.FullName == "" && p.user != nil {
		info.FullName = strings.TrimSpace(gocloak.PString(p.user.FirstName) + " " + gocloak.PString(p.user.LastName))
		if info.FullName == "" {
			info.FullName = gocloak.PString(p.user.Username)
		}
	}
	return info, "", nil
}

// channelAllowed is the rule (hub ticket iletisim-servisi 03): a required
// type goes on its channels whatever the person allows; an optional one only
// on those core says the person allows.
func channelAllowed(required bool, allowed *[]string, channel string) bool {
	if required {
		return true
	}
	return allowed != nil && slices.Contains(*allowed, channel)
}

func validIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// requestSHA256 is a SHA-256 of a request as it was understood: the same
// request hashes the same whatever its JSON looked like.
func requestSHA256(params requests.SendNotification) (string, error) {
	canonical, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

var errNotificationReplay = errors.New("idempotency key already used")

// Send godoc
//
//	@Summary		Send a notification to people
//	@Description	core says what happened (type) and to whom (recipients, by Keycloak sub); SkyMail picks each person's channels: the type's default channels, for an optional type only those in the person's allowed_channels (a required type ignores them), and only where the person has an address. E-mail goes to the request's address or, without one, to the person's primary e-mail in Keycloak; a person Keycloak does not know, a disabled account and an unverified address are skipped and counted. The e-mail is the template of the type's key, queued in the type's lane. The Idempotency-Key header is required: the same key from the same caller answers the first answer again (Idempotent-Replayed: true) and queues nothing; a different request with it is 422. Only a service account's token with skymail:notifications:send may call it.
//	@Tags			Notifications
//	@Accept			json
//	@Produce		json
//	@Param			Idempotency-Key	header		string						true	"The notification's own key: 1 to 255 visible ASCII characters, the same on every retry"
//	@Param			notification	body		requests.SendNotification	true	"The notification"
//	@Success		201				{object}	handlers.NotificationResult
//	@Header			201				{string}	Idempotent-Replayed	"true when the answer is the first answer to this Idempotency-Key"
//	@Failure		400				{object}	apperrors.AppError	"validation.error; notification.idempotency_key_required; notification.allowed_channels_required (params.recipient: its index)"
//	@Failure		403				{object}	apperrors.AppError	"server.forbidden: no skymail:notifications:send; auth.service_account_required: a person's token"
//	@Failure		409				{object}	apperrors.AppError	"notification.email_variant_missing: the type goes by e-mail and has no template of its key"
//	@Failure		422				{object}	apperrors.AppError	"notification.type_unknown; notification.idempotency_key_reused"
//	@Failure		503				{object}	apperrors.AppError	"notification.directory_unavailable: Keycloak did not answer; nothing was queued"
//	@Router			/notifications [post]
func (h *notificationHandlerImpl) Send(c fiber.Ctx) error {
	key := c.Get(IdempotencyKeyHeader)
	if !validIdempotencyKey(key) {
		return errIdempotencyKeyRequired
	}
	var params requests.SendNotification
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	sentBy, ok := c.Locals("user_id").(string)
	if !ok || sentBy == "" {
		return apperrors.ErrForbidden
	}
	hash, err := requestSHA256(params)
	if err != nil {
		return err
	}
	ctx := c.Context()

	// A retry answers before anything is looked up again.
	if done, err := h.replay(c, sentBy, key, hash); done || err != nil {
		return err
	}

	kind, err := h.db.GetNotificationType(ctx, params.Type)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind.ArchivedAt != nil) {
		return errNotificationTypeUnknown.WithParams(map[string]interface{}{"type": params.Type})
	}
	if err != nil {
		return err
	}
	if !kind.Required {
		for i, r := range params.Recipients {
			if r.AllowedChannels == nil {
				return errAllowedChannelsRequired.WithParams(map[string]interface{}{"recipient": i})
			}
		}
	}
	var template database.Template
	if slices.Contains(kind.DefaultChannels, ChannelEmail) {
		key := kind.Key
		template, err = h.db.GetTemplateByKey(ctx, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			return errEmailVariantMissing.WithParams(map[string]interface{}{"type": kind.Key})
		}
		if err != nil {
			return err
		}
	}

	// Each person once, in the order named.
	people := make([]*person, 0, len(params.Recipients))
	seen := make(map[uuid.UUID]bool, len(params.Recipients))
	result := NotificationResult{Type: kind.Key, Channels: map[string]ChannelResult{}}
	for _, r := range params.Recipients {
		if seen[r.Sub] {
			result.Duplicates++
			continue
		}
		seen[r.Sub] = true
		people = append(people, &person{recipient: r})
	}
	result.Recipients = len(people)

	var emails []mailer.RecipientInfo
	for _, channel := range kind.DefaultChannels {
		outcome := ChannelResult{Skipped: map[string]int{}}
		for _, p := range people {
			if !channelAllowed(kind.Required, p.recipient.AllowedChannels, channel) {
				outcome.Skipped[SkipNotAllowed]++
				continue
			}
			// Only e-mail is a channel today (the CHECK on default_channels).
			info, skip, err := h.emailOf(ctx, p)
			if err != nil {
				log.Error().Err(err).Str("type", kind.Key).Msg("notification: Keycloak did not answer a recipient's lookup")
				return errDirectoryUnavailable
			}
			if skip != "" {
				outcome.Skipped[skip]++
				continue
			}
			emails = append(emails, info)
		}
		result.Channels[channel] = outcome
	}

	priority := mailer.PriorityBulk
	if kind.Lane == "system" {
		priority = mailer.PrioritySystem
	}
	bodyVariables, err := json.Marshal(params.Variables)
	if err != nil {
		return err
	}
	if params.Variables == nil {
		bodyVariables = []byte("{}")
	}
	var sourceKind, sourceRef *string
	if params.Source != nil {
		sourceKind = &params.Source.Kind
		if params.Source.Ref != "" {
			sourceRef = &params.Source.Ref
		}
	}

	queued := false
	err = h.db.InTx(ctx, func(q *database.Queries) error {
		id, err := q.InsertNotification(ctx, database.InsertNotificationParams{
			Type: kind.Key, SentBy: sentBy, IdempotencyKey: key, RequestSha256: hash,
			SourceKind: sourceKind, SourceRef: sourceRef,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Another request with this key committed while this one looked
			// people up.
			return errNotificationReplay
		}
		if err != nil {
			return err
		}
		result.ID = id
		if len(emails) > 0 {
			taskID, err := h.mailer.Queue(q, priority).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
				SentBy:        sentBy,
				TemplateID:    template.ID,
				BodyVariables: bodyVariables,
				Recipients:    emails,
			})
			if err != nil {
				return err
			}
			if err := q.SetMailTaskNotification(ctx, database.SetMailTaskNotificationParams{NotificationID: id, ID: taskID}); err != nil {
				return err
			}
			rows, err := q.CountMailTaskRows(ctx, taskID)
			if err != nil {
				return err
			}
			email := result.Channels[ChannelEmail]
			email.TaskID = &taskID
			email.Queued = int(rows.Queued)
			email.Failed = int(rows.Failed)
			result.Channels[ChannelEmail] = email
			queued = rows.Queued > 0
		}
		answer, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return q.SetNotificationResult(ctx, database.SetNotificationResultParams{ID: id, Result: answer})
	})
	if errors.Is(err, errNotificationReplay) {
		if done, err := h.replay(c, sentBy, key, hash); done || err != nil {
			return err
		}
		return apperrors.ErrConflict
	}
	if err != nil {
		return err
	}
	if queued {
		h.mailer.Wake()
	}
	logNotification(result, kind.Lane)

	return c.Status(fiber.StatusCreated).JSON(result)
}

// replay answers a request whose Idempotency-Key its caller already used:
// the first answer again for the same request, 422 for another. done is false
// when the key is new.
func (h *notificationHandlerImpl) replay(c fiber.Ctx, sentBy, key, hash string) (bool, error) {
	earlier, err := h.db.GetNotificationByIdempotencyKey(c.Context(), database.GetNotificationByIdempotencyKeyParams{SentBy: sentBy, IdempotencyKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if earlier.RequestSha256 != hash {
		return true, errIdempotencyKeyReused
	}
	var result NotificationResult
	if err := json.Unmarshal(earlier.Result, &result); err != nil {
		return true, err
	}
	c.Set("Idempotent-Replayed", "true")
	return true, c.Status(fiber.StatusCreated).JSON(result)
}

// logNotification writes the line the per-type and per-channel counts are
// read from: the type, the lane and each channel's counts. No person, address
// or variable is in it.
func logNotification(result NotificationResult, lane string) {
	for channel, outcome := range result.Channels {
		event := log.Info().
			Str("event", "notification_queued").
			Str("notification_id", result.ID.String()).
			Str("type", result.Type).
			Str("lane", lane).
			Str("channel", channel).
			Int("queued", outcome.Queued).
			Int("failed", outcome.Failed)
		for reason, n := range outcome.Skipped {
			event = event.Int("skipped_"+reason, n)
		}
		event.Msg("notification queued")
	}
}

// ListTypes godoc
//
//	@Summary		List the notification types
//	@Description	Every notification type, archived ones included, by key: its preference category, whether it is required, its default channels and its lane.
//	@Tags			Notifications
//	@Produce		json
//	@Success		200	{array}		handlers.NotificationType
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Router			/notification_types [get]
func (h *notificationHandlerImpl) ListTypes(c fiber.Ctx) error {
	rows, err := h.db.ListNotificationTypes(c.Context())
	if err != nil {
		return err
	}
	types := make([]NotificationType, len(rows))
	for i, row := range rows {
		types[i] = notificationTypeOf(row)
	}
	return c.JSON(types)
}

// GetType godoc
//
//	@Summary	Get a notification type
//	@Tags		Notifications
//	@Produce	json
//	@Param		key	path		string	true	"The type's key"
//	@Success	200	{object}	handlers.NotificationType
//	@Failure	403	{object}	apperrors.AppError	"Forbidden"
//	@Failure	404	{object}	apperrors.AppError	"Not Found"
//	@Router		/notification_types/{key} [get]
func (h *notificationHandlerImpl) GetType(c fiber.Ctx) error {
	row, err := h.db.GetNotificationType(c.Context(), c.Params("key"))
	if err != nil {
		return err
	}
	return c.JSON(notificationTypeOf(row))
}

// PutType godoc
//
//	@Summary		State a notification type (Template seed)
//	@Description	Writes the type of the key as the Template seed states it: its preference category (null: none), whether it is required (preferences not looked at), its default channels (today only email) and its queue lane. A type that exists is written over; its archive stays as it is. The type's e-mail variant is the template of the same key (PUT /templates/by-key/{key}).
//	@Tags			Notifications
//	@Accept			json
//	@Produce		json
//	@Param			key		path		string							true	"The type's key, the Template key's alphabet"
//	@Param			body	body		requests.PutNotificationType	true	"The type"
//	@Success		200		{object}	handlers.NotificationType
//	@Failure		400		{object}	apperrors.AppError	"validation.error; notification_type.invalid_category"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Router			/notification_types/{key} [put]
func (h *notificationHandlerImpl) PutType(c fiber.Ctx) error {
	key := c.Params("key")
	if !validator.IsTemplateKey(key) {
		return errInvalidTemplateKey
	}
	var params requests.PutNotificationType
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	if params.Category != nil && !notificationCategory.MatchString(*params.Category) {
		return errNotificationCategory
	}
	row, err := h.db.UpsertNotificationType(c.Context(), database.UpsertNotificationTypeParams{
		Key:             key,
		Category:        params.Category,
		Required:        params.Required,
		DefaultChannels: params.DefaultChannels,
		Lane:            params.Lane,
	})
	if err != nil {
		return err
	}
	return c.JSON(notificationTypeOf(row))
}
