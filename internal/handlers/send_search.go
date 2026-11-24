package handlers

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skynotify-backend/internal/apperrors"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
)

// maxSearch is the longest q a list takes, in characters: longer than any
// subject, name or address a person types to find one.
const maxSearch = 200

// Send kinds, as GET /mail_tasks filters and returns them; mail_task_kind()
// (20261124200000) is the rule: bulk (a list, a group, a Forms source, an
// approved send, a News send), system (mail a service or SkyNotify sent) and
// single (a person's single mail).
const (
	SendKindBulk   = "bulk"
	SendKindSingle = "single"
	SendKindSystem = "system"
)

// Send list orders (_sort): created_at, the default, and open_rate.
const (
	sendSortCreatedAt = "created_at"
	sendSortOpenRate  = "open_rate"
)

// errOpenRateSortForbidden refuses _sort=open_rate to a caller who may not
// read every send's tracking counts: the order would tell the counts GET
// /mail_tasks/{id}/tracking withholds from them.
var errOpenRateSortForbidden = apperrors.New("mail_task.sort_forbidden",
	"Sorting by open rate needs skynotify:mails:tracking:read.", fiber.StatusForbidden)

// sendListQuery is GET /mail_tasks's filters and order, read from the query.
type sendListQuery struct {
	filter    database.CountMailTaskSendsParams
	sort      string
	ascending bool
}

// read is the page of the send list the query asks for: newest first, the
// default, down idx_mail_tasks_created_at_id (ListMailTaskSends); any other
// order reads every matching send to order them (ListMailTaskSendsSorted).
func (q sendListQuery) read(ctx context.Context, db *database.Store, limit, offset int32) ([]database.ListMailTaskSendsRow, error) {
	if q.sort == "" && !q.ascending {
		return db.ListMailTaskSends(ctx, q.list(limit, offset))
	}
	rows, err := db.ListMailTaskSendsSorted(ctx, q.sorted(limit, offset))
	if err != nil {
		return nil, err
	}
	sends := make([]database.ListMailTaskSendsRow, len(rows))
	for i, row := range rows {
		// The two queries select the same columns.
		sends[i] = database.ListMailTaskSendsRow(row)
	}
	return sends, nil
}

// list is the page of the send list newest first.
func (q sendListQuery) list(limit, offset int32) database.ListMailTaskSendsParams {
	return database.ListMailTaskSendsParams{
		Limit:         limit,
		Offset:        offset,
		Status:        q.filter.Status,
		Search:        q.filter.Search,
		CreatedFrom:   q.filter.CreatedFrom,
		CreatedBefore: q.filter.CreatedBefore,
		Kind:          q.filter.Kind,
		TemplateID:    q.filter.TemplateID,
		ListID:        q.filter.ListID,
		SentBy:        q.filter.SentBy,
	}
}

// sorted is the page of the send list in the order the query asks for.
func (q sendListQuery) sorted(limit, offset int32) database.ListMailTaskSendsSortedParams {
	return database.ListMailTaskSendsSortedParams{
		Limit:         limit,
		Offset:        offset,
		Ascending:     q.ascending,
		Sort:          q.sort,
		Status:        q.filter.Status,
		Search:        q.filter.Search,
		CreatedFrom:   q.filter.CreatedFrom,
		CreatedBefore: q.filter.CreatedBefore,
		Kind:          q.filter.Kind,
		TemplateID:    q.filter.TemplateID,
		ListID:        q.filter.ListID,
		SentBy:        q.filter.SentBy,
	}
}

// parseSendListQuery reads status, q, from, to, kind, template_id, list_id,
// sent_by, _sort and _order. Every invalid value is named in one validation
// error; open_rate needs skynotify:mails:tracking:read.
func parseSendListQuery(c fiber.Ctx) (sendListQuery, error) {
	var out sendListQuery
	invalid := map[string]interface{}{}

	status, err := parseSendStatusFilter(c.Query("status"))
	if err != nil {
		return out, err
	}
	out.filter.Status = status

	if search, ok := searchParam(c.Query("q")); !ok {
		invalid["q"] = searchRule
	} else {
		out.filter.Search = search
	}

	zone := summaryLocation()
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		if from, ok := parseSearchInstant(raw, zone, false); ok {
			out.filter.CreatedFrom = &from
		} else {
			invalid["from"] = "must be a date (YYYY-MM-DD, Europe/Istanbul) or an RFC 3339 time"
		}
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		if before, ok := parseSearchInstant(raw, zone, true); ok {
			out.filter.CreatedBefore = &before
		} else {
			invalid["to"] = "must be a date (YYYY-MM-DD, Europe/Istanbul) or an RFC 3339 time"
		}
	}
	if out.filter.CreatedFrom != nil && out.filter.CreatedBefore != nil && !out.filter.CreatedFrom.Before(*out.filter.CreatedBefore) {
		invalid["to"] = "must not be before from"
	}

	switch kind := strings.ToLower(strings.TrimSpace(c.Query("kind"))); kind {
	case "":
	case SendKindBulk, SendKindSingle, SendKindSystem:
		out.filter.Kind = &kind
	default:
		invalid["kind"] = "must be one of bulk, single, system"
	}

	if raw := strings.TrimSpace(c.Query("template_id")); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			out.filter.TemplateID = &id
		} else {
			invalid["template_id"] = "must be a UUID"
		}
	}
	if raw := strings.TrimSpace(c.Query("list_id")); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			out.filter.ListID = &id
		} else {
			invalid["list_id"] = "must be a UUID"
		}
	}
	if raw := strings.TrimSpace(c.Query("sent_by")); raw != "" {
		if len(raw) > 255 || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
			invalid["sent_by"] = "must be valid UTF-8 text without NUL, at most 255 bytes"
		} else {
			out.filter.SentBy = &raw
		}
	}

	switch sort := strings.ToLower(strings.TrimSpace(c.Query("_sort"))); sort {
	case "", sendSortCreatedAt:
	case sendSortOpenRate:
		out.sort = sendSortOpenRate
	default:
		invalid["_sort"] = "must be one of created_at, open_rate"
	}
	switch order := strings.ToLower(strings.TrimSpace(c.Query("_order"))); order {
	case "", "desc":
	case "asc":
		out.ascending = true
	default:
		invalid["_order"] = "must be one of asc, desc"
	}

	if len(invalid) > 0 {
		return out, apperrors.ErrValidation.WithParams(invalid)
	}
	if out.sort == sendSortOpenRate && !contentCallerOf(c).has(TrackingReadRole) {
		return out, errOpenRateSortForbidden
	}
	return out, nil
}

