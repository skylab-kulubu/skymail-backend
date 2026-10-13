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
- `mail_queue.recipient_full_name`, `recipient_email`, and `error`, which can
  quote the address. The mail rendered for them — `subject`, `body`,
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

## Sent mail content

Every queue row holds its recipient's mail as it was rendered and sent:
`subject`, `body` (plain text) and `body_html`. A send's page shows it,
recipient by recipient, through
`GET /v1/mail_tasks/{id}/queue/{itemId}/content` (Yusuf, 2026-10-12: shown;
2026-10-13: the sensitive ones too, their codes and one-time links masked).

**Sensitive mail is shown masked.** `templates.content_sensitive` says which
mail is sensitive:
`true` for mail that carries a code or a one-time link (Keycloak's
verification, reset and personal e-mail mails, the account center's mails,
core's contact consent confirmation, an event ticket's code), `false` for
mail that may be shown, and
`null`, the default, leaves it to the kind of send: a single mail
(`POST /mail_tasks/single`: what a service sends one person, a code among it)
is hidden, a send to a list, a Keycloak group or an approved Mail onayı
request is shown. `mail_task_bulk()` and `mail_task_content_sensitive()` are
the one definition. A new template is therefore masked on the single path
until its seed or an operator says `false`. The Template seed sets the flag
(`content_sensitive` in `PUT /v1/templates/by-key/{key}`; left out, it stays),
and the panel sets it either way (`PUT /v1/templates/{id}/content-sensitive`,
`skymail:templates:write`). The migration marked the credential-carrying
templates `true` and the ones the seed shows (`free.basic`, `core.welcome`,
`core.certificate`, `club.team-membership`, `event.updated`,
`event.cancelled`, `mail.approval-*`, `ops.send-failed`) `false`, so the
first sweep does not empty their single mails before the next seed.

**Locked sensitive.** `keycloak.*`, `account.*`,
`core.contact-consent-confirm`, `event.ticket-created` and `event.reminder`
carry a credential: a login or verification code, a reset or account link, a
ticket's code. So does any key under `auth.`, `login.` or `otp.`, or with a
word such as `login`, `code`, `otp`, `reset`, `verify`, `confirm`, `token`,
`magic`, `invite`, `ticket` or `link` between its dots and dashes (two words
joined count too: `sign-in`, `log-in`), for the sign-in flows that may move to
SkyMail (YıldızPlace mails its login link over its own SMTP today). The key of
a locked template cannot be changed or removed (`PATCH` answers
`409 template.sensitive_key_immutable`), as a system template's cannot: the
lock goes with the key, and renaming would open its past sends. The lock is
read from the template's key when content is asked for, not fixed on the send
(a follow-up ticket fixes it at send time). Whoever reads system mail would otherwise read everyone's.
They are sensitive whatever their row says; the panel's `false` is refused
with `409 template.content_sensitive_locked`, the seed's is written as `true`,
and a create or edit that gives a template such a key marks it `true`. The
list is `internal/handlers/content_sensitivity.go`.

**How it is masked** (`internal/mailer/mask.go`; `access: masked`). The
whole mail is shown, and every code and one-time link in it is replaced by
`••••••` on the server: the unmasked value is never sent to a client. Two
ways, the first when it can be proved:

1. *Variables.* The send's variables that carry a code or a link — by name
   (`link`, `code`, `CancelUrl`, `TicketCode`, `token`, `password`…, the last
   word of the name: `linkExpirationMinutes` is not one) or by shape (an
   address, 4 to 10 digits, a code of capitals and digits, a token of 20 or
   more characters; an address inside text is masked on its own), and a
   person's IP address and whereabouts (`IpAddress`, `ClientIP`, `Location`,
   any IPv4 or IPv6 address, also inside text: a raw IP address is shown
   nowhere) — are
   masked, and the mail is rendered again. Only a template that renders the
   unmasked variables into exactly the stored mail is used: the template as
   it is, or one of its last 50 published versions. So the masked mail is the
   mail that was sent, and a value is masked wherever the template put it —
   text, `href`, subject, plain text, cut up or escaped.
2. *Patterns,* when no template renders the stored mail (the template
   changed outside its versions, Account erasure emptied the variables). The
   known values are replaced in the stored mail in every form the renderers
   print them, then every address, every IP address, every run of four or more digits (a date
   or a time with separators stays) and every code- or token-shaped word is
   masked in the subject, the plain text, and the HTML's text, comments and
   attributes. It masks more than it must.

