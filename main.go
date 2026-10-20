package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/docs"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/config"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/erasuretoken"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/health"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
	"github.com/yokeTH/gofiber-scalar/scalar/v3"
)

var swaggerDocument = sync.OnceValue(docs.SwaggerInfo.ReadDoc)

//	@title			Skymail
//	@version		1.0
//	@description	This is the API documentation for Skymail.

//	@tag.name			Templates
//	@tag.description	Email template management operations

//	@tag.name			Lists
//	@tag.description	Mailing list and recipient management operations

//	@tag.name			Mail approval
//	@tag.description	Mail onayı: sends submitted by members without send permission, held until an approver decides them

//	@contact.name	Enes Genç
//	@contact.url	https://enesgenc.dev
//	@contact.email	hello@enesgenc.dev

// @schemes	https
// @host		skymail-api.yildizskylab.com
// @BasePath	/v1
func main() {
	// A maintenance command or the container's health check instead of the
	// server: they need none of the server's startup, migrations included.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case queueCloseRestoredCommandName:
			os.Exit(runQueueCloseRestoredFromEnv(os.Args[2:], os.Stdout))
		case healthcheckCommandName:
			os.Exit(runHealthcheck(os.Args[2:], healthcheckEnv(), os.Stderr))
		}
	}

	// SIGTERM (Docker's stop) or SIGINT ends stop; serve then shuts down in
	// order (docs/health-and-shutdown.md). A second signal kills at once.
	stop, releaseSignals := stopSignals()
	defer releaseSignals()
	go func() {
		<-stop.Done()
		releaseSignals()
	}()

	vld := validator.NewStructValidator()

	cfg, err := config.LoadConfig(vld)
	if err != nil {
		panic(err)
	}
	migrationConfig, err := migrations.ConfigFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid database migration configuration")
	}
	// Read before anything runs: a value that is neither on nor paused stops
	// startup rather than sending a restored queue.
	mailSender, err := mailer.SenderFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid mail sender configuration")
	}
	// How long a queue row this process takes stays its own: past it, another
	// process may put the row back and send it.
	queueLease, err := mailer.QueueLeaseFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid mail queue lease configuration")
	}
	// The relay's limits, shared by every process (README, send budget).
	rateBudget, err := mailer.RateBudgetFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid mail send budget configuration")
	}
	keycloakAdminURL, err := keycloak.ParseAdminURL(cfg.KeycloakAdminURL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid Keycloak configuration")
	}
	audienceMode, err := middlewares.AudienceModeFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid v1 token audience configuration")
	}
	formsSourceConfig, err := middlewares.FormsSourceConfigFromEnv(config.Value, cfg.KeycloakRealmURL, cfg.KeycloakClientID)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid Forms source configuration")
	}
	// How long a send's content is kept and shown (docs/data-lifecycle.md).
	contentRetentionDays, err := handlers.ContentRetentionDaysFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid mail content retention configuration")
	}
	// How long the record of who opened a masked mail is kept.
	viewRetentionDays, err := handlers.ViewRetentionDaysFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid sent content views retention configuration")
	}
	// Open and click tracking: off unless SKYMAIL_TRACKING=on, and then only
	// with the public /t/ address and core to ask (README, tracking).
	trackingConfig, err := trackingConfigFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid open and click tracking configuration")
	}
	// Its retention run: off unless SKYMAIL_TRACKING_RETENTION says
	// dry-run, apply or purge (README, tracking retention).
	trackingRetention, err := tracking.RetentionModeFromEnv(config.Value, trackingConfig)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid open and click tracking retention configuration")
	}
	trackingRetentionMaxRows, err := tracking.RetentionMaxRowsFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid open and click tracking retention configuration")
	}
	if migrationConfig.Mode == migrations.ModeApply {
		// A stop signal ends a wait for another process's migrations, never
		// a migration under way.
		version, migrationErr := migrations.Run(stop, cfg.DatabaseURL, migrationConfig.BaselineVersion)
		if migrationErr != nil && stop.Err() != nil {
			log.Info().Err(migrationErr).Msg("stop signal while waiting for another process's migrations: exiting")
			return
		}
		if migrationErr != nil {
			log.Fatal().Err(migrationErr).Msg("database migration failed")
		}
		log.Info().Uint("version", version).Msg("database migrations applied")
	} else {
		log.Warn().Msg("database migrations disabled: DATABASE_MIGRATIONS_MODE is not apply")
	}

	conn, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("error connecting to database")
	}
	// /ready pings on a connection of its own (docs/health-and-shutdown.md).
	readinessDatabase, err := openReadinessDatabase(cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("error configuring readiness")
	}
	readiness := health.NewReadiness(readinessDatabase, health.Options{
		Logf: func(format string, args ...any) { log.Warn().Msgf(format, args...) },
	})
	// Closed in this order once shutdown has stopped everything else.
	var closers []closing

	if keycloakAdminURL != "" {
		log.Info().Str("admin_url", keycloakAdminURL).Msg("keycloak admin REST: KEYCLOAK_ADMIN_URL (tokens are still checked against KEYCLOAK_REALM_URL)")
	}
	kcClient := keycloak.NewClient(cfg.KeycloakRealmURL, keycloakAdminURL, cfg.KeycloakServiceClientID, cfg.KeycloakServiceClientSecret)

	log.Info().Str("mode", string(trackingConfig.Mode)).Str("base_url", trackingConfig.BaseURL).Str("core_url", trackingConfig.CoreURL).
		Msg("open and click tracking (SKYMAIL_TRACKING)")
	log.Info().Str("mode", string(trackingRetention)).Int("event_retention_days", tracking.EventRetentionDays).Int64("max_rows", trackingRetentionMaxRows).
		Msg("open and click tracking retention (SKYMAIL_TRACKING_RETENTION)")
	db := database.NewStore(conn)
	mailerService := mailer.NewMailer(db, mailer.SMTPConfig{
		FromEmail: cfg.SMTPFrom,
		Host:      cfg.SMTPHost,
		Port:      cfg.SMTPPort,
		User:      cfg.SMTPUser,
		Password:  cfg.SMTPPass,
		FQDN:      cfg.SMTPFQDN,
		Plain:     cfg.SMTPPlain,
	}, mailSender, mailer.WithQueueLease(queueLease), mailer.WithRateBudget(rateBudget),
		mailer.WithTracking(mailer.Tracking{
			Config:      trackingConfig,
			Preferences: tracking.NewCoreClient(trackingConfig.CoreURL, kcClient, nil),
		}))

	log.Info().Str("mode", string(audienceMode)).Str("audience", cfg.KeycloakClientID).Msg("v1 token audience check (V1_TOKEN_AUDIENCE_MODE)")
	authMiddleware := middlewares.NewAuthMiddleware(cfg.KeycloakClientID, cfg.KeycloakRealmURL, audienceMode)
	gateConfig, err := accessgate.ConfigFromEnv(config.Value, cfg.KeycloakRealmURL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid account access gate configuration")
	}
	var accountAccessGate accessgate.Reader
	if gateConfig.Mode == accessgate.ModeEnforce {
		redisClient, redisErr := accessgate.NewRedisClient(gateConfig)
		if redisErr != nil {
			log.Fatal().Err(redisErr).Msg("error configuring account access gate")
		}
		closers = append(closers, closing{name: "the account access gate's Redis client", close: func() { _ = redisClient.Close() }})
		accountAccessGate = accessgate.NewRedisGate(redisClient, gateConfig.OperationTimeout)
	}

	trustedProxies, err := trustedProxyRanges(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid trusted proxy configuration")
	}
	log.Info().Str("ranges", strings.Join(trustedProxies, ",")).Msg("trusted proxy ranges")

	templateHandler := handlers.NewTemplateHandler(db)
	listHandler := handlers.NewListHandler(db, kcClient)
	mailHandler := handlers.NewMailHandler(db, mailerService, kcClient, handlers.WithContentRetentionDays(contentRetentionDays), handlers.WithFormSourceLimit(formsSourceConfig.MaxRecipients))
	approvalHandler := handlers.NewMailApprovalHandler(db, mailerService, kcClient, handlers.MailApprovalOptions{
		ClientID: cfg.KeycloakClientID,
		UIURL:    config.Value("SKYMAIL_UI_URL"),
	})
	erasureHandler := handlers.NewAccountErasureHandler(db, erasuretoken.NewVerifier(erasureTokenConfig(cfg)), accountAccessGate)
	withdrawalHandler := handlers.NewTrackingWithdrawalHandler(db, erasuretoken.NewVerifier(withdrawalTokenConfig(cfg)), accountAccessGate)

	app := fiber.New(newFiberConfig(vld, trustedProxies))

	app.Use(serverRequestID())
	app.Use(recover.New())

	/*
		app.Use(cors.New(cors.Config{
			AllowOrigins:     []string{"*"},
			AllowHeaders:     []string{"*", "Authorization", "Retry-After"},
			ExposeHeaders:    []string{"X-Total-Count"},
			AllowCredentials: false,
		}))
	*/

	handlers.NewTrackingPublicHandler(db, trackingConfig).Register(app)
	registerPublicRoutes(app, accountAccessGate, readiness)
	registerInternalRoutes(app, erasureHandler, withdrawalHandler)

	api := protectedAPI(app, authMiddleware, accountAccessGate)

	registerTemplateRoutes(api, authMiddleware, templateHandler)

	lists := api.Group("/mailing_lists")
	lists.Post("/", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.CreateList)
	lists.Get("/", authMiddleware.RequireAnyPermission("skymail:lists:read"), listHandler.GetLists)
	lists.Get("/:id", authMiddleware.RequireAnyPermission("skymail:lists:read"), listHandler.GetList)
	lists.Patch("/:id", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.UpdateList)
	lists.Delete("/:id", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.DeleteList)
	lists.Post("/:id/restore", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.RestoreList)
	lists.Post("/:id/recipients", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.AddRecipient)
	lists.Get("/:id/recipients", authMiddleware.RequireAnyPermission("skymail:lists:read"), listHandler.GetRecipients)
	lists.Delete("/:id/recipients/:recipientId", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.RemoveRecipient)

	registerFormMailSourceRoutes(api, authMiddleware, mailHandler, formsSourceConfig)
	registerMailTaskRoutes(api, authMiddleware, mailHandler)
	registerMailApprovalRoutes(api, authMiddleware, approvalHandler)
	registerNotificationRoutes(api, authMiddleware, handlers.NewNotificationHandler(db, mailerService, kcClient))

	// The background work's context: shutdown cancels it only after the
	// mailer has stopped (or its time ran out), which cuts off a send still
	// in progress and leaves its row to the lease.
	workers, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	// Paused (MAIL_SENDER=paused), this only puts back the processing rows
	// whose lease ran out: the API, /ready and the erase endpoint serve as
	// ever, and nothing is sent.
	mailerService.Start(workers, 3)
	approvalsStopped := make(chan struct{})
	go func() {
		defer close(approvalsStopped)
		expireMailApprovals(workers, approvalHandler, time.Minute)
	}()
	contentSweepStopped := make(chan struct{})
	go func() {
		defer close(contentSweepStopped)
		clearMailContent(workers, db, contentRetentionDays, viewRetentionDays, contentSweepInterval)
	}()
	trackingRetentionStopped := make(chan struct{})
	go func() {
		defer close(trackingRetentionStopped)
		runTrackingRetention(workers, db, database.TrackingRetentionOptions{Mode: trackingRetention, BrakeMaxRows: trackingRetentionMaxRows, OnlyIfDue: true}, trackingRetentionCheckInterval)
	}()

	addr := fmt.Sprintf(":%d", 3000)
	if cfg.AppPort != 0 {
		addr = fmt.Sprintf(":%d", cfg.AppPort)
	}
	// Fiber's Listen network, opened here so that shutdown can close it.
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}

	closers = append(closers,
		closing{name: "the readiness database pool", close: readinessDatabase.Close},
		closing{name: "the database pool", close: conn.Close},
	)
	err = serve(stop, app, ln, shutdownPlan{
		Readiness:    readiness,
		HTTPDrain:    httpDrainTimeout,
		Total:        shutdownTimeout,
		StopMailer:   mailerService.Stop,
		SendCutGrace: sendCutGrace,
		StopWorkers:  stopWorkers,
		Wait: []stopping{
			{name: "the mail approval expiry sweep", done: approvalsStopped},
			{name: "the mail content sweep", done: contentSweepStopped},
			{name: "the tracking retention run", done: trackingRetentionStopped},
			{name: "the mailer's sends", done: mailerService.Stopped()},
		},
		Close: closers,
		Logf:  func(format string, args ...any) { log.Info().Msgf(format, args...) },
	})
	if err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}
}

