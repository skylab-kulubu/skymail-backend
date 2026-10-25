// Package news makes a SkyMail send of a published News record (hub ticket
// duyuru-haber 06; SPEC §5.7, §5.8; README, "Haberden gönderim"): it reads
// the record from inscribed (CMS), asks core who it goes to, and turns the
// record into the club.news template's variables. The send itself is
// handlers.NewsSendHandler's.
package news

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound: inscribed has no published record of the slug. An archived
// record answers the same, as inscribed hides it from an anonymous read.
var ErrNotFound = errors.New("news: no published record of this slug")

// The record's audience values (ADR 0067 karar 3). In Faz 1 inscribed holds
// only public records; an absent audience is public.
const (
	AudiencePublic  = "herkes"
	AudienceMembers = "uyeler"
	AudienceTeams   = "ekipler"
)

// The record's kinds; an absent kind is a haber (SPEC §5.2).
const (
	KindNews         = "haber"
	KindAnnouncement = "duyuru"
)

// maxRecord bounds what is read of inscribed's answer: a News record's body
// is a page of text, well under it.
const maxRecord = 2 << 20

// Link is the record's call-to-action button.
type Link struct {
	Href  string `json:"href"`
	Label string `json:"label"`
}

// Record is a published News record as a send keeps it (mail_tasks
// .source_snapshot): what the mail is made of, and nothing of who reads it.
type Record struct {
	// Slug is the record's canonical slug, as inscribed answers it (an old
	// alias is followed to it).
	Slug      string    `json:"slug"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary,omitempty"`
	// Body is the record's body as inscribed holds it: HTML, or plain text
	// in a record from before the schema's second version (bilet 01).
	Body      string   `json:"body"`
	HeroImage string   `json:"heroImage,omitempty"`
	HeroAlt   string   `json:"heroAlt,omitempty"`
	Cta       *Link    `json:"cta,omitempty"`
	Kind      string   `json:"kind"`
	Audience  string   `json:"audience"`
	Teams     []string `json:"teams,omitempty"`
}

// CMS reads News records from inscribed anonymously, as the site and
// sky-app do: Faz 1's records are all public (ADR 0067 karar 3). A private
// record is Faz 1b's, read with a service account (bilet 04 madde 5).
type CMS struct {
	base string
	http *http.Client
}

// NewCMS reads from inscribed at base (SKYMAIL_NEWS_CMS_URL: its address
// without the /cms path, e.g. http://inscribed:8080 on the Docker network).
// A nil client is a plain http.Client with a 10 second timeout.
func NewCMS(base string, client *http.Client) *CMS {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &CMS{base: strings.TrimRight(base, "/"), http: client}
}

type cmsItem struct {
	Slug      *string         `json:"slug"`
	Version   *int            `json:"version"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Data      json.RawMessage `json:"data"`
}

type cmsData struct {
	Title     string          `json:"title"`
	Summary   string          `json:"summary"`
	Body      string          `json:"body"`
	HeroImage string          `json:"heroImage"`
	HeroAlt   string          `json:"heroAlt"`
	Cta       json.RawMessage `json:"cta"`
	Kind      string          `json:"kind"`
	Audience  string          `json:"audience"`
	Teams     []string        `json:"teams"`
}

// Get reads the published record of slug. A slug inscribed does not know, or
// whose record is archived, is ErrNotFound; any other failure (inscribed
// unreachable, an answer off the contract) is an error that names no
// content.
func (c *CMS) Get(ctx context.Context, slug string) (Record, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/cms/collections/news/"+url.PathEscape(slug), nil)
	if err != nil {
		return Record{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Record{}, fmt.Errorf("news cms: %w", withoutURL(err))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return Record{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return Record{}, fmt.Errorf("news cms: status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxRecord+1))
	if err != nil {
		return Record{}, fmt.Errorf("news cms: %w", err)
	}
	if len(raw) > maxRecord {
		return Record{}, errors.New("news cms: the record is too large")
	}
	return parseRecord(raw)
}

func parseRecord(raw []byte) (Record, error) {
	var item cmsItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return Record{}, errors.New("news cms: the answer is not a record")
	}
	if item.Slug == nil || strings.TrimSpace(*item.Slug) == "" || item.Version == nil || len(item.Data) == 0 {
		return Record{}, errors.New("news cms: the record has no slug, version or data")
	}
	var data cmsData
	if err := json.Unmarshal(item.Data, &data); err != nil {
		return Record{}, errors.New("news cms: the record's data is off the news schema")
	}
	record := Record{
		Slug: *item.Slug, Version: *item.Version, UpdatedAt: item.UpdatedAt.UTC(),
		Title: strings.TrimSpace(data.Title), Summary: strings.TrimSpace(data.Summary), Body: data.Body,
		HeroImage: strings.TrimSpace(data.HeroImage), HeroAlt: strings.TrimSpace(data.HeroAlt),
		Kind: data.Kind, Audience: data.Audience, Teams: data.Teams,
	}
	if record.Title == "" || strings.TrimSpace(record.Body) == "" {
		return Record{}, errors.New("news cms: the record has no title or body")
	}
	if len(data.Cta) > 0 && string(data.Cta) != "null" {
		var cta Link
		if err := json.Unmarshal(data.Cta, &cta); err == nil && strings.TrimSpace(cta.Href) != "" {
			cta.Href, cta.Label = strings.TrimSpace(cta.Href), strings.TrimSpace(cta.Label)
			record.Cta = &cta
		}
	}
	// What an absent field means (SPEC §5.2).
	if record.Kind == "" {
		record.Kind = KindNews
	}
	if record.Kind != KindNews && record.Kind != KindAnnouncement {
		return Record{}, errors.New("news cms: the record's kind is off the news schema")
	}
	if record.Audience == "" {
		record.Audience = AudiencePublic
	}
	return record, nil
}
