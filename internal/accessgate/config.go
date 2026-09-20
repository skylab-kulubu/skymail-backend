package accessgate

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Mode string

const (
	ModeOff     Mode = "off"
	ModeEnforce Mode = "enforce"
)

type Config struct {
	Mode             Mode
	Address          string
	Username         string
	Password         string
	DB               int
	TLSServerName    string
	TLSCertFile      string
	TLSKeyFile       string
	CACertFile       string
	OperationTimeout time.Duration
}

func ConfigFromEnv(getenv func(string) string, keycloakRealmURL string) (Config, error) {
	mode := Mode(strings.TrimSpace(getenv("ACCOUNT_ACCESS_GATE_MODE")))
	if mode == "" {
		mode = ModeOff
	}
	config := Config{Mode: mode}
	if mode == ModeOff {
		return config, nil
	}
	if mode != ModeEnforce {
		return Config{}, errors.New("ACCOUNT_ACCESS_GATE_MODE must be off or enforce")
	}
	if keycloakRealmURL != Issuer {
		return Config{}, errors.New("account access gate requires the exact v1 issuer")
	}

	required := []struct {
		key  string
		dest *string
	}{
		{"ACCOUNT_ACCESS_REDIS_ADDR", &config.Address},
		{"ACCOUNT_ACCESS_REDIS_USERNAME", &config.Username},
		{"ACCOUNT_ACCESS_REDIS_PASSWORD", &config.Password},
		{"ACCOUNT_ACCESS_REDIS_TLS_SERVER_NAME", &config.TLSServerName},
		{"ACCOUNT_ACCESS_REDIS_TLS_CERT_FILE", &config.TLSCertFile},
		{"ACCOUNT_ACCESS_REDIS_TLS_KEY_FILE", &config.TLSKeyFile},
		{"ACCOUNT_ACCESS_REDIS_CA_CERT_FILE", &config.CACertFile},
	}
	for _, item := range required {
		*item.dest = strings.TrimSpace(getenv(item.key))
		if *item.dest == "" {
			return Config{}, fmt.Errorf("%s is required in enforce mode", item.key)
		}
	}
	if strings.TrimSpace(getenv("ACCOUNT_ACCESS_REDIS_TLS")) != "true" {
		return Config{}, errors.New("ACCOUNT_ACCESS_REDIS_TLS must be true in enforce mode")
	}

	var err error
	config.DB, err = requiredNonNegativeInt(getenv, "ACCOUNT_ACCESS_REDIS_DB")
	if err != nil {
		return Config{}, err
	}
	config.OperationTimeout, err = requiredPositiveDuration(getenv, "ACCOUNT_ACCESS_REDIS_OPERATION_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	if config.OperationTimeout < 50*time.Millisecond || config.OperationTimeout > 2*time.Second {
		return Config{}, errors.New("ACCOUNT_ACCESS_REDIS_OPERATION_TIMEOUT must be between 50ms and 2s")
	}
	return config, nil
}

func NewRedisClient(config Config) (*redis.Client, error) {
	if config.Mode != ModeEnforce {
		return nil, errors.New("account access Redis client requires enforce mode")
	}

	clientCertificate, err := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load account access Redis client certificate: %w", err)
	}
	pem, err := os.ReadFile(config.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("read account access Redis CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("account access Redis CA contains no certificates")
	}

	return redis.NewClient(&redis.Options{
		Addr:     config.Address,
		Username: config.Username,
		Password: config.Password,
		DB:       config.DB,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			ServerName:   config.TLSServerName,
			Certificates: []tls.Certificate{clientCertificate},
			RootCAs:      roots,
		},
		DialTimeout:           config.OperationTimeout,
		ReadTimeout:           config.OperationTimeout,
		WriteTimeout:          config.OperationTimeout,
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	}), nil
}

func requiredNonNegativeInt(getenv func(string) string, key string) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return 0, fmt.Errorf("%s is required in enforce mode", key)
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return value, nil
}

func requiredPositiveDuration(getenv func(string) string, key string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return 0, fmt.Errorf("%s is required in enforce mode", key)
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}