// trackingConfigFromEnv validates tracking before any service starts. Routes
// now exist, but production enablement still waits for the rollout gates.
func trackingConfigFromEnv(getenv func(string) string) (tracking.Config, error) {
	return tracking.ConfigFromEnv(getenv)
}

// erasureTokenConfig is how the erase route checks core's token: the exact
// issuer KEYCLOAK_REALM_URL and that realm's keys. KEYCLOAK_ADMIN_URL plays
// no part.
func erasureTokenConfig(cfg config.Config) erasuretoken.Config {
	return erasuretoken.Config{
		Issuer:         cfg.KeycloakRealmURL,
		JWKSURL:        cfg.KeycloakRealmURL + "/protocol/openid-connect/certs",
		ResourceClient: cfg.KeycloakClientID,
	}
}

// withdrawalTokenConfig is how the tracking withdrawal route checks core's
// token: as the erase route's, but core's own client with SkyMail's
// withdraw role (ticket 17), never core-erasure's.
func withdrawalTokenConfig(cfg config.Config) erasuretoken.Config {
	tokens := erasureTokenConfig(cfg)
	tokens.CallerClientID = erasuretoken.WithdrawCallerClientID
	tokens.Role = erasuretoken.WithdrawRole
	return tokens
}

func registerTemplateRoutes(api fiber.Router, auth middlewares.AuthMiddleware, template handlers.TemplateHandler) {
	templates := api.Group("/templates")
	templates.Post("/", auth.RequireAnyPermission("skymail:templates:write"), template.CreateTemplate)
	templates.Get("/", auth.RequireAnyPermission("skymail:templates:read"), template.GetTemplates)
	templates.Get("/:id", auth.RequireAnyPermission("skymail:templates:read"), template.GetTemplate)
	templates.Patch("/:id", auth.RequireAnyPermission("skymail:templates:write"), template.UpdateTemplate)
	templates.Delete("/:id", auth.RequireAnyPermission("skymail:templates:write"), template.DeleteTemplate)
	templates.Get("/by-key/:key", auth.RequireAnyPermission("skymail:templates:read"), template.GetTemplateByKey)
	templates.Put("/by-key/:key", auth.RequireAnyPermission("skymail:templates:write"), template.UpsertTemplateByKey)
	templates.Post("/:id/restore", auth.RequireAnyPermission("skymail:templates:write"), template.RestoreTemplate)
	templates.Get("/:id/versions", auth.RequireAnyPermission("skymail:templates:read"), template.ListTemplateVersions)
	templates.Get("/:id/versions/:versionId", auth.RequireAnyPermission("skymail:templates:read"), template.GetTemplateVersion)
	templates.Post("/:id/required-variables", auth.RequireAnyPermission("skymail:templates:write"), template.AddRequiredVariable)
	templates.Delete("/:id/required-variables/:name", auth.RequireAnyPermission("skymail:templates:write"), template.RemoveRequiredVariable)
	templates.Post("/:id/drafts", auth.RequireAnyPermission("skymail:templates:write"), template.SaveTemplateDraft)
	templates.Post("/:id/versions/:versionId/publish", auth.RequireAnyPermission("skymail:templates:write"), template.PublishTemplateVersion)
	templates.Post("/:id/versions/:versionId/restore", auth.RequireAnyPermission("skymail:templates:write"), template.RestoreTemplateVersion)
	templates.Post("/:id/versions/:versionId/discard", auth.RequireAnyPermission("skymail:templates:write"), template.DiscardTemplateVersion)
	templates.Put("/:id/content-sensitive", auth.RequireAnyPermission("skymail:templates:write"), template.SetContentSensitive)
	templates.Get("/:id/content-sensitive/events", auth.RequireAnyPermission("skymail:templates:read"), template.ListContentSensitiveEvents)
}

