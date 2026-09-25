# SkyMail account-access gate

SkyMail checks the shared permanent account denylist after Keycloak `userinfo`
authentication and before any permission middleware or `/v1` handler. This
applies equally to human and client-credentials subjects. A blocked subject gets
a generic `401`. Redis, timeout, missing or mismatched contract, and malformed
marker failures get `503`, `Cache-Control: no-store`, and `Retry-After: 1`.

The gate has only two modes:

- `ACCOUNT_ACCESS_GATE_MODE=off` does not connect to Redis and preserves the
  pre-cutover behavior.
- `ACCOUNT_ACCESS_GATE_MODE=enforce` requires every Redis, auth, mTLS, database,
  and timeout setting in `.env.example`. The operation timeout must be between
  50 ms and 2 seconds; use 200 ms unless the production network is proven to
  need a different bound. The Keycloak realm URL must be exactly
  `https://e.yildizskylab.com/realms/e-skylab`.

Use the shared dedicated Redis deployment, never the CMS or Forms cache. It must
run a pinned Redis version with `maxmemory-policy noeviction`, AOF persistence,
`appendfsync always`, authenticated TLS, and a read-only SkyMail ACL. That ACL
must be limited to connection/read operations plus the following key prefixes:

- `skylab:account-access:v1:contract`
- `skylab:account-access:v1:blocked:*`

SkyMail requires only `GET` and `MGET`; do not grant write, expiry, delete,
unlink, flush, or administrative commands. Mount the client certificate, private
key, and CA certificate read-only at the configured paths.

The v1 contract value is `sha256(iss\0sub);marker=1;ttl=none`. Marker values must
be exactly `1` and have no TTL. The reader never logs the subject, marker digest,
email, username, or other identity data. Decision logs contain only the request
correlation ID and `allowed`, `blocked`, or `unavailable`.

## Account erasure

The erase route, `PUT /internal/v1/account-erasures/{request_id}`, is outside
`/v1` and does not use `userinfo`, but it reads the same Redis through the same
reader twice: the caller's own marker (`core-erasure`'s service account, never
blocked: `401` if it is, `503 access_gate_unavailable` on failure), and the
marker of the subject to erase, which must be there (`409
subject_not_blocked` when it is not, `503 subject_block_unverifiable` on
failure). In `off` mode there is no reader, so the route answers `503
subject_block_unverifiable` with `Retry-After: 300` and erases nothing. See
`docs/data-lifecycle.md`.

## Cutover gates

1. Keep this service in `off` mode until Core has backfilled every durable
   deletion marker and written the contract key last.
2. Configure the read-only ACL and mTLS files, switch to `enforce`, and require
   `/ready` to return `204`.
3. Exercise a token minted before a synthetic block against template, mailing
   list, and mail-task reads and mutations. It must return `401` with no handler
   side effect; a Redis outage must return `503`.
4. Keep `/health` and `/docs` available during Redis failure.

After the first real deletion, disabling the reader is unsafe. Take SkyMail out
of rotation and ship a forward fix instead. Redis recovery writes all permanent
markers first and the contract key last.
