# Template and mailing-list lifecycle

SkyMail retains durable templates, internal mailing lists and historical mail
delivery records. Operator-facing delete commands archive these records; they do
not physically remove them.

## API contract

- `DELETE /v1/templates/{id}` archives a template and returns `204`. Repeating
  the command for the same template is safe and leaves the original archive
  time and actor unchanged.
- `POST /v1/templates/{id}/restore` restores a template and returns the
  restored record. Repeating the command is safe.
- `DELETE /v1/mailing_lists/{id}` and
  `POST /v1/mailing_lists/{id}/restore` provide the same lifecycle for an
  internal mailing list. Keycloak groups are read-only list sources and are not
  archived by these routes.
- Normal item reads, updates, recipient reads and list queries exclude archived
  records. Management list queries accept `lifecycle=current|inactive|all`;
  the default is `current`.
- Restore commands use the existing template/list write permission. A database
  uniqueness conflict is returned as `409 Conflict`; archived records do not
  release an existing unique key for reuse.

Archive metadata is recorded as `archived_at` and `archived_by`. The actor is
the authenticated Keycloak subject when available.

## Mail template versions

From the release that adds `template_versions`, every write through the API
that changes what a version holds — the name, the subject, a source, which
source is main, the rendered HTML or plain text — records a Mail template
version, and versions are never deleted. A write that changes none of these (a
repeated seed, an identical save) records none. The migration gives every
template that exists then one published first version from its row. Names
joined the versions later (migration `20260923230000`): the versions that
existed then carry the name their template had at that moment, and from then
on renaming a template — in the old panel, or through a draft's `name` — is a
version like any other change.

The template row is a copy of the published version, and
`published_version_id` names it. Archiving and restoring a template changes
only the row; its versions stay readable through
`GET /v1/templates/{id}/versions` whether it is archived or not.

Drafts are versions too. Saving a draft
(`POST /v1/templates/{id}/drafts`) and restoring a version
(`POST /v1/templates/{id}/versions/{versionId}/restore`) each record a new,
unpublished version; publishing one
(`POST /v1/templates/{id}/versions/{versionId}/publish`) marks it published
and copies it onto the row. An operator's newest version of a template, while
unpublished, is their draft in progress; their earlier drafts stay in the
history, superseded, and so does a draft nobody publishes. Discarding a draft
(`POST /v1/templates/{id}/versions/{versionId}/discard`) sets its
`discarded_at`: it stays in the history, readable and restorable, but it is
nobody's draft in progress and is never published. Drafts are not the
ephemeral "expired drafts" ADR-0042 has hard-deleted:
a Mail template draft is part of the template's history (ADR-0046), and no
version is deleted. Like any other update, saving, restoring or publishing a
version of an archived template answers `404` until the template is
un-archived with `POST /v1/templates/{id}/restore`.

During the deploy, the migration runs before the new binary takes over, and
the old binary keeps writing rows without versions in between. A template
written in that window has no version for that write: its row, which is what
is sent, differs from the version marked current until the template's next
write, which records the row as it then is.

## Template seed

The Template seed writes the Mail templates kept in skymail-frontend's
`emails/` by key, through `PUT /v1/templates/by-key/{key}`. It and SkyMail's
editor both write templates, and neither silently overwrites the other
(ADR-0047). The seed writes everything it sends — the subject too — as a
Template seed version, published at once; `react_email_content` holding a JSX
source makes that source the version's JSX Main source (the seed's older
pointer comment is no source, and its body is then the HTML Main source).

Unless the request says `?force=true`, a seed that would change a template an
operator changed since the last seed is refused with
`409 template.seed_conflict`, and nothing is written. It is refused when any of
these holds; `params.rules` lists each one that does:

- `published_by_operator` — the version the template sends is not the last
  Template seed version: an operator published since, or no seed ever wrote it.
- `newer_operator_version` — an operator wrote a version numbered after the
  last seed version, a draft included. A discarded draft does not count.
- `operator_subject` — the subject sent is an operator's and the seed would
  overwrite it: it is neither the subject the last seed version asked for (that
  seed kept it, as seeds did before this rule, or an operator published it
  since) nor the one the seed asks for now. A migration's first version does
  not know what the seed asked for; there, a seed asking for another subject
  than the one sent counts.

`params` also name the template (`key`, `template_id`), the versions involved
(`published_version`, `last_seed_version`, `operator_versions`, each a version
summary as `GET /v1/templates/{id}/versions` serves it) and the two subjects
(`subject` sent now, `requested_subject` asked for). A seed that would leave the
template as its published version already is overwrites nothing and is never
refused. A forced seed is written like any other; the operator's versions stay
in the history and can be restored as drafts, and its answer — the template —
carries `overrode: {rules, published_version, operator_versions}`, the same
shapes, saying what it wrote over. A forced seed that overrode nothing has no
`overrode`.

A refused seed records no version. The template keeps it as `seed_refusal`,
`{"refused_at", "rules", "payload_sha256"}`, served with the template:
`refused_at` is when that content was first refused (refused again, the time
stays; other content starts over), and `payload_sha256` tells the content
apart. The next seed that goes through — forced, or once nothing conflicts —
clears it.

## Required variables

A template's Required variables are the variables its HTML body must keep
referencing because the mail cannot do its job without them. They are kept on
the template row, not on its versions, so every version — a restored one
included — is checked against the sets the template has now. Both are served
with every template and kept sorted by name, byte by byte:

- `contract_required_variables` — `[{"name", "reason"}]`, the sending service's
  contract, declared in skymail-frontend beside the Template key and written
  only by the Template seed through `PUT /v1/templates/by-key/{key}`
  (`contract_required_variables`: entries, or names alone with a null reason;
  left out, the set stays as it is; `[]` clears it). The panel cannot change it.
- `operator_required_variables` — names operators mark and release:
  - `POST /v1/templates/{id}/required-variables` with `{"name"}` marks one the
    published body references. A name the contract holds changes nothing.
  - `DELETE /v1/templates/{id}/required-variables/{name}` releases one. A
    contract name is refused with `409 template.required_variable_in_contract`;
    a name that is not required changes nothing.

  Both need `skymail:templates:write`, answer with the template, record no
  version and leave `updated_at` alone. An archived template is `404`.