func registerFormMailSourceRoutes(api fiber.Router, auth middlewares.AuthMiddleware, mail handlers.MailHandler, cfg middlewares.FormsSourceConfig) {
	sources := api.Group("/form_mail_sources")
	sources.Post("/", auth.RequireAnyPermission("skymail:mails:send", "skymail:mails:write"), middlewares.RequireFormsSourceService(cfg), mail.CreateFormSource)
	sources.Get("/:id", mail.GetFormSource)
}

func registerMailTaskRoutes(api fiber.Router, auth middlewares.AuthMiddleware, mail handlers.MailHandler) {
	tasks := api.Group("/mail_tasks")
	tasks.Post("/", auth.RequireAnyPermission("skymail:mails:write"), mail.CreateTask)
	tasks.Post("/single", auth.RequireAnyPermission("skymail:mails:send", "skymail:mails:write"), mail.SendSingle)
	tasks.Post("/form-source", auth.RequireAnyPermission("skymail:mails:send", "skymail:mails:write"), mail.SendFormSource)
	tasks.Get("/", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTasks)
	// Before /:id, which would otherwise take "summary" for a task id.
	tasks.Get("/summary", auth.RequireAnyPermission("skymail:mails:read"), mail.GetSummary)
	tasks.Get("/:id", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTask)
	tasks.Get("/:id/tracking", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTracking)
	tasks.Get("/:id/tracking/recipients", mail.GetTrackingRecipients)
	tasks.Get("/:id/queue", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTaskQueueItems)
	// Who may see the content itself the handler decides from the send and
	// the caller's roles (handlers.SystemMailsReadRole).
	tasks.Get("/:id/queue/:itemId/content", auth.RequireAnyPermission("skymail:mails:read"), mail.GetQueueItemContent)
}

