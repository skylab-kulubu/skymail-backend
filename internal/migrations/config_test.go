package migrations_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
)

func TestConfigDefaultsToOff(t *testing.T) {
	t.Parallel()

	config, err := migrations.ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != migrations.ModeOff || config.BaselineVersion != 0 {
		t.Fatalf("config = %+v", config)
	}
}

func TestApplyConfigAcceptsOptionalBaseline(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"DATABASE_MIGRATIONS_MODE":             "apply",
		"DATABASE_MIGRATIONS_BASELINE_VERSION": "20260919180000",
	}
	config, err := migrations.ConfigFromEnv(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != migrations.ModeApply || config.BaselineVersion != 20260919180000 {
		t.Fatalf("config = %+v", config)
	}
}

func TestConfigRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		env  map[string]string
	}{
		{name: "unknown mode", env: map[string]string{"DATABASE_MIGRATIONS_MODE": "force"}},
		{name: "invalid baseline", env: map[string]string{
			"DATABASE_MIGRATIONS_MODE":             "apply",
			"DATABASE_MIGRATIONS_BASELINE_VERSION": "latest",
		}},
		{name: "zero baseline", env: map[string]string{
			"DATABASE_MIGRATIONS_MODE":             "apply",
			"DATABASE_MIGRATIONS_BASELINE_VERSION": "0",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := migrations.ConfigFromEnv(func(key string) string { return test.env[key] })
			if err == nil {
				t.Fatal("configuration unexpectedly accepted")
			}
			if strings.Contains(strings.ToLower(err.Error()), "password") {
				t.Fatal("configuration error exposed a secret")
			}
		})
	}
}
