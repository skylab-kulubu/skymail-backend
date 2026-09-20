package accessgate_test

import (
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
)

func TestEnforceConfigurationRequiresExactIssuerAndExplicitTLSAuthentication(t *testing.T) {
	t.Parallel()

	valid := map[string]string{
		"ACCOUNT_ACCESS_GATE_MODE":               "enforce",
		"ACCOUNT_ACCESS_REDIS_ADDR":              "access-gate.internal:6379",
		"ACCOUNT_ACCESS_REDIS_USERNAME":          "skymail-reader",
		"ACCOUNT_ACCESS_REDIS_PASSWORD":          "secret",
		"ACCOUNT_ACCESS_REDIS_DB":                "0",
		"ACCOUNT_ACCESS_REDIS_TLS":               "true",
		"ACCOUNT_ACCESS_REDIS_TLS_SERVER_NAME":   "access-gate.internal",
		"ACCOUNT_ACCESS_REDIS_TLS_CERT_FILE":     "/run/secrets/skymail-reader.crt",
		"ACCOUNT_ACCESS_REDIS_TLS_KEY_FILE":      "/run/secrets/skymail-reader.key",
		"ACCOUNT_ACCESS_REDIS_CA_CERT_FILE":      "/run/secrets/ca.crt",
		"ACCOUNT_ACCESS_REDIS_OPERATION_TIMEOUT": "200ms",
	}
	getenv := func(key string) string { return valid[key] }

	config, err := accessgate.ConfigFromEnv(getenv, accessgate.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != accessgate.ModeEnforce || config.DB != 0 || config.OperationTimeout != 200*time.Millisecond {
		t.Fatalf("config = %+v", config)
	}

	for _, test := range []struct {
		name   string
		issuer string
		key    string
		value  string
	}{
		{name: "wrong issuer", issuer: accessgate.Issuer + "/"},
		{name: "missing address", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_ADDR"},
		{name: "missing username", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_USERNAME"},
		{name: "missing password", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_PASSWORD"},
		{name: "missing db", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_DB"},
		{name: "plaintext transport", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_TLS", value: "false"},
		{name: "missing server name", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_TLS_SERVER_NAME"},
		{name: "missing client certificate", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_TLS_CERT_FILE"},
		{name: "missing client key", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_TLS_KEY_FILE"},
		{name: "missing CA certificate", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_CA_CERT_FILE"},
		{name: "missing timeout", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_OPERATION_TIMEOUT"},
		{name: "timeout too short", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_OPERATION_TIMEOUT", value: "49ms"},
		{name: "timeout too long", issuer: accessgate.Issuer, key: "ACCOUNT_ACCESS_REDIS_OPERATION_TIMEOUT", value: "2001ms"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := make(map[string]string, len(valid))
			for key, value := range valid {
				env[key] = value
			}
			if test.key != "" {
				env[test.key] = test.value
			}
			_, err := accessgate.ConfigFromEnv(func(key string) string { return env[key] }, test.issuer)
			if err == nil {
				t.Fatal("configuration unexpectedly accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration error exposed a secret")
			}
		})
	}
}

func TestGateDefaultsOffWithoutRedisConfiguration(t *testing.T) {
	t.Parallel()

	config, err := accessgate.ConfigFromEnv(func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != accessgate.ModeOff {
		t.Fatalf("mode = %q", config.Mode)
	}
}

func TestGateRejectsUnknownMode(t *testing.T) {
	t.Parallel()

	_, err := accessgate.ConfigFromEnv(func(key string) string {
		if key == "ACCOUNT_ACCESS_GATE_MODE" {
			return "observe"
		}
		return ""
	}, accessgate.Issuer)
	if err == nil {
		t.Fatal("unknown mode unexpectedly accepted")
	}
}
