// Package migrations applies the schema version bundled with the application.
package migrations

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Mode string

const (
	ModeOff   Mode = "off"
	ModeApply Mode = "apply"
)

type Config struct {
	Mode            Mode
	BaselineVersion uint
}

func ConfigFromEnv(getenv func(string) string) (Config, error) {
	mode := Mode(strings.TrimSpace(getenv("DATABASE_MIGRATIONS_MODE")))
	if mode == "" {
		mode = ModeOff
	}
	config := Config{Mode: mode}
	if mode == ModeOff {
		return config, nil
	}
	if mode != ModeApply {
		return Config{}, errors.New("DATABASE_MIGRATIONS_MODE must be off or apply")
	}

	rawBaseline := strings.TrimSpace(getenv("DATABASE_MIGRATIONS_BASELINE_VERSION"))
	if rawBaseline == "" {
		return config, nil
	}
	baseline, err := strconv.ParseUint(rawBaseline, 10, 0)
	if err != nil || baseline == 0 {
		return Config{}, fmt.Errorf("DATABASE_MIGRATIONS_BASELINE_VERSION must be a positive migration version")
	}
	config.BaselineVersion = uint(baseline)
	return config, nil
}
