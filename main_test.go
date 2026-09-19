package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorHandlerMapsUniqueConflict(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	app.Post("/restore", func(fiber.Ctx) error {
		return &pgconn.PgError{Code: "23505", Message: "unique violation"}
	})

	response, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/restore", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
}