Either way a last check looks for each credential the variables held (a code,
a token, a link that carries a query or a long path segment, and each such
query value) in every escaped form, letter case aside. One found, the answer
is `access: sensitive` with nothing in it, and a warning is logged. In the
HTML a masked value is `<mark data-skymail-masked="">••••••</mark>`.
`masked_variables` and `masked_by` (`variables` or `patterns`) say what and
how. A send's `body_variables` are listed only when its content is
`available`, never masked.

**Who opened it.** Every opening of a recipient's masked mail is a row of
`sent_content_views`: `task_id`, `queue_item_id`, `actor_sub`, `actor_name`
(from the token), `masked_by`, `masked_variables`, `created_at`. No content
is shown unless the row is written. It names no recipient; Account erasure
replaces the actor with Silinmiş kullanıcı. There is no API to read it yet:
`SELECT … FROM sent_content_views WHERE task_id = …`.

**Who changed it.** Every change of `content_sensitive`, by the panel or the
seed, is a row of `template_content_sensitive_events`: `actor_kind`,
`actor_sub`, `actor_name`, `old_value`, `new_value`, `created_at`
(`GET /v1/templates/{id}/content-sensitive/events`). The migration's own
marks are not rows there. The rows are never rewritten but by Account
erasure, which replaces the actor with Silinmiş kullanıcı.

**Who sees it.** The route takes `skymail:mails:read`;
the handler then shows the content to the send's sender, for a send to many
to holders of `skymail:mails:write`, and to holders of
`skymail:system-mails:read`, the administrator's role. Anyone else gets
`access: restricted`, sensitive or not. A send's `body_variables` in the send
list and on its page follow the same rule (`content_access`).

**How long.** A sweep runs at startup and every five minutes, in every
replica (`FOR UPDATE SKIP LOCKED`, 20 sends a statement). It takes the sends
none of whose rows is still pending or processing and that are at least five
minutes old, and empties each `MAIL_CONTENT_RETENTION_DAYS` after it was
created, sensitive or not (unset: 3650, the most it may be): its rows'
`subject` and `body` become
`''` and `body_html` `NULL`, as Account erasure leaves a row, its
`body_variables` `{}`, and `mail_tasks.content_cleared_at` says when. Status,
recipient, error and the counts stay. Past the period the API answers
`expired` even before the sweep has run. A sweep runs while the sender is
paused too: it never touches mail still to go out.

**Sensitive content emptied before 2026-10-13 does not come back.** From
#61's release (2026-10-13 11:16) until sensitive content was kept, the sweep
emptied every sensitive send five minutes after it went out: on its first run
every one there was, the sends from before #61 among them (Keycloak's mails,
Forms' single mails, core's consent confirmations). Only the nightly dump
still holds them. The API answers `access: not_kept` for such a send (emptied
within the retention period, and sensitive), not `sensitive` or `expired`.

The period is Yusuf's decision (2026-10-12: the longest allowed). KVKK's
storage limitation principle (Law 6698, art. 4(2)(d): kept only as long as
needed for its purpose) asks that it be reviewed: the content names and
addresses people, and a shorter period is set with the variable alone.

The sweep covers what a send keeps, not a Mail onayı request:
`mail_approvals.body_variables` stays as the request was (see Mail approvals)
until Account erasure.

**Account erasure** empties the person's rows and the rows that name them
(below); the content route then answers `unavailable` for them. A restore
brings content back with the dump; the sweep empties it again on its next run.

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
E's recipient rows. Free text is searched for E and N, case-insensitively; a
single-word name is never searched for, and a namesake's text that contains
the same full name is cleared too — the accepted cost. A field that names the
person is cleared whole, never masked.

| Where | Which rows | What happens |
|---|---|---|
| `recipients` | `email` in E | deleted, with their `mailing_list_recipients` |
| `mail_queue`, `pending` | `recipient_email` in E | deleted: never sent |
| `mail_queue`, `processing` | `recipient_email` in E | waited for (`202`) |
| `mail_queue`, `sent`/`failed` | `recipient_email` in E | `recipient_full_name` Silinmiş kullanıcı, `recipient_email`, `subject`, `body` `''`, `body_html`, `error` NULL; the row stays for the counts |
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
cancelled; they expire in seven days. There is no suppression list: the
address can be added to a list again.

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
  `approval_recipient_placeholders`, `approval_send_links_deleted`. The counts
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
failed as soon as one of its rows is. On a send's page, each closed recipient
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
