package apperrors

import (
	"errors"
	"fmt"

	"github.com/gofiber/fiber/v3"
)

type AppError struct {
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Params  map[string]interface{} `json:"params,omitempty"`
	Status  int                    `json:"-"`
}

func (e *AppError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func New(code string, message string, status int) *AppError {
	return &AppError{
		Code:    code,
		Message: message,
		Status:  status,
	}
}

func (e *AppError) WithParams(params map[string]interface{}) *AppError {
	newErr := *e
	newErr.Params = params
	return &newErr
}

func (e *AppError) Is(target error) bool {
	var t *AppError
	ok := errors.As(target, &t)
	if !ok {
		return false
	}

	return e.Code == t.Code
}

func FromFiberError(f *fiber.Error) *AppError {
	var err AppError

	switch f.Code {
	case fiber.StatusNotFound:
		err = *ErrStatusNotFound
	case fiber.StatusMethodNotAllowed:
		err = *ErrStatusMethodNotAllowed
	case fiber.StatusUnprocessableEntity:
		err = *ErrValidation
	case fiber.StatusTooManyRequests:
		err = *ErrStatusTooManyRequests
	default:
		err = *ErrUnknownError
		// Keep the status Fiber chose (431 header too large, 413 body too
		// large, 400 malformed request): it is the client's error, not ours.
		if f.Code >= 400 && f.Code < 600 {
			err.Status = f.Code
		}
	}

	return err.WithParams(map[string]interface{}{
		"original_code":    f.Code,
		"original_message": f.Message,
	})
}
