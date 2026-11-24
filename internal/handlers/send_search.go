package handlers

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// maxSearch is the longest q a list takes, in characters: longer than any
// subject, name or address a person types to find one.
const maxSearch = 200

// Send kinds, as GET /mail_tasks filters and returns them: bulk (a list, a
// group, a Forms source, an approved send; mail_task_bulk), system (a System
// template's mail that is not bulk: a login code, a ticket, a Forms
// confirmation) and single (any other: a person's mail to one or a few).
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
	"Sorting by open rate needs skymail:mails:tracking:read.", fiber.StatusForbidden)

// sendKindOf is a send's kind by the rule the kind filter uses.
func sendKindOf(bulk, templateSystem bool) string {
	switch {
	case bulk:
		return SendKindBulk
	case templateSystem:
		return SendKindSystem
	default:
		return SendKindSingle
	}
}

// sendListQuery is GET /mail_tasks's filters and order, read from the query.
type sendListQuery struct {
	filter    database.CountMailTaskSendsParams
	sort      string
	ascending bool
}

// list is the page of the send list the query asks for.
func (q sendListQuery) list(limit, offset int32) database.ListMailTaskSendsParams {
	return database.ListMailTaskSendsParams{
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
// error; open_rate needs skymail:mails:tracking:read.
func parseSendListQuery(c fiber.Ctx) (sendListQuery, error) {
	var out sendListQuery
	invalid := map[string]interface{}{}

	status, err := parseSendStatusFilter(c.Query("status"))
	if err != nil {
		return out, err
	}
	out.filter.Status = status

	if search, ok := searchParam(c.Query("q")); !ok {
		invalid["q"] = "must be at most 200 characters"
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
		if len(raw) > 255 {
			invalid["sent_by"] = "must be at most 255 characters"
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

// searchParam is a list's q: trimmed, nil when empty, false when longer than
// maxSearch characters.
func searchParam(raw string) (*string, bool) {
	q := strings.TrimSpace(raw)
	if q == "" {
		return nil, true
	}
	if utf8.RuneCountInString(q) > maxSearch {
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

// errSearchTooLong refuses a q longer than maxSearch characters.
var errSearchTooLong = apperrors.ErrValidation.WithParams(map[string]interface{}{
	"q": "must be at most 200 characters",
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

// groupsMatching is the Keycloak groups whose name or path contains search,
// case-insensitively; every group when search is nil.
func groupsMatching(groups []*gocloak.Group, search *string) []*gocloak.Group {
	if search == nil {
		return groups
	}
	needle := strings.ToLower(*search)
	kept := make([]*gocloak.Group, 0, len(groups))
	for _, g := range groups {
		if containsFold(g.Name, needle) || containsFold(g.Path, needle) {
			kept = append(kept, g)
		}
	}
	return kept
}

func containsFold(value *string, lowered string) bool {
	return value != nil && strings.Contains(strings.ToLower(*value), lowered)
}