The two sets never share a name; a name the contract comes to declare leaves
the operators' set.

Every write of a version — the old panel's `POST /v1/templates` and
`PATCH /v1/templates/{id}`, the seed's upsert, saving a draft
(`POST /v1/templates/{id}/drafts`), restoring a version as one and publishing
a draft — is checked inside its transaction, against the version's own
subject, plain text and HTML and the template's sets as they stand at that
moment; marking a variable checks the published body. A refused write leaves
neither row nor version behind. The rules live in one place,
`internal/requiredvars`:

- `422 template.unparseable`, `params: {"part": "subject"|"plain_text"|"html",
  "error"}` — the mailer, which parses all three before every send, could not
  parse that part, so the version could not be sent at all.
- `422 template.required_variables_missing`, `params: {"missing": [{"name",
  "source": "contract"|"operator", "reason"}]}` — the body no longer references
  these Required variables. A reference is a field of the mailer's data used
  in an action: inside an `if` it counts; in a comment, an HTML comment, plain
  text, a `range` or `with` element's field, or `index . "X"` it does not
  (`internal/mailer/variables.go`, and the cases in
  `internal/mailer/testdata/referenced-variables.json`, which skymail-frontend
  holds a copy of).

## Mail approvals

A Mail onayı request (ADR-0031) is kept in `mail_approvals`, and everything
that happened to it in `mail_approval_events`. Neither is ever deleted or
rewritten: a request stays when it is sent, rejected, declined or expired, and
a resubmission is the same request with the events before it kept. The one
exception is Account erasure (ADR-0042, ADR-0051): it rewrites the erased
person's part of both, as [Account erasure](#account-erasure) says. The
`create_mail_approvals` migration's comment that events are never rewritten
predates it and is left as applied. The sends
an approval queues — a list's one, or one per person — are ordinary
`mail_tasks` rows, kept as every send is; `mail_approval_tasks` names them in
order, and the `approved` or `accepted` event names the first by `task_id`.

A request keeps what would be sent — the template, pinned to the version it
was submitted on, the audience (a mailing list, or 1..100 people in
`mail_approval_recipients`) and the variables — its state and its deadline:
seven days after it was last submitted, or after an approver returned it. A
request still pending or returned at its deadline is reported as expired at
once; the one-minute sweep records the expiry as an `expired` event with no
actor and mails the submitter. Reading or listing a request writes nothing.

A resubmission that changes who a request goes to physically deletes the
`mail_approval_recipients` rows of the people it no longer goes to. That is
ADR-0042's exception for relationship rows whose removal is itself the fact:
a row says only that the request goes to that person, and the change is
recorded, before and after, by the `resubmitted` event's `changes`, which is
never rewritten but by Account erasure.

## Personal data

These fields keep a person's identity with no end date, until Account erasure
takes the person out of them:

- `recipients.full_name` and `email`, and their `mailing_list_recipients` —
  the people on internal mailing lists.
