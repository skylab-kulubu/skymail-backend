package apperrors

import "github.com/gofiber/fiber/v3"

var (
	ErrForbidden              = New("server.forbidden", "You do not have permission to access this resource.", fiber.StatusForbidden)
	ErrStatusNotFound         = New("server.not_found", "The requested resource was not found.", fiber.StatusNotFound)
	ErrStatusMethodNotAllowed = New("server.method_not_allowed", "The requested HTTP method is not allowed for this resource.", fiber.StatusMethodNotAllowed)
	ErrStatusTooManyRequests  = New("server.too_many_requests", "Too many requests have been made in a short period of time.", fiber.StatusTooManyRequests)
	ErrStatusInternalServer   = New("server.internal_server_error", "An internal server error occurred.", fiber.StatusInternalServerError)
	ErrUnknownError           = New("server.unknown_error", "An unknown error occurred.", fiber.StatusInternalServerError)
)

var (
	ErrValidation = New("validation.error", "One or more validation errors occurred.", fiber.StatusBadRequest)
)