// contentSweepInterval is how often the content sweep runs: a sensitive
// send's raw mail is masked within it of the send being done, and a send's
// content is gone within it of its retention period ending.
const contentSweepInterval = 5 * time.Minute

// contentSweepBatch is how many sends one statement of the sweep empties. A
// list send can hold thousands of rows with their HTML, so the batch is kept
// small: a run that finds many sends past retention is many short
// transactions rather than a few long ones.
const contentSweepBatch = 20

// viewSweepBatch is how many records of who opened a masked mail one
// statement deletes.
const viewSweepBatch = 500

// clearMailContent masks the sensitive sends that are done and empties the
// content of the sends due — past retentionDays, sensitive or not, once none
// of its rows is still to go out — at
// startup and every interval after (docs/data-lifecycle.md, sent mail
// content). It runs while the sender is paused too: it touches no row still
// to go out. Then it deletes the records of who opened a masked mail older
// than viewRetentionDays. It logs counts only.
func clearMailContent(ctx context.Context, db *database.Store, retentionDays, viewRetentionDays int, interval time.Duration) {
	log.Info().Int("retention_days", retentionDays).Int("view_retention_days", viewRetentionDays).
		Msg("mail content sweep (MAIL_CONTENT_RETENTION_DAYS, SENT_CONTENT_VIEWS_RETENTION_DAYS)")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// Sensitive sends are masked first, so the raw mail is gone within a
		// tick of a send being done (internal/mailer/mask.go).
		masked, err := mailer.MaskSensitiveContent(ctx, db, mailer.MaskOptions{})
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("sensitive content masking failed")
		}
		if masked.Sends > 0 || masked.NotKept > 0 || masked.Failed > 0 {
			log.Info().Int("sends", masked.Sends).Int("rows", masked.Rows).Int("not_kept", masked.NotKept).
				Int("past_deadline", masked.PastDeadline).Int("failed", masked.Failed).Int("not_sensitive", masked.NotSensitive).
				Msg("masked the content of sensitive sends")
		}
		sends, rows, err := db.ClearMailContent(ctx, retentionDays, contentSweepBatch)
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("mail content sweep failed")
		}
		if sends > 0 {
			log.Info().Int("sends", sends).Int64("rows", rows).Msg("emptied the content of sends past retention")
		}
		views, err := db.DeleteSentContentViews(ctx, viewRetentionDays, viewSweepBatch)
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("sent content views sweep failed")
		}
		if views > 0 {
			log.Info().Int64("views", views).Msg("deleted the records of masked mail openings past retention")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// trackingRetentionCheckInterval is how often the tracking retention run
// checks whether it is due; it runs at most once a day (the receipts say).
const trackingRetentionCheckInterval = time.Hour

// runTrackingRetention runs open and click tracking's retention rules
// (database.RunTrackingRetention) at startup and whenever a run of its mode
// is due, checking every interval. Off, it does nothing. Each run writes its
// receipt (tracking_retention_runs) and one log line of counts; the brake
// and a failure log at error level. Nothing it logs names a person, a token
// or a link.
func runTrackingRetention(ctx context.Context, db *database.Store, opts database.TrackingRetentionOptions, interval time.Duration) {
	if opts.Mode == tracking.RetentionOff {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		trackingRetentionOnce(ctx, db, opts)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// trackingRetentionOnce runs when due: the run checks that itself, holding
// the lock, so two replicas never run it twice in a day.
func trackingRetentionOnce(ctx context.Context, db *database.Store, opts database.TrackingRetentionOptions) {
	report, err := db.RunTrackingRetention(ctx, opts)
	if errors.Is(err, database.ErrTrackingRetentionNotDue) {
		return
	}
	if errors.Is(err, database.ErrTrackingRetentionLocked) {
		log.Info().Str("event", "tracking_retention_locked").Msg("tracking retention: another replica is running it")
		return
	}
	if report == nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Str("event", "tracking_retention_failed").Msg("tracking retention could not start")
		}
		return
	}
	line := log.Info()
	switch {
	case err != nil || report.Status == "failed":
		line = log.Error().Err(err)
	case report.Status == "refused_large" || report.Status == "would_refuse_large":
		line = log.Error().Str("alarm", "tracking_retention_brake")
	case report.Status == "partial":
		line = log.Warn()
	}
	line.Str("event", "tracking_retention_run").Int64("run_id", report.RunID).Str("mode", string(report.Mode)).
		Str("status", report.Status).Time("events_cutoff", report.EventsCutoff).Interface("counts", report.Counts).
		Msg("tracking retention run (receipt in tracking_retention_runs)")
}

// registerMailApprovalRoutes serves Mail onayı. Anyone who can use SkyMail —
// the /v1 gate is skymail:access — follows their own requests. Submitting one
// takes reading what it submits, skymail:templates:read and for a list
// skymail:lists:read (Yusuf, 2026-09-24); the handler checks them, since only
// the body says which. Deciding one takes the approver's role, and the handler
// keeps a submitter's actions to the submitter.
func registerMailApprovalRoutes(api fiber.Router, auth middlewares.AuthMiddleware, approvals handlers.MailApprovalHandler) {
	requests := api.Group("/mail_approvals")
	approver := auth.RequireAnyPermission(handlers.MailApproverRole)
	requests.Post("/", approvals.Submit)
	requests.Get("/", approvals.List)
	requests.Get("/:id", approvals.Get)
	requests.Post("/:id/approve", approver, approvals.Approve)
	requests.Post("/:id/return", approver, approvals.Return)
	requests.Post("/:id/reject", approver, approvals.Reject)
	requests.Post("/:id/accept", approvals.Accept)
	requests.Post("/:id/decline", approvals.Decline)
	requests.Post("/:id/resubmit", approvals.Resubmit)
}

// registerNotificationRoutes serves POST /notifications to services (hub
// ticket iletisim-servisi 03): a service account's token with
// skymail:notifications:send, never a person's. The notification types are
// read as templates are and written by the Template seed.
func registerNotificationRoutes(api fiber.Router, auth middlewares.AuthMiddleware, notifications handlers.NotificationHandler) {
	api.Post("/notifications", auth.RequireAnyPermission(handlers.NotificationsSendRole), middlewares.RequireServiceAccount(), notifications.Send)
	types := api.Group("/notification_types")
	types.Get("/", auth.RequireAnyPermission("skymail:templates:read"), notifications.ListTypes)
	types.Get("/:key", auth.RequireAnyPermission("skymail:templates:read"), notifications.GetType)
	types.Put("/:key", auth.RequireAnyPermission("skymail:templates:write"), notifications.PutType)
}

// expireMailApprovals expires the requests left undecided past their deadline
// every interval, and tells each submitter. It is the only writer of an
// expiry but one: an action on an overdue request expires it in its own
// transaction and is refused. Reads write nothing; they report an overdue
// request as expired, so nothing a reader sees waits on the sweep.
func expireMailApprovals(ctx context.Context, approvals handlers.MailApprovalHandler, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired, err := approvals.ExpireDue(ctx)
			if err != nil {
				log.Error().Err(err).Msg("mail approval expiry sweep failed")
			}
			if expired > 0 {
				log.Info().Int("expired", expired).Msg("expired mail approval requests past their deadline")
			}
		}
	}
}