- `mail_queue.recipient_full_name`, `recipient_email`, `recipient_sub` (the
  recipient's Keycloak subject, when the send knew it: a Keycloak group's
  member), and `error`, which can quote the address. The mail rendered for them — `subject`, `body`,
  `body_html` — is kept only for a while: see
  [Sent mail content](#sent-mail-content).
- `mail_tasks.sent_by` — who sent: the Keycloak subject of the token that
  queued the send — for an approved Mail onayı request, its submitter's — or
  `skymail` for the mail SkyMail sends of its own accord, the notice that a
  request expired.
- `mail_tasks.body_variables` — the values of the send, which may name or
  address people; for Keycloak's mails, the person's reset or verification
  link. Kept as the rendered mail is (below).
- `archived_by` on templates and mailing lists — who archived.
- `template_versions.author_sub` with `author_name` — who wrote a version, and
  the name their token carried then.
- `template_content_sensitive_events.actor_sub` with `actor_name` — who changed
  whether a template's sends show their content.
- `sent_content_views.actor_sub` with `actor_name` — who opened a sensitive
  send's masked content (see [Sent mail content](#sent-mail-content)).
- `mail_approvals.submitter_sub`, `submitter_name` and `submitter_email` — who
  submitted a request, and the name and verified address their token carried
  (`submitter_email_unverified` says the token carried an address Keycloak
  had not verified, which is not kept).
- `mail_approval_recipients.email` and `full_name` — the people a request
  goes to. A resubmission replaces them; its `resubmitted` event keeps who they
  were.
- `mail_approvals.body_variables` — the values of the send, which may name or
  address people.
- `mail_approval_events.actor_sub` and `actor_name` — who did each thing to a
  request — `note`, and `changes`, each variable's value, and who a request
  went to, before and after an edit or a resubmission.
- Open and click tracking (see [Open and click tracking](#open-and-click-tracking)):
  `mail_queue.tracking_token` (a random token that stands for the row's
  recipient in their mail's pixel and links), `tracking_skip_reason` (why
  they are not tracked: `preference_off` says they turned it off),
  `tracking_disabled_at`, `sent_at`; `mail_tracking_events` (when the
  recipient's mail was opened and which link they clicked, the client's
  class; no IP address, User-Agent or referrer); `mail_task_links.url`, which
  can name a person when a variable put them in a link of a send to them
  alone. `mail_task_tracking_totals` holds counts only, for sends with at
  least 10 tracked recipients.
- Bounces and the suppression list (see [Bounces and suppression](#bounces-and-suppression)):
  `mail_suppressions.address` and `recipient_sub` — an address a permanent
  failure was reported for, and whose it is when SkyMail knew. `mail_bounces`
  holds no address: it belongs to a queue row.

## Bounces and suppression

What a delivery status notification (an NDR, RFC 3464) or a relay's
permanent refusal said about a queue row (README, "Geri dönen posta ve
bastırma listesi"; hub spec skymail-bounce-isleme §5):

| What | Kept | Then |
|---|---|---|
| the report itself (addresses, the returned headers) | never in SkyMail: read in memory and dropped; in the sender mailbox, 30 days (Exchange retention, not SkyMail's) | — |
| `mail_bounces` (queue row, `source`, `kind`, `action`, enhanced code, SMTP code, Exchange's `AS(…)` token, `recipient_matched`, the SHA-256 of the report's id, `received_at`) — no address, no text | **one year** from `received_at` | deleted by the content sweep (every 5 minutes); with its queue row (`ON DELETE CASCADE`) |
| `mail_suppressions` (lower-cased address, `recipient_sub`, `source`, `reason_code` — codes only —, first and last seen, count, last queue row, `exempt_flagged_at`) | until an operator removes it or the person is erased: nothing removes it by itself (Yusuf, 2026-10-20) | Account erasure deletes the person's |

Logs carry the queue row's id, the outcome and the codes; never an address
or a report's text.

## Open and click tracking

Off unless `SKYMAIL_TRACKING=on` (hub ticket iletisim-servisi 14; design note
07; README, "Açılma ve tıklama takibi"). What it keeps and for how long:

| What | Kept | Then |
|---|---|---|
| `mail_queue.tracking_token`, `tracking_skip_reason`, `sent_at` | with the row | the token closes a year after `sent_at` (retention), and at once on withdrawal and Account erasure |
| `mail_tracking_events` | **one year** from the recipient's `sent_at` (`tracking.EventRetentionDays` = 365; Yusuf, 2026-10-19 — design note 07 said 90 days). Past it no event is written or shown | deleted by the retention run; withdrawal (core, my.) and Account erasure delete the person's at once |
| `mail_task_tracking_totals` | without end, sends with ≥10 tracked recipients only (T4); written by the retention run, from all the send's events, before the first of them goes | — |
| `mail_task_links.url` | one year from the link's `created_at` (T3) | emptied by the retention run (`url_cleared_reason` `expired`); a send masked or sensitive since it went out (review D7) and Account erasure of a person it names: emptied (`erased`) |
| `tracking_retention_runs` | without end: one row per run, counts and times only | — |

**Withdrawal** (`PUT /internal/v1/tracking-withdrawals/{sub}`, core, when
the person turns tracking off on my.): every token of mail to the subject
closes, sent or still queued, and their events are deleted, in one
transaction that locks their tracked queue rows first. Links stay and still
redirect; counts without people already kept stay. Idempotent, `204`.

**Retention** (`SKYMAIL_TRACKING_RETENTION` = `off` | `dry-run` | `apply` |
`purge`; README, "Saklama"): daily, under an advisory lock, one transaction
per send with the send's tracked queue rows locked first (then links, then
events — Account erasure's order). The dry run runs the same statements and
rolls them back; apply refuses a run past
`SKYMAIL_TRACKING_RETENTION_MAX_ROWS` (50,000 by default; the brake). The
≥10 threshold counts only delivered mail whose token is still open. Every run
writes its receipt to `tracking_retention_runs`. `purge`, the full
roll-back, deletes every event and closes every token (tracking must be
off).

The mail kept (`body_html`) never holds the pixel or a click address: they
are put in as the mail goes to the relay, so the content panel shows the
mail as rendered and looking at it is never an opening. Single mail, System
templates and sensitive templates are never tracked.

## Sent mail content

Every queue row holds its recipient's mail: `subject`, `body` (plain text)
and `body_html`. A send's page shows it, recipient by recipient, through
`GET /v1/mail_tasks/{id}/queue/{itemId}/content` (`Cache-Control: no-store`).
Yusuf, 2026-10-12: shown; 2026-10-13: the sensitive ones too, their codes and
one-time links masked. After the #63 review the orchestrator decided to mask
when the content is stored: a sensitive send's raw mail is not kept.

**Which mail is sensitive.** `templates.content_sensitive`: `true` for mail
that carries a code or a one-time link (Keycloak's verification, reset and
personal e-mail mails, the account center's mails, core's contact consent
confirmation, an event ticket's code), `false` for mail that may be shown as
sent, and `null`, the default, leaves it to the kind of send: a single mail
(`POST /mail_tasks/single`) is sensitive, a send to a list, a Keycloak group
or an approved Mail onayı request is not. The value is copied onto the send
when it is queued (`mail_tasks.content_sensitive_at_send`: `true`, `false`,
`unset`); a template whose key is locked (below) is copied `true` whatever
its row says (`20261020120000`). A send is sensitive if that copy **or** its
template now says so, or its template's key is locked now: changing a
template to `false` after a send never shows that send as sent, and neither
does taking the lock off its key. `mail_task_content_sensitive()` is the one
definition and reads the copy first; the lock on the key as it is now stays
behind it as a defence (the sends an image from before `20261020120000`
queues during a rollout copy the template's value alone). That migration
wrote `true` into the copy of every kept send sensitive at the time. The Template seed sets the flag (`content_sensitive` in
`PUT /v1/templates/by-key/{key}`), the panel either way
(`PUT /v1/templates/{id}/content-sensitive`, `skymail:templates:write`).

**Locked sensitive.** `keycloak.*`, `account.*`, `auth.*`, `login.*`,
`otp.*`, `core.contact-consent-confirm`, `event.ticket-created`,
`event.reminder`, and any key with a word such as `login`, `code`, `otp`,
`reset`, `verify`, `confirm`, `token`, `magic`, `invite`, `ticket`, `link`, or
Turkish `giris`, `kod`, `dogrulama`, `sifre`, `parola`, `baglanti`, `davet`,
`onay` between its dots and dashes (two words joined count too: `sign-in`).
Go's list is `internal/handlers/content_sensitivity.go`, the database's
`template_key_content_locked()`; `TestTheDatabaseLocksTheSameKeysAsGo` holds
them together. A locked template's key cannot be changed or removed
(`409 template.sensitive_key_immutable`); the panel's `false` is refused
(`409 template.content_sensitive_locked`), the seed's is written `true`.

**Who changed it.** Every change of `content_sensitive`, by the panel or the
seed, is a row of `template_content_sensitive_events`: `actor_kind`,
`actor_sub`, `actor_name`, `old_value`, `new_value`, `created_at`
(`GET /v1/templates/{id}/content-sensitive/events`). Account erasure replaces
the actor with Silinmiş kullanıcı.

**Masked when stored** (`internal/mailer/mask.go`, `mask_sweep.go`). Every
five minutes, in every replica, the content sweep takes the sends still to be
decided on (`mail_tasks.content_mask_due`, indexed) that are done (none of
their rows pending or processing, five minutes old), **one send a
transaction** (`FOR UPDATE SKIP LOCKED`). A send found not sensitive is not
looked at again until its template's `content_sensitive` or key changes (a
trigger makes its unmasked sends due again). A sensitive one has each row's
subject, plain text and HTML replaced with its masked copy, 500 rows at a
time, and its `body_variables` with their masked values
(`content_masked_at`, `content_masked_by`, `content_masked_variables`). The
rows are read `FOR UPDATE` and written only while they still hold a mail and
the send is not masked or emptied: a row Account erasure empties meanwhile is
never written back. A send whose masking fails (a panic
included) is put back: tried again after five minutes, doubled for each
failure (a day at most, `content_mask_retry_at`), behind the sends that never
failed, so broken sends at the head of the queue hold no other back. Its
failure waits (`content_mask_unconfirmed`) and is counted
(`content_mask_failures`) only when the same run masks some other send — the
fault was the send's — and only that run's failure: a run that masks none
counts none, as the fault may be SkyMail's, and its failures are never
counted later (they are dropped when the send's next failure is counted), so
a fault across SkyMail empties nothing once it is over. On
its third counted failure the send is emptied (`not_kept`), so its raw mail
is not kept. Counted or not, a sensitive send whose masking has kept failing
for **seven days** since its first failure (`content_mask_failing_since`,
cleared when it is masked) is emptied at the end of every run: a queue of
broken sends alone, whose failures no run counts, keeps no raw mail for the
retention period. Each send emptied for its failures writes an error line
`"event":"sensitive_content_mask_attention"` with `"reason":"failures"` or
`"deadline"` and the send's id, never its content; each failed try a warning
`"event":"sensitive_content_mask_failed"` (README, Gönderilen içerik, for the
count). A run stops after five sends fail one after another. A run masks at most 200 sensitive sends; sends found
not sensitive do not count (at most 5000 a run). The migration left due only
the kept sends that were sensitive then, so the first runs do not read the
history first. A template's `content_sensitive` or key change makes its
unmasked sends due again (`templates_mask_due`, `idx_mail_tasks_template`),
waiting for a sweep that is deciding on one of them. Every code, one-time link and IP address becomes
`••••••`; in the HTML's text `<mark data-skymail-masked="">••••••</mark>`.
Nothing is masked on reading. Two ways, the first when it can be proved:

1. *Variables.* The send's variables that carry a code, a link, an IP address
   or a place are masked — first those its template lists
   (`templates.sensitive_variables`, set by the Template seed's
   `sensitive_variables`), each whole with everything under it, whatever its
   name or shape; then, listed or not, by name (the last word: `link`, `url`,
   `code`, `otp`, `pin`, `token`, `password`, `secret`, `verifier`, `invite`,
   `key`, `verification`, `passphrase`, `kod`, `bağlantı`, `şifre`, `parola`,
   `davet`, `doğrulama`…; `IpAddress`, `ClientIP`,
   `Location`, `Konum`; but `subjectKey`, and a number under `…Minutes`,
   `…Seconds`, `…Hours`, `…Days`, `…Count`, stay), by shape (an address with
   or without a scheme, an IPv4 or IPv6 address with or without a port, four
   or more digits, a code such as `QR-88K2` or `x7Gk2pQ9`, a token of 20 or
   more characters) and inside text (each such piece on its own; a date or a
   time stays). The mail is rendered again with **the template as it stood
   when the send was queued** only: the row, if it has not been written
   since, and the newest version published by then. A version written after
   the send is never used: its render could depend on the hidden value. The
   render is used only if the unmasked variables render exactly the stored
   mail.
2. *Patterns,* when it does not, or the variables are gone. The known values
   are replaced in the stored mail in every form the renderers print them
   (`&amp;`, `&#43;`, URL escapes) — in the subject, the plain text, and the
   HTML's text, comments, attribute values and style sheets' quoted strings,
   never in its tags, layout attributes or the rest of its style sheets (a
   known `482` leaves `color:#482c8f`), and one of fewer than four characters
   only where no letter or digit touches it — then every address, IP address, run of
   four or more digits, code cut by spaces (`483 920`), code- or token-shaped
   word is masked in the subject, the plain text, and the HTML's text,
   comments and attributes (`data-*`, `id`, `class` and `name` too: a
   template may write a value into one, `id="c-{{.code}}"`; a class with no
   code in it stays); every address in an attribute is
   masked whole (relative ones too; a `mailto:`, a `tel:` and a `#` link
   stay); a style sheet's strings and long numbers are masked, its colours
   and sizes kept. It masks more than it must.

A last check, both ways: every credential the variables held (a code, a
token, an IP address, a link that carries a query or a code-shaped path, and
each of its query values, raw and decoded) is looked for in every escaped
form, letter case aside, and a number one with its digits cut apart; and any
code-shaped string the mail shows must be one the template writes itself,
the recipient's, or a value left as it is. One found: the send is not kept —
its content is emptied (`content_cleared_at`) and reads `not_kept`.

**Reading.** In this order: who asks (`restricted`), still kept (`expired`),
emptied (`not_kept` if sensitive, else `expired`), then masked (`masked`, with
`masked_variables` and `masked_by`), sensitive but not masked yet (`pending`:
nothing shown until the sweep has masked it), or as sent (`available`). A
send's `body_variables` are listed only when `available`.

**Who opened it.** Every opening of a recipient's masked mail is a row of
`sent_content_views`: `task_id`, `queue_item_id`, `actor_sub`, `actor_name`
(from the token), `masked_by`, `masked_variables`, `created_at`. No content
is returned unless the row is written. It names no recipient; Account erasure
replaces the actor; it is deleted after `SENT_CONTENT_VIEWS_RETENTION_DAYS`
(two years by default, below). No API reads it yet (hub ticket
`.scratch/skymail-frontend-rewrite/issues/32-maskeli-icerik-acilis-kaydi-okuma.md`).

**Who sees it.** The route takes `skymail:mails:read`; the handler then shows
the content to the send's sender, for a send to many to holders of
`skymail:mails:write`, and to holders of `skymail:system-mails:read`, the
administrator's role. Anyone else gets `access: restricted`, sensitive or
not.

**How long.** The same sweep, after masking, takes the sends none of whose
rows is still to go out and empties each `MAIL_CONTENT_RETENTION_DAYS` after
it was created, sensitive or not (unset: 3650, the most it may be): its rows'
`subject` and `body` become `''` and `body_html` `NULL`, its `body_variables`
`{}`, and `mail_tasks.content_cleared_at` says when. Status, recipient, error
and the counts stay. Past the period the API answers `expired` even before
the sweep has run. It runs while the sender is paused too.

**Not kept: before 2026-10-13.** From #61's release (2026-10-13 11:16) until
sensitive content was kept masked, the sweep emptied every sensitive send
five minutes after it went out: on its first run every one there was, the
sends from before #61 among them (Keycloak's mails, Forms' single mails,
core's consent confirmations). Only the nightly dump still holds them. The
API answers `not_kept` for such a send.

**Rolling back.** An image from before this one (#61) empties every
sensitive send on its first sweep, masked or not: safe, and for good. Its
sends are queued with `content_sensitive_at_send = unset` (the column's
default). Do not run the down migration of `20261013140000`: it drops
`sent_content_views`, the record of who opened what.

The period is Yusuf's decision (2026-10-12: the longest allowed). KVKK's
storage limitation principle (Law 6698, art. 4(2)(d): kept only as long as
needed for its purpose) asks that it be reviewed: the content names and
addresses people, and a shorter period is set with the variable alone.
`sent_content_views` has its own period: `SENT_CONTENT_VIEWS_RETENTION_DAYS`
(below, under Where the raw mail still is).

The sweep covers what a send keeps, not a Mail onayı request:
`mail_approvals.body_variables` stays as the request was (see Mail approvals)
until Account erasure.

**Account erasure** empties the person's rows and the rows that name them
(below); the content route then answers `unavailable` for them. A restore
brings content back with the dump; the sweep masks or empties it again on its
next run.

### Where the raw mail still is

Masking replaces a sensitive send's raw mail in the live rows. The raw code
or link is still, for a while, in these places (#63 review, R5):

- **Until the send is done, and a run after.** The queue needs the mail to
  send it. The sweep takes a send five minutes after it is queued and once
  none of its rows is `pending` or `processing`, so the raw mail lives as long
  as the send is going out, plus up to one interval (five minutes) and the
  run. A paused sender (`MAIL_SENDER=paused`) or rows waiting for a retry keep
  it longer. A send whose masking fails keeps it until it is masked, three
  counted failures, or seven days after its first failure. All that time the
  API answers `pending` and shows nothing.
- **The nightly dump.** The production backup dumps the database every night
  (encrypted, off the server; object lock 30 days, deleted on day 31: hub
  ticket account-erasure 19). A dump taken before a send was masked holds its
  raw mail until the dump is deleted. A restored dump comes back with
  `content_masked_at` empty for those sends, so the sweep masks them again on
  its first run. Manual dumps (`skymail-production-dump-wizard.sh`) are kept at
  most `PERIODIC_DESTRUCTION_INTERVAL` (90 days).
- **PostgreSQL's dead row versions.** An `UPDATE` writes a new row version;
  the old one, with the raw mail, stays in the table's pages until `VACUUM`
  removes it and the space is reused. No query, the API and `pg_dump`
  included, reads a dead version: it is there only for whoever can read the
  data files, who can read the live database too. Autovacuum takes a table
  once about a fifth of its rows are dead (PostgreSQL's default
  `autovacuum_vacuum_scale_factor`, 0.2), which on `mail_queue`, mostly
  appended to, may be days or longer. So routine masking needs nothing more.
  After a large backlog is masked or emptied at once (a first release, a
  restore, a long sender pause), run on the database, once the sweep is
  through: `VACUUM (VERBOSE) mail_queue; VACUUM (VERBOSE) mail_tasks;`. Plain
  `VACUUM` takes no lock that stops reads or writes; `VACUUM FULL` is not
  needed and locks the tables.
- **The WAL.** Every write, the raw mail with it, is in the write-ahead log
  until its segment is removed or recycled after a later checkpoint (every
  five minutes on PostgreSQL's defaults), unless a replication slot or
  `wal_keep_size` holds it. WAL archiving is off in production, so no
  point-in-time recovery archive holds it (hub ticket account-erasure 19).
  Turning archiving on would keep raw mail in the archive as long as the
  archive; this page must then say how long.
- **`mail_approvals.body_variables`.** A Mail onayı request keeps the values it
  was submitted with until Account erasure (see Mail approvals); the sweep
  does not mask it. Approved sends are to many, sensitive only when their
  template says so.
- Not kept elsewhere: `mail_queue.error` holds the SMTP or render error, not
  the mail; the logs carry ids and counts, never a subject or a body.

**`sent_content_views` period (Yusuf, 2026-10-14: two years).** A record of
who opened a masked mail is deleted `SENT_CONTENT_VIEWS_RETENTION_DAYS` after
`created_at` (unset: 730, two years; a whole number from 1 to 3650, anything
else stops startup), by the same sweep, after the content, 500 rows a
statement.
The rows are an access log of who opened a person's masked mail, there to
answer "who saw my mail" and to review an operator's use. KVKK fixes no
period for such a log; it asks for one tied to its purpose (art. 4(2)(d)).
Two years covers a full board term and the one after it: a question about
how the previous board used the panel can still be answered after the
handover. What matches Microsoft 365: its audit log, which records who
opened a mailbox item (`MailItemsAccessed`), is kept for a fixed period
separate from the mail's own retention — 180 days on Audit (Standard), one
year on Audit (Premium), up to ten years with an add-on. Where it deviates:
two years rather than 180 days or one year, for the yearly board term; and
the period is SkyMail's one setting, not a per-plan default. Should the
content's period be set shorter than two years, a view row outlives the mail
it names; it holds no content.

## Account erasure

`PUT /internal/v1/account-erasures/{request_id}` is core's Erasure command for
SkyMail (ADR-0051). Core's `docs/account-erasure-command.md` holds the
contract; this section is SkyMail's part of it.

- **Reach.** Internal Docker network only (ADR-0016). A request carrying a
  forwarding header Traefik adds (`X-Forwarded-*`, `Forwarded`, `X-Real-Ip`)
  gets a bare `404`, so the caller must not set one. The route is outside
  `/v1` and does not go through `userinfo`.
- **Token.** Checked locally against the realm's JWKS: signature (RS, PS or
  ES; never HMAC or unsigned), `iss` exactly `KEYCLOAK_REALM_URL`, `exp`
  required. Then `azp` = `core-erasure`, `aud` ∋ `skymail`, and the role
  `skymail:account:erase` under `resource_access.skymail.roles` — roles on any
  other client do not count. The caller's own access marker is checked as on
  every route.
- **Subject must be blocked.** The subject's marker must already be in
  account-access Redis, read with the same `skymail-reader` ACL as the gate.
  Missing: `409 subject_not_blocked`. Redis unreachable: `503
  subject_block_unverifiable`. With `ACCOUNT_ACCESS_GATE_MODE=off`, as in
  sandbox, the block cannot be read and the answer is `503` too, never `409`.
- **Body.** `{"request_id", "subject_id", "emails"}`, nothing else, each once,
  at most 4 KB. `subject_id` is a canonical lower-case UUID and not
  Silinmiş kullanıcı; `emails` holds 0..3 plain addresses of at most 254
  characters. Anything else is `400 invalid_erasure_command`.

What it erases, with S the subject, E the addresses (compared whole,
case-insensitively) and N the full names SkyMail holds on S's actor rows and
the recipient rows of E (and of S, in the queue). Free text is searched for E and N, case-insensitively; a
single-word name is never searched for, and a namesake's text that contains
the same full name is cleared too — the accepted cost. A field that names the
person is cleared whole, never masked.

| Where | Which rows | What happens |
|---|---|---|
| `recipients` | `email` in E | deleted, with their `mailing_list_recipients` |
| `mail_queue`, `pending` | `recipient_email` in E or `recipient_sub` = S | deleted: never sent |
| `mail_queue`, `processing` | `recipient_email` in E or `recipient_sub` = S | waited for (`202`) |
| `mail_queue`, `sent`/`failed` | `recipient_email` in E or `recipient_sub` = S | `recipient_full_name` Silinmiş kullanıcı, `recipient_email`, `subject`, `body` `''`, `body_html`, `error`, `recipient_sub` NULL; the row stays for the counts |
| `mail_queue`, anyone else's, `sent`/`failed` | `subject`, `body` or `body_html` names E or N | those three cleared (`''`, `''`, NULL) |
| `mail_queue`, anyone else's, `pending`/`processing` | the same | waited for (`202`): it goes out as it was rendered, then it is cleared |
| `mail_tasks` | `sent_by` = S | Silinmiş kullanıcı |
| `mail_tasks.body_variables` | names E or N, or every row of the send is to E | `{}` — a send to the person alone, Keycloak's reset and verification mails among them |
| `mail_approvals` | `submitter_sub` = S | sub and name Silinmiş kullanıcı, `submitter_email` NULL, `submitter_email_unverified` false |
| `mail_approvals.body_variables` | names E or N | `{}` |
| `mail_approval_recipients` | `email` in E | deleted; see below |
| `mail_approval_events` | `actor_sub` = S | `actor_sub` and `actor_name` Silinmiş kullanıcı |
| `mail_approval_events.note` | actor S, or names E or N | NULL; `[silindi]` on a `rejected` event, whose reason cannot be empty |
| `mail_approval_events.changes` | names E or N — either shape, `{recipient_email, recipient_full_name}` from before requests went to several people or `{recipients: […]}` | NULL |
| `template_versions` | `author_sub` = S | `author_sub` and `author_name` Silinmiş kullanıcı |
| `template_content_sensitive_events` | `actor_sub` = S | `actor_sub` and `actor_name` Silinmiş kullanıcı |
| `templates`, `mailing_lists` | `archived_by` = S | Silinmiş kullanıcı |
| `mail_tracking_events` | of a `mail_queue` row to the person (address in E or `recipient_sub` = S) | deleted |
| `mail_queue.tracking_disabled_at` | a tracked row to the person | set: an opening or a click of that mail records nothing from now on; its links still redirect |
| `mail_task_links.url` | names E or N, as written or URL-decoded, or every row of the send is to the person | NULL, `url_cleared_reason` `erased`: the link says it was removed |
| `mail_suppressions` | `address` in E, `recipient_sub` = S, or its last queue row is to the person | deleted, before that row's address is cleared |
| `mail_bounces` | of a `mail_queue` row to the person | kept with the cleared row: it holds no address, codes only |

Silinmiş kullanıcı is the subject `00000000-0000-4000-8000-000000000000`
with the name `Silinmiş kullanıcı`, the same for everyone. A request's person
it stands in for has the address `silinmis-kullanici@invalid`.

A Mail onayı request keeps its constraints. When the person was the last one a
request went to, one Silinmiş kullanıcı takes their place, so the request
still goes to a list or at least one person. When a send was queued to the
person, Silinmiş kullanıcı takes their position, so `task_ids[i]` stays the
send to `recipients[i]`; an address is in a request once, so if a second of
the person's addresses had a send there, that row goes with its
`mail_approval_tasks` link and the send itself stays in `mail_tasks`.

Template and version content, list names and the club's mail history as a
count stay (ADR-0051: editorial record). Pending Mail onayı requests are not
cancelled; they expire in seven days. The person's addresses leave the
suppression list: mail to them is sent again if they are added to a list
again. A report about their old mail that arrives later finds its row cleared
and suppresses nothing (it locks the row first: README, "Geri dönen posta ve
bastırma listesi").

- **Order.** One transaction under an advisory lock on `request_id`. The steps
  that leave the names findable run first. If mail must be waited for, they
  are committed and the answer is `202` with `Retry-After: 30`; the same
  `PUT` later finishes, the rows keyed by name last. A pending row to the
  person is deleted in the first call. The names are read again on every call
  from the rows that still hold them, so a name only a deleted pending row
  held is not searched for in a later call — a mail in flight would have to
  name the person by that name alone.
- **Answer.** `200 {"request_id","status":"completed","completed_at","counts"}`
  with `counts` = `recipients_deleted`, `list_memberships_deleted`,
  `queue_rows_deleted`, `queue_rows_cleared`, `bodies_cleared`,
  `variables_cleared`, `actor_columns_replaced`, `notes_cleared`,
  `changes_cleared`, `approval_recipients_removed`,
  `approval_recipient_placeholders`, `approval_send_links_deleted`,
  `tracking_events_deleted`, `tracking_links_cleared`,
  `suppressions_deleted`. The counts
  are what the completing call did; work committed by an earlier `202` call
  is not in them. A repeat, or a concurrent duplicate, returns the stored
  body without doing the work again. Other answers: `202` (above),
  `400 invalid_erasure_command`, `401 erasure_unauthorized`,
  `403 erasure_forbidden`, `404` (ingress), `409 subject_not_blocked`, `503`
  with `Retry-After` (`subject_block_unverifiable`, `access_gate_unavailable`,
  `token_keys_unavailable`, `erasure_store_unavailable`). Errors are RFC 7807
  `application/problem+json` with a fixed `code` and no value from the
  request.
- **Logs.** The body is never logged. Log lines carry the request id and a
  fixed code, a Postgres failure its SQLSTATE only; never the subject, an
  address or a name.
- **Proof.** `account_erasure_receipts` (`request_id`, `completed_at`,
  `counts`) holds no subject and no address. It is the deletion record and is
  kept at least three years; no code path deletes it.

## Backup and restore

A dump taken before an Account erasure brings the erased person's rows back
on restore. That includes the mail queue: mail still `pending` or `processing`
in the dump would go out to the person, and another person's queued mail
could still name them. So SkyMail restores with its sender paused, closes the
queue the dump brought back without sending it, and only then lets core
replay its erasures (account erasure spec §8) and turns the sender back on.

`MAIL_SENDER` is `on` (the default, also when unset) or `paused`. Any other
value stops startup. It is a plain environment value, not a secret. It is not a
database flag, because a restored dump would bring back the flag's old value
too. Scaling SkyMail to zero is no substitute: the replay goes to SkyMail's
API.

Paused, SkyMail:

- still puts back to `pending` the `processing` rows whose queue lease has
  run out (`MAIL_QUEUE_LEASE`, 10 minutes by default; README), at startup and
  every fifth of the lease after, as the sender always does. A row within its
  lease is left alone: under Dokploy's start-first deploy the old task is
  still sending what it took. A `processing` row without `claimed_at` (taken
  by an image from before the lease, or from a dump made before it) gets its
  lease started when it is first seen and goes back one lease later;
- starts no dispatcher and no workers, so nothing is sent. Mail sent through
  the API, Keycloak's reset and verification mails among it, is queued and
  stays `pending`;
- logs a warning at startup that names `MAIL_SENDER`. When on, the mailer's
  `Starting up` line carries `"sender":"on"`;
- serves the API, `/ready` and the erase endpoint as usual.
  `GET /v1/mail_tasks/summary` answers `sender_paused: true`.

Pausing alone does not protect the people erased since the dump. Core keeps no
address once an erasure is done. The replay reads the addresses back from the
matching core and Keycloak backups (step 5), but an address the person changed
between two backups is still missed. The person's queued mail to an address the
replay does not have would stay `pending` and go out when the sender comes
back on. Another person's queued mail that names them would keep the replay at
`202`, because only a sent or failed mail's body is cleared. Core cannot say
who the mail was for, so the restored queue is closed by time, not by person
(account erasure ticket 16).

A SkyMail dump can be restored only together with a core and a Keycloak dump
taken at the same time: without them the replay has no addresses. The SKY LAB
workspace's `ops/wizards/skymail-production-dump-wizard.sh` takes all three in
one run.

### The procedure

1. **Pause.** In Dokploy, set `MAIL_SENDER=paused` on SkyMail and deploy.
   Check that `GET /v1/mail_tasks/summary` answers `sender_paused: true`. The
   container that is running while the dump lands must already be paused:
   an unpaused dispatcher looks at the queue every 10 seconds and would send
   the restored queue as soon as it is there.
2. **Restore.** Just before `pg_restore` starts, write down the time in UTC
   on the host: `date -u +%Y-%m-%dT%H:%M:%SZ`. That is the restore instant,
   `--before` in step 4. Then `pg_restore` the dump.
3. **Deploy.** SkyMail starts on the restored database, still paused: any
   pending migrations run, and rows the dump held as `processing` go back to
   `pending` once their lease has run out (at once for a dump older than the
   lease; one lease after startup for a row the dump holds without
   `claimed_at`). Step 4 closes them either way. A redeploy in between, by the
   secret rotator for example, stays paused too, since the environment has
   not changed.
4. **Close the restored queue.** On the Dokploy host, run the command in
   SkyMail's own container (with `sudo` if your user cannot run `docker`), a
   dry run first and then with `--apply`. Set `APP` to the SkyMail backend's
   App Name in Dokploy, not its display name. Production and sandbox run on
   the same host, so check that `echo` prints the container being restored.

   ```sh
   C=$(docker ps --format '{{.Names}}' | grep "^${APP:?App Name}" | head -n 1); echo "$C"
   docker exec "$C" /app/skymail-backend queue-close-restored --before 2026-09-26T09:00:00Z
   docker exec "$C" /app/skymail-backend queue-close-restored --before 2026-09-26T09:00:00Z --apply
   ```

   See below for what it prints.
5. **Replay.** Core replays its erasures (spec §8, ADR-0053) with its own
   tool, `core-backend replay-from-backup` (core #138): every request
   completed after the dump was taken. Core keeps no address of an erased
   person, so the tool reads each person's addresses from the matching core
   and Keycloak backups, loaded into temporary Postgres databases with no
   network and named by `CORE_SNAPSHOT_DATABASE_URL` and
   `KEYCLOAK_SNAPSHOT_DATABASE_URL`. It sends the normal Erasure command with
   those addresses. Run it on core's side, a dry run first and then with
   `--apply`:

   ```sh
   core-backend replay-from-backup --service skymail --dumped-at <T>
   core-backend replay-from-backup --service skymail --dumped-at <T> --apply
   ```

   `<T>` is when the SkyMail dump was taken: `start_utc` of `skymail.dump` in
   the dump folder's `MANIFEST.txt`. It is not the restore instant from
   step 2. Expect `200` with a receipt for each request.

   **Do not send a replay with `emails: []` first.** SkyMail answers a
   repeated `request_id` with the receipt it recorded. An `emails: []` replay
   writes that receipt after erasing only subject-keyed rows, and
   `replay-from-backup` then gets the receipt back, exits 0 and never erases
   the e-mail-keyed rows (core #142). Use `emails: []` afterwards, and only
   for requests `replay-from-backup` reported as FAIL because it found no
   subject in the backups.
6. **Unpause.** Check that `GET /v1/mail_tasks?status=sending` lists no send
   created before the restore instant. Then remove `MAIL_SENDER` and
   redeploy. Only the mail queued since the restore is sent.

### `queue-close-restored`

`skymail-backend queue-close-restored --before <RFC3339> [--apply]` is a
subcommand of the server's binary. It has no HTTP endpoint. It reads the
environment the server reads (`DATABASE_URL`, `MAIL_SENDER`), which is why it
runs through `docker exec` in SkyMail's container.

- `--before` is required: an RFC3339 time with its offset
  (`2026-09-26T09:00:00Z` or `2026-09-26T12:00:00+03:00`). A time in the future
  is refused. The rows it acts on are the `pending` and `processing` rows
  queued before that time. A row without `created_at` counts as one of them:
  SkyMail stamps every row it queues, so only a dump can hold such a row. Rows
  queued at or after `--before`, and every `sent` or `failed` row, are never
  touched.
- Without `--apply` it only counts, and changes nothing. It runs whatever
  `MAIL_SENDER` says, and notes when the sender is not paused.
- `--apply` refuses unless `MAIL_SENDER=paused` in the container, so no worker
  runs beside it. In one transaction it sets those rows to `failed` with the
  error `restore: gönderilmedi`. `attempts` stays as it was, since nothing
  tried them. It prints the same counts for what it closed. Run again, it
  finds `0`.
- It prints counts and times only, never an address, a name or a mail. It
  exits `0` when done, `1` when refused or when the database fails (an apply
  that fails changes nothing), and `2` for a wrong argument or a missing
  `DATABASE_URL`.

A dry run prints:

```text
queue-close-restored: dry run, nothing changed. Add --apply to close these rows.
before: 2026-09-26T09:00:00Z
pending: 41
processing: 0
tasks: 3
oldest created_at: 2026-09-24T18:02:11Z
newest created_at: 2026-09-25T23:40:05Z
queued at or after --before, left alone: 2
```

- `pending`, `processing`: the rows the dump held that were still to go out.
  After step 3's deploy, `processing` is normally `0`.
- `tasks`: the sends those rows belong to.
- `oldest created_at`, `newest created_at`: when they were queued, in UTC, or
  `-` when there are none. The newest should be no later than the dump. If it
  is later, `--before` is past the restore instant and would close mail queued
  since: correct it before `--apply`.
- `without created_at, counted in: N`: printed only when there are such rows.
- `queued at or after --before, left alone`: mail queued since the restore.
  It goes out at step 6.
- `note: …`: printed when `MAIL_SENDER` is not paused. `--apply` would refuse.

`--apply` prints the same lines after
`queue-close-restored: closed without sending, error "restore: gönderilmedi".`

### Closed mail afterwards

Each closed send shows as failed (`Başarısız`) in the send list, on the home
screen and in `GET /v1/mail_tasks/summary`. `mail_task_status()` makes a send
failed as soon as one of its rows is (a row skipped for a reserved domain,
`reserved_domain: …`, aside). On a send's page, each closed recipient
is failed with the error `restore: gönderilmedi`. A list send that was partly
out when the dump was taken shows its sent count beside it.

A failed row is final. SkyMail has no retry for it: neither the API nor the
panel sends a failed row again. To send closed mail again, make a new send:

- to a person: the send form, or `POST /v1/mail_tasks/single`, with the same
  template and values. The address is on the send's page
  (`GET /v1/mail_tasks/{id}/queue?status=failed`);
- to a list: `POST /v1/mail_tasks`. It goes to everyone on the list, including
  those whose mail had gone out before the dump.

Keycloak's reset and verification mails are not sent again: their links are
stale by then, and the person asks again.

Send closed mail again only when you know who it is for. Until the replay has
run, and for an address it missed, the restored database still holds the
addresses of people erased after the dump was taken: their recipients, their
list memberships and the closed rows to them. The closed rows do not show
whose erasure they belong to. A Keycloak group is safe to send to again,
because its members are read from Keycloak at send time and an erased person
is no longer there. A SkyMail list restored from the dump can still hold an
erased person. For a person, send again only to someone whose account still
exists or who asked for it.

A command that carries the addresses (a replay that read them from the
matching backups, or a request core had not completed when the dump was taken)
finds the person's mail by address as well. It clears the person's closed mail
to them and deletes any mail queued to them since the restore.

## Retention boundaries

Archiving a template or list preserves `mail_tasks`, `mail_queue`, and mailing
list recipient associations. Historical task reads continue to show the source
template/list name. Archived templates and lists cannot be used to create new
mail tasks.

Removing one recipient from a mailing list remains a physical deletion of the
`mailing_list_recipients` relationship row. Queue residue and other transient
worker data remain outside this durable-record lifecycle.
