package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

type lifecycleFilter string

const (
	lifecycleCurrent  lifecycleFilter = "current"
	lifecycleInactive lifecycleFilter = "inactive"
	lifecycleAll      lifecycleFilter = "all"
)

func parseLifecycleFilter(raw string) (lifecycleFilter, error) {
	switch lifecycleFilter(strings.ToLower(strings.TrimSpace(raw))) {
	case "", lifecycleCurrent:
		return lifecycleCurrent, nil
	case lifecycleInactive:
		return lifecycleInactive, nil
	case lifecycleAll:
		return lifecycleAll, nil
	default:
		return "", apperrors.ErrValidation.WithParams(map[string]interface{}{
			"lifecycle": "must be one of current, inactive, all",
		})
	}
}

// localText reads a string the middleware left on the request — the caller's
// subject, their name — or nil when it is missing or blank.
func localText(c fiber.Ctx, key string) *string {
	text, ok := c.Locals(key).(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil
	}
	return &text
}
