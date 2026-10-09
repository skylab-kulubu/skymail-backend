# Health, readiness and shutdown

How SkyMail tells Docker Swarm whether a task should get traffic, and how it
stops. Code: `internal/health`, `serve.go`, `healthcheck.go`,
`internal/mailer` (`Stop`).

Dokploy updates a service start-first (the new task starts, then the old one
stops) and rolls back a failed update. Without a health check Swarm stops the
old task as soon as the new one is running, before it listens, and gives it
traffic at once; with one, the old task keeps serving until the new one is
healthy. A health check therefore helps with one replica as much as with
several.

## Endpoints

| Route | Answers | Asks |
|---|---|---|
| `GET /health` | always `204` | nothing (liveness: the process answers). Release checks read it (`/api/skymail/health`) |
| `GET /ready` | `204`, or `503` with `Cache-Control: no-store` and `Retry-After: 1` | in order: the task is not shutting down; the database answers a ping; the account access gate's contract sentinel (`ACCOUNT_ACCESS_GATE_MODE=enforce` only, `docs/account-access-gate.md`) |
| `GET /ready?gate=skip` | the same | the first two only: the container's health check |

`/ready` keeps answering `204` when all is well, which is what the gate's
cutover and the secret rotator read; it now also says `503` while the
database does not answer. The `503` body never says why; the log does.

- **Database.** A ping on readiness's own connection (one per task, never the
  main pool's: a pool whose connections are all busy is a slow moment, not an
  outage, and must not get the task restarted under load), within 2 s. One
  ping at a time: a caller arriving while it runs takes its answer (never a
  ping of its own after it), so no caller waits longer than the ping timeout,
  however slow the database. The answer is then reused for a second from when
  it came, so however often the public route is asked the database sees at
  most one ping a second. The log says once when the database stops answering
  and once when it is back. The connection is kept a day, not re-made every
  hour: re-made at a moment the database is at `max_connections`, it would
  fail the check of a SkyMail that is fine.
- **Migrations** are done by construction: SkyMail starts listening only
  after they have run (`DATABASE_MIGRATIONS_MODE=apply`).
- **Shutting down.** From the stop signal on, `/ready` answers `503`.
- **Not asked:** Keycloak, the SMTP relay, the account access gate's Redis
  (in the health check). Their outage fails only what needs them, the same on
  every task; failing readiness for them would take every task out (or get
  them restarted) and turn a partial outage into a whole one. Mail queued
  while the relay is down waits in the database and is retried.

Swarm has one health check, not Kubernetes' separate liveness and readiness:
a task that fails it `retries` times in a row is replaced. So the health check
asks `/ready?gate=skip`: the task and its database, not the gate's Redis.
Restarting SkyMail for a gate Redis that is down fixes nothing and takes down
what needs no Redis (the queue keeps sending; `/health`, `/docs`). With the
gate down, `/v1` requests answer `503` as before, and `/ready` itself still
says so.

The database stays in the check: SkyMail can neither queue nor send without
it. The settings below let a database restart pass (a minute) without a
restart of SkyMail. A longer outage restarts it; it comes back on its own once
the database is back.

## The health check

`skymail-backend healthcheck` asks this container's SkyMail for
`/ready?gate=skip` (`http://127.0.0.1:$APP_PORT`, 3000 unset, 3 s) and exits 0
on `2xx`, 1 otherwise, saying why on stderr (kept by Docker: `docker
inspect`). The image needs no curl or wget.

The image carries it (`Dockerfile`):

```
HEALTHCHECK --interval=10s --timeout=5s --start-period=60s --start-interval=2s --retries=6 \
  CMD ["/app/skymail-backend", "healthcheck"]
```

A Swarm service uses the image's health check unless its own replaces it, so
Dokploy's Health Check field may stay empty for SkyMail. Set there (Advanced →
Swarm Settings; durations in nanoseconds), it is:

```json
{
  "Test": ["CMD", "/app/skymail-backend", "healthcheck"],
  "Interval": 10000000000,
  "Timeout": 5000000000,
  "StartPeriod": 60000000000,
  "StartInterval": 2000000000,
  "Retries": 6
}
```