// defaultTrustedProxyRanges covers the private and loopback space a container
// network hands out. The proxy in front of Skymail is itself a container whose
// address inside that network is reassigned whenever it is recreated, so trust
// is expressed as ranges and never as one address.
const defaultTrustedProxyRanges = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128,fc00::/7"

// trustedProxyRanges reads TRUSTED_PROXY_RANGES as a comma-separated list of
// CIDR ranges; a bare address means that single host. An entry that is neither
// stops startup rather than being skipped, because a typo would quietly either
// trust a spoofable header or stop trusting the real proxy.
func trustedProxyRanges(getenv func(string) string) ([]string, error) {
	raw := strings.TrimSpace(getenv("TRUSTED_PROXY_RANGES"))
	if raw == "" {
		raw = defaultTrustedProxyRanges
	}
	var ranges []string
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(field); err != nil && net.ParseIP(field) == nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_RANGES: %q is neither a CIDR range nor an IP address", field)
		}
		ranges = append(ranges, field)
	}
	if len(ranges) == 0 {
		return nil, errors.New("TRUSTED_PROXY_RANGES must list at least one CIDR range")
	}
	return ranges, nil
}

func serverRequestID() fiber.Handler {
	assign := requestid.New()
	return func(c fiber.Ctx) error {
		// Correlation IDs are logged by the access gate. Generate them locally so
		// an untrusted header cannot inject identity data into security logs.
		c.Request().Header.Del(fiber.HeaderXRequestID)
		return assign(c)
	}
}

