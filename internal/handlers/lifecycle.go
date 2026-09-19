package handlers

import (
	"strings"

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

func lifecycleActor(raw interface{}) *string {
	actor, ok := raw.(string)
	if !ok || strings.TrimSpace(actor) == "" {
		return nil
	}
	return &actor
}