| Setting | Value | Why |
|---|---|---|
| Interval | 10 s | a failing task is noticed within seconds |
| Timeout | 5 s | over the command's 3 s, which is over the ping's 2 s |
| StartPeriod | 60 s | startup is the migrations and little else; a task waiting for another's migrations waits too (up to 5 minutes, `migrations.LockWait`). A release with a long migration needs more |
| StartInterval | 2 s | during the start period: the new task is healthy within 2 s of listening, so a deploy moves over quickly (Docker Engine 25+; older engines ignore it and use Interval) |
| Retries | 6 | a task is replaced after about a minute of failures; a database restart passes |
| **Stop Grace Period** | **30 s** (`30000000000`) | must be over SkyMail's 25 s shutdown; Swarm's default is 10 s |

The Dokploy settings are written by the hub's Swarm settings wizard
(sky_lab_genel `.scratch/horizontal-scale`, ticket 19).

## Shutdown

On SIGTERM (Docker's stop; SIGINT too, a second one kills at once) SkyMail:

1. answers `/ready` with `503`;
2. stops the mailer, beside the HTTP drain:
   - the dispatcher takes no more queue rows (it finishes a take already
     under way);
   - the rows it took that no worker has begun go back to `pending` at once,
     unclaimed, with no attempt counted (`ReleaseMailQueueItem`, fenced on
     `claimed_by` and `claimed_at` like every outcome), so the next task sends
     them without waiting out the lease (`MAIL_QUEUE_LEASE`, 10 minutes);
   - the sends in progress (at most one per worker, three) finish and record
     their outcome;
   - the lease reaper stops;
3. stops taking connections, closes idle keep-alive connections, answers each
   request in flight with `Connection: close`, and waits up to **20 s** for
   them; a request still open then is cut off;
4. once the mailer has stopped (or at **25 s**), cancels the background work:
   the mail approval expiry sweep (its transaction rolls back and the next
   sweep takes it up) and a send still in progress, which is cut off;
5. closes the access gate's Redis client and the database pools, still within
   the 25 s, and exits.

Swarm takes a task out of its load balancer (and waits about two seconds)
before it sends SIGTERM, so step 3 turns away no new connection. Whatever has
not stopped or closed at 25 s is named in the log and left behind (a pool's
close waits for every connection in use), and the process exits at once,
before Docker's SIGKILL at 30 s.

A send is bounded by `SendBudget` (1 minute from dial to QUIT), so a send can
outlast the 25 s on a stalled relay. Cut off at step 4, it counts no failed
attempt: the row keeps its claim and goes back to `pending` when its lease
runs out, as after a crash. A send that did finish writes its outcome even if
the cut came at that moment (5 s of its own), so a mail the relay took is not
sent again after the lease.

Mail queued during the drain (a request in flight) is in the database; the
other task's dispatcher takes it at its next tick (10 s), or the next task to
start does.

A stop signal while waiting for another task's migrations ends the wait and
the process exits; a migration under way is never cut off. A stop signal
during the rest of startup is acted on once startup is through.

## Migrations: expand, then contract

Start-first means the old task keeps serving while the new one migrates and
starts, with the new schema. A migration must keep the previous release
working:

- **expand** (this release): add tables, nullable columns, columns with a
  default, indexes (`CONCURRENTLY` on large tables), new constraints
  `NOT VALID` first;
- **contract** (a later release, once no running code reads it): drop or
  rename columns and tables, make columns `NOT NULL`, validate constraints.

A rename is an expand (add the new column, write both) and a later contract
(drop the old one). A release that cannot follow this is deployed stop-first,
once, with the downtime announced. More, rollback and dirty-version recovery
included: `docs/database-migrations.md`.

Tasks starting together (a scale-up, two replicas) migrate one at a time: a
task holds the session advisory lock `hashtextextended('skymail-backend
migrations', 0)` from before it reads the version until it is through, and the
others wait for it (up to 5 minutes, then the task fails and restarts), then
find nothing to do. Without it a task starting during another's migration
read the version golang-migrate marks dirty while it applies, or an empty
database the other was filling, and refused to start.

## Connections

Each task holds its main pool (pgxpool's default, max(4, CPUs), unless
`DATABASE_URL` says `pool_max_conns`), readiness's 1, and while it migrates
one more connection plus golang-migrate's own (closed once the migrations are
through). During a start-first update two tasks hold them. Recommended:
`pool_max_conns=5` in `DATABASE_URL` (the migrations' own connections leave
the `pool_*` settings out), so a task holds at most 6 and two tasks 12.