// registerPublicRoutes serves liveness, readiness and the API docs.
// /health is liveness: 204 while the process answers (release checks read
// it). /ready is readiness: the task is not shutting down and its database
// answers, then the account access gate (enforce mode only); ?gate=skip, the
// container's health check, leaves the gate out (docs/health-and-shutdown.md).
// A nil readiness asks no database.
func registerPublicRoutes(app *fiber.App, gate accessgate.Reader, readiness *health.Readiness) {
	document := swaggerDocument()
	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/ready", func(c fiber.Ctx) error {
		if err := readiness.Check(c.Context()); err != nil {
			return unavailable(c)
		}
		if gate != nil && c.Query("gate") != "skip" {
			if err := gate.Ready(c.Context()); err != nil {
				return unavailable(c)
			}
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/docs/openapi.json", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
		return c.SendString(document)
	})
	app.Group("/docs").
		Use(scalar.New(scalar.Config{
			FileContentString: document,
			Path:              "",
			Title:             "Skymail API Documentation",
		}))
}

// registerInternalRoutes serves what other SkyLab services call over the
// Docker network (ADR-0016), outside /v1 and its userinfo chain. The guard
// answers 404 to anything that came through the public ingress; each route
// then checks its token itself (account erasure spec §2.5).
func registerInternalRoutes(app *fiber.App, erasure handlers.AccountErasureHandler, withdrawal handlers.TrackingWithdrawalHandler) {
	internal := app.Group("/internal", middlewares.InternalRouteGuard())
	internal.Put("/v1/account-erasures/:request_id", erasure.Erase)
	internal.Put("/v1/tracking-withdrawals/:sub", withdrawal.Withdraw)
}

