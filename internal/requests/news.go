package requests

// NewsSendAudience is who a News send goes to (SPEC §5.8): every Member, or
// the members of some teams and their followers. core resolves it.
type NewsSendAudience struct {
	// members: every Member (category club:<kind>). teams: the named teams, their sub-teams and their followers (team:<id>:<kind>).
	Type string `json:"type" validate:"required,oneof=members teams" enums:"members,teams"`
	// With teams, 1 to 32 teams, each a team's Keycloak Group path (/UYELER/<area>/<team>) or id; refused with members.
	Teams []string `json:"teams" validate:"omitempty,max=32,dive,required,max=255"`
}

// SendNews mails a published News record (POST /v1/news_sends).
type SendNews struct {
	// The record's slug in inscribed's news collection.
	Slug     string           `json:"slug" validate:"required,max=200" example:"bahar-donemi-kayitlari"`
	Audience NewsSendAudience `json:"audience" validate:"required"`
	// full: the whole body in the mail. summary: the summary and a link to read on.
	Format string `json:"format" validate:"required,oneof=full summary" enums:"full,summary"`
	// The mail's subject; empty: the record's title, a duyuru's after "Duyuru: ".
	Subject string `json:"subject" validate:"max=200"`
	// The preview text mail clients show beside the subject; empty: the record's summary.
	Preheader string `json:"preheader" validate:"max=300"`
	// Required and true: the sender's statement that the mail's main purpose is not promotion (KVKK notice s6 §5.10): a short thanks to a sponsor or its logo is allowed.
	CommercialContentAbsent *bool `json:"commercial_content_absent"`
	// Open and click tracking, by the rules of every person's send (SKYMAIL_TRACKING, SKYMAIL_TRACKING_OPERATOR).
	Tracking bool `json:"tracking"`
}
