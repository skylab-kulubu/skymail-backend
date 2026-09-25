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
- `mail_queue.recipient_full_name`, `recipient_email`, and the mail rendered
  for them: `subject`, `body`, `body_html`, and `error`, which can quote the
  address.
- `mail_tasks.sent_by` — who sent: the Keycloak subject of the token that
  queued the send — for an approved Mail onayı request, its submitter's — or
  `skymail` for the mail SkyMail sends of its own accord, the notice that a
  request expired.
- `mail_tasks.body_variables` — the values of the send, which may name or
  address people; for Keycloak's mails, the person's reset or verification
  link.
- `archived_by` on templates and mailing lists — who archived.
- `template_versions.author_sub` with `author_name` — who wrote a version, and
  the name their token carried then.
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

## Retention boundaries

Archiving a template or list preserves `mail_tasks`, `mail_queue`, and mailing
list recipient associations. Historical task reads continue to show the source
template/list name. Archived templates and lists cannot be used to create new
mail tasks.

Removing one recipient from a mailing list remains a physical deletion of the
`mailing_list_recipients` relationship row. Queue residue and other transient
worker data remain outside this durable-record lifecycle.
