package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bytedance/sonic"
	jwtware "github.com/gofiber/contrib/v3/jwt"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/config"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

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
	})

	authMiddleware := middlewares.NewAuthMiddleware("skymail", cfg.KeycloakRealmURL)

	templateHandler := handlers.NewTemplateHandler(db)
	listHandler := handlers.NewListHandler(db)
	mailHandler := handlers.NewMailHandler(db, mailerService)

	app := fiber.New(fiber.Config{
		StructValidator:    vld,
		JSONDecoder:        sonic.Unmarshal,
		JSONEncoder:        sonic.Marshal,
		ErrorHandler:       errorHandler,
		ProxyHeader:        fiber.HeaderXForwardedFor,
		EnableIPValidation: true,
	})

	app.Use(recover.New())

	app.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowHeaders:     []string{"*", "Authorization", "Retry-After"},
		ExposeHeaders:    []string{"X-Total-Count"},
		AllowCredentials: false,
	}))

	app.Use(jwtware.New(jwtware.Config{
		JWKSetURLs: []string{cfg.KeycloakRealmURL + "/protocol/openid-connect/certs"},
	}))
	app.Use(authMiddleware.GetRoles)
	app.Use(authMiddleware.RequireAnyPermission("skymail:access"))

	api := app.Group("/v1")

	templates := api.Group("/templates")
	templates.Post("/", authMiddleware.RequireAnyPermission("skymail:templates:write"), templateHandler.CreateTemplate)
	templates.Get("/", authMiddleware.RequireAnyPermission("skymail:templates:read"), templateHandler.GetTemplates)
	templates.Get("/:id", authMiddleware.RequireAnyPermission("skymail:templates:read"), templateHandler.GetTemplate)
	templates.Patch("/:id", authMiddleware.RequireAnyPermission("skymail:templates:write"), templateHandler.UpdateTemplate)
	templates.Delete("/:id", authMiddleware.RequireAnyPermission("skymail:templates:write"), templateHandler.DeleteTemplate)

	lists := api.Group("/mailing_lists")
	lists.Post("/", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.CreateList)
	lists.Get("/", authMiddleware.RequireAnyPermission("skymail:lists:read"), listHandler.GetLists)
	lists.Get("/:id", authMiddleware.RequireAnyPermission("skymail:lists:read"), listHandler.GetList)
	lists.Patch("/:id", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.UpdateList)
	lists.Delete("/:id", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.DeleteList)
	lists.Post("/:id/recipients", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.AddRecipient)
	lists.Get("/:id/recipients", authMiddleware.RequireAnyPermission("skymail:lists:read"), listHandler.GetRecipients)
	lists.Delete("/:id/recipients/:recipientId", authMiddleware.RequireAnyPermission("skymail:lists:write"), listHandler.RemoveRecipient)

	tasks := api.Group("/mail_tasks")
	tasks.Post("/", authMiddleware.RequireAnyPermission("skymail:mails:write"), mailHandler.CreateTask)
	tasks.Get("/", authMiddleware.RequireAnyPermission("skymail:mails:read"), mailHandler.GetTasks)
	tasks.Get("/:id", authMiddleware.RequireAnyPermission("skymail:mails:read"), mailHandler.GetTask)
	tasks.Get("/:id/queue", authMiddleware.RequireAnyPermission("skymail:mails:read"), mailHandler.GetTaskQueueItems)

	mailerService.Start(ctx, 3)

	if err = app.Listen(":3000"); err != nil {
		log.Fatal().Err(err).Msg("error starting server")
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
		log.Info().
			Str("method", ctx.Method()).
			Str("path", ctx.Path()).
			Str("ip", ctx.IP()).
			Str("ua", ctx.Get(fiber.HeaderUserAgent)).
			Err(err).
			Msg("Resource not found in database (404)")
		return ctx.Status(apperrors.ErrStatusNotFound.Status).JSON(apperrors.ErrStatusNotFound)
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
