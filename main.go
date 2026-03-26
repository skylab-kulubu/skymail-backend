package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bytedance/sonic"
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

	templateHandler := handlers.NewTemplateHandler(db)
	listHandler := handlers.NewListHandler(db)

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

	api := app.Group("/v1")

	templates := api.Group("/templates")
	templates.Post("/", templateHandler.CreateTemplate)
	templates.Get("/", templateHandler.GetTemplates)
	templates.Get("/:id", templateHandler.GetTemplate)
	templates.Patch("/:id", templateHandler.UpdateTemplate)
	templates.Delete("/:id", templateHandler.DeleteTemplate)

	lists := api.Group("/mailing_lists")
	lists.Post("/", listHandler.CreateList)
	lists.Get("/", listHandler.GetLists)
	lists.Get("/:id", listHandler.GetList)
	lists.Patch("/:id", listHandler.UpdateList)
	lists.Delete("/:id", listHandler.DeleteList)
	lists.Post("/:id/recipients", listHandler.AddRecipient)
	lists.Get("/:id/recipients", listHandler.GetRecipients)
	lists.Delete("/:id/recipients/:recipientId", listHandler.RemoveRecipient)

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