func protectedAPI(app *fiber.App, auth middlewares.AuthMiddleware, gate accessgate.Reader) fiber.Router {
	api := app.Group("/v1")
	api.Use(auth.Authenticate)
	api.Use(middlewares.AccountAccessGate(gate))
	api.Use(auth.RequireAnyPermission("skymail:access"))
	return api
}

func unavailable(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderRetryAfter, "1")
	return apperrors.ErrServiceUnavailable
}

func newFiberConfig(vld fiber.StructValidator, trustedProxies []string) fiber.Config {
	// The reverse proxy in front of Skymail discards a caller-supplied
	// X-Forwarded-For and writes its own, so ProxyHeader is only safe to read
	// when the connection came from one of those proxies. Without TrustProxy
	// Fiber ignores the header entirely and ctx.IP() reports the proxy.
	return fiber.Config{
		StructValidator: vld,
		JSONDecoder:     sonic.Unmarshal,
		JSONEncoder:     sonic.Marshal,
		ErrorHandler:    errorHandler,
		// Fiber's 4 KiB read buffer also caps the request headers, and an
		// admin's bearer (many groups and roles) plus the browser's
		// .yildizskylab.com cookies ran over it: every call answered 431.
		// 16 KiB matches core-backend and Node's default.
		ReadBufferSize:     16 << 10,
		TrustProxy:         true,
		TrustProxyConfig:   fiber.TrustProxyConfig{Proxies: trustedProxies},
		ProxyHeader:        fiber.HeaderXForwardedFor,
		EnableIPValidation: true,
	}
}

