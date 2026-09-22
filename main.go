package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

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
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
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

//	@contact.name	Enes Genç
//	@contact.url	https://enesgenc.dev
//	@contact.email	hello@enesgenc.dev

// @schemes	https
// @host		skymail-api.yildizskylab.com
// @BasePath	/v1
func main() {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	vld := validator.NewStructValidator()

	cfg, err := config.LoadConfig(vld)
	if err != nil {
		panic(err)
	}
	migrationConfig, err := migrations.ConfigFromEnv(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid database migration configuration")
	}
	if migrationConfig.Mode == migrations.ModeApply {
		version, migrationErr := migrations.Run(ctx, cfg.DatabaseURL, migrationConfig.BaselineVersion)
		if migrationErr != nil {
			log.Fatal().Err(migrationErr).Msg("database migration failed")
		}
		log.Info().Uint("version", version).Msg("database migrations applied")
	} else {
		log.Warn().Msg("database migrations disabled: DATABASE_MIGRATIONS_MODE is not apply")
	}

	conn, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("error connecting to database")
	}
	defer conn.Close()

	db := database.NewStore(conn)
	mailerService := mailer.NewMailer(db, mailer.SMTPConfig{
		FromEmail: cfg.SMTPFrom,
		Host:      cfg.SMTPHost,
		Port:      cfg.SMTPPort,
		User:      cfg.SMTPUser,
		Password:  cfg.SMTPPass,
		FQDN:      cfg.SMTPFQDN,
		Plain:     cfg.SMTPPlain,
	})

	authMiddleware := middlewares.NewAuthMiddleware("skymail", cfg.KeycloakRealmURL)
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
		defer redisClient.Close()
		accountAccessGate = accessgate.NewRedisGate(redisClient, gateConfig.OperationTimeout)
	}

	trustedProxies, err := trustedProxyRanges(config.Value)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid trusted proxy configuration")
	}
	log.Info().Str("ranges", strings.Join(trustedProxies, ",")).Msg("trusted proxy ranges")

	kcClient := keycloak.NewClient(cfg.KeycloakRealmURL, cfg.KeycloakServiceClientID, cfg.KeycloakServiceClientSecret)

	templateHandler := handlers.NewTemplateHandler(db)
	listHandler := handlers.NewListHandler(db, kcClient)
	mailHandler := handlers.NewMailHandler(db, mailerService, kcClient)

	// The reverse proxy in front of Skymail discards a caller-supplied
	// X-Forwarded-For and writes its own, so ProxyHeader is only safe to read
	// when the connection came from one of those proxies. Without TrustProxy
	// Fiber ignores the header entirely and ctx.IP() reports the proxy.
	app := fiber.New(fiber.Config{
		StructValidator:    vld,
		JSONDecoder:        sonic.Unmarshal,
		JSONEncoder:        sonic.Marshal,
		ErrorHandler:       errorHandler,
		TrustProxy:         true,
		TrustProxyConfig:   fiber.TrustProxyConfig{Proxies: trustedProxies},
		ProxyHeader:        fiber.HeaderXForwardedFor,
		EnableIPValidation: true,
	})

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

	registerPublicRoutes(app, accountAccessGate)

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

	registerMailTaskRoutes(api, authMiddleware, mailHandler)

	mailerService.Start(ctx, 3)

	addr := fmt.Sprintf(":%d", 3000)
	if cfg.AppPort != 0 {
		addr = fmt.Sprintf(":%d", cfg.AppPort)
	}

	if err = app.Listen(addr); err != nil {
		log.Fatal().Err(err).Msg("error starting server")
	}
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
}

func registerMailTaskRoutes(api fiber.Router, auth middlewares.AuthMiddleware, mail handlers.MailHandler) {
	tasks := api.Group("/mail_tasks")
	tasks.Post("/", auth.RequireAnyPermission("skymail:mails:write"), mail.CreateTask)
	tasks.Post("/single", auth.RequireAnyPermission("skymail:mails:send", "skymail:mails:write"), mail.SendSingle)
	tasks.Get("/", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTasks)
	// Before /:id, which would otherwise take "summary" for a task id.
	tasks.Get("/summary", auth.RequireAnyPermission("skymail:mails:read"), mail.GetSummary)
	tasks.Get("/:id", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTask)
	tasks.Get("/:id/queue", auth.RequireAnyPermission("skymail:mails:read"), mail.GetTaskQueueItems)
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

func registerPublicRoutes(app *fiber.App, gate accessgate.Reader) {
	document := swaggerDocument()
	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/ready", func(c fiber.Ctx) error {
		if gate != nil {
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
		log.Info().
			Str("method", ctx.Method()).
			Str("path", ctx.Path()).
			Str("ip", ctx.IP()).
			Str("ua", ctx.Get(fiber.HeaderUserAgent)).
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
