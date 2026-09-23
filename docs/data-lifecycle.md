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
a resubmission is the same request with the events before it kept. The send an
approval queues is an ordinary `mail_tasks` row, kept as every send is, and
the request and its `approved` or `accepted` event name it by `task_id`.

A request keeps what would be sent — the template, pinned to the version it
was submitted on, the audience and the variables — its state and its
deadline: seven days after it was last submitted, or after an approver
returned it. A request still pending or returned at its deadline is reported
as expired at once; the one-minute sweep records the expiry as an `expired`
event with no actor and mails the submitter. Reading or listing a request
writes nothing.

## Personal data

These fields keep a person's identity with no end date:

- `mail_tasks.sent_by` — who sent: the Keycloak subject of the token that
  queued the send — for an approved Mail onayı request, its submitter's — or
  `skymail` for the mail SkyMail sends of its own accord, the notice that a
  request expired.
- `archived_by` on templates and mailing lists — who archived.
- `template_versions.author_sub` with `author_name` — who wrote a version, and
  the name their token carried then.
- `mail_approvals.submitter_sub`, `submitter_name` and `submitter_email` — who
  submitted a request, and the name and verified address their token carried
  (`submitter_email_unverified` says the token carried an address Keycloak
  had not verified, which is not kept).
- `mail_approvals.recipient_email` and `recipient_full_name` — the one
  recipient of a single send.
- `mail_approvals.body_variables` — the values of the send, which may name or
  address people.
- `mail_approval_events.actor_sub` and `actor_name` — who did each thing to a
  request — and `changes`, each variable's value before and after an edit or a
  resubmission.

SkyMail has no erasure or anonymisation path for account deletion: nothing
tells it an account was deleted, and nothing clears or replaces these fields.
This is a known gap, shared by all of them, against ADR-0042's rule that PII
is erased on account deletion rather than kept.

## Retention boundaries

Archiving a template or list preserves `mail_tasks`, `mail_queue`, and mailing
list recipient associations. Historical task reads continue to show the source
template/list name. Archived templates and lists cannot be used to create new
mail tasks.

Removing one recipient from a mailing list remains a physical deletion of the
`mailing_list_recipients` relationship row. Queue residue and other transient
worker data remain outside this durable-record lifecycle.