// clientClass is what a log line says of a client instead of its address:
// loopback, private (the Docker networks, RFC 1918, fc00::/7), link_local,
// public, or unknown when the address does not parse. An IP address is
// personal data and is never logged.
func clientClass(raw string) string {
	ip := net.ParseIP(raw)
	switch {
	case ip == nil || ip.IsUnspecified():
		return "unknown"
	case ip.IsLoopback():
		return "loopback"
	case ip.IsPrivate():
		return "private"
	case ip.IsLinkLocalUnicast():
		return "link_local"
	default:
		return "public"
	}
}

func errorHandler(ctx fiber.Ctx, err error) error {
	var validationErrors validator.ValidationErrors
	if errors.As(err, &validationErrors) {
		return ctx.Status(fiber.StatusBadRequest).
			JSON(apperrors.ErrValidation.WithParams(map[string]interface{}{
				"errors": validator.ParseValidationErrors(validationErrors),
			}))
	}

	var appError *apperrors.AppError
	if errors.As(err, &appError) {
		return ctx.Status(appError.Status).JSON(appError)
	}

	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		// An IP address is personal data: the line says what kind of client
		// asked (clientClass), never its address or its User-Agent.
		log.Info().
			Str("method", ctx.Method()).
			Str("path", ctx.Path()).
			Str("client", clientClass(ctx.IP())).
			Err(err).
			Msg("Resource not found in database (404)")
		return ctx.Status(apperrors.ErrStatusNotFound.Status).JSON(apperrors.ErrStatusNotFound)
	}

	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return ctx.Status(apperrors.ErrConflict.Status).JSON(apperrors.ErrConflict)
	}

	var e *fiber.Error
	if errors.As(err, &e) {
		appError = apperrors.FromFiberError(e)
	} else {
		appError = apperrors.ErrStatusInternalServer
	}

	if appError.Status >= 500 {
		log.Error().
			Str("path", ctx.Path()).
			Str("type", fmt.Sprintf("%T", err)).
			Err(err).
			Send()
	}

	return ctx.Status(appError.Status).JSON(appError)
}