// searchRule is what a q must be.
const searchRule = "must be valid UTF-8 text without NUL, at most 200 characters"

// searchParam is a list's q: trimmed, nil when empty, false when it is not
// valid UTF-8, holds a NUL (PostgreSQL's text takes neither) or is longer than
// maxSearch characters.
func searchParam(raw string) (*string, bool) {
	q := strings.TrimSpace(raw)
	if q == "" {
		return nil, true
	}
	if !utf8.ValidString(q) || strings.ContainsRune(q, 0) || utf8.RuneCountInString(q) > maxSearch {
		return nil, false
	}
	return &q, true
}

// parseSearchInstant reads from or to: a calendar day in zone, or an RFC 3339
// time. Both bounds include what they name, so to is returned as the first
// instant after it: the day after a date, a microsecond (PostgreSQL's
// precision) after a time.
func parseSearchInstant(raw string, zone *time.Location, end bool) (time.Time, bool) {
	if day, err := time.ParseInLocation(time.DateOnly, raw, zone); err == nil {
		if end {
			return day.AddDate(0, 0, 1), true
		}
		return day, true
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil && strings.Contains(raw, " ") {
		// An unencoded + in a query string reads as a space:
		// 2026-11-10T10:00:00+03:00 arrives as 2026-11-10T10:00:00 03:00.
		at, err = time.Parse(time.RFC3339Nano, strings.Replace(raw, " ", "+", 1))
	}
	if err != nil {
		return time.Time{}, false
	}
	if end {
		return at.Truncate(time.Microsecond).Add(time.Microsecond), true
	}
	return at, true
}

// summaryLocation is summaryTimeZone, or Türkiye's fixed UTC+3 when the
// image has no zone database.
func summaryLocation() *time.Location {
	if zone, err := time.LoadLocation(summaryTimeZone); err == nil {
		return zone
	}
	return time.FixedZone(summaryTimeZone, 3*60*60)
}

// errSearchTooLong refuses a q searchParam does not take.
var errSearchTooLong = apperrors.ErrValidation.WithParams(map[string]interface{}{
	"q": searchRule,
})

// recipientStatusSkipped is the recipient filter for failed rows not sent on
// purpose: a reserved domain, a suppressed or unsubscribed address, a
// preference, a cancelled send (mail_task_status() leaves them out of failed).
const recipientStatusSkipped = "skipped"

// parseRecipientListFilter is parseRecipientStatusFilter with skipped: the
// failed status, narrowed to the rows not sent on purpose.
func parseRecipientListFilter(raw string) (database.NullMailQueueStatus, bool, error) {
	if strings.EqualFold(strings.TrimSpace(raw), recipientStatusSkipped) {
		return database.NullMailQueueStatus{MailQueueStatus: database.MailQueueStatusFailed, Valid: true}, true, nil
	}
	status, err := parseRecipientStatusFilter(raw)
	if err != nil {
		return status, false, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"status": "must be one of pending, processing, sent, failed, skipped",
		})
	}
	return status, false, nil
}

// groupsMatching is the Keycloak groups whose name or path contains search
// as searchFold compares them; every group when search is nil.
func groupsMatching(groups []*gocloak.Group, search *string) []*gocloak.Group {
	if search == nil {
		return groups
	}
	needle := searchFold(*search)
	kept := make([]*gocloak.Group, 0, len(groups))
	for _, g := range groups {
		if containsFold(g.Name, needle) || containsFold(g.Path, needle) {
			kept = append(kept, g)
		}
	}
	return kept
}

func containsFold(value *string, folded string) bool {
	return value != nil && strings.Contains(searchFold(*value), folded)
}

// turkishFold is skynotify_search_fold()'s translate (20261124200000): Turkish
// capitals to their small letters, the four i's to i.
var turkishFold = strings.NewReplacer("İ", "i", "I", "i", "ı", "i", "Ş", "ş", "Ğ", "ğ", "Ü", "ü", "Ö", "ö", "Ç", "ç")

// searchFold is text as skynotify_search_fold() compares it.
func searchFold(value string) string {
	return strings.ToLower(turkishFold.Replace(value))
}
