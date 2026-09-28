package config

import (
	"errors"
	"reflect"

	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
	"github.com/spf13/viper"
)

// DefaultKeycloakClientID is the client SkyMail's roles are on when
// KEYCLOAK_CLIENT_ID does not say.
const DefaultKeycloakClientID = "skymail"

type Config struct {
	DatabaseURL      string `mapstructure:"DATABASE_URL" validate:"required"`
	SMTPFrom         string `mapstructure:"SMTP_FROM" validate:"required"`
	SMTPHost         string `mapstructure:"SMTP_HOST" validate:"required"`
	SMTPPort         int    `mapstructure:"SMTP_PORT" validate:"required"`
	SMTPUser         string `mapstructure:"SMTP_USER" validate:"required"`
	SMTPPass         string `mapstructure:"SMTP_PASS" validate:"required"`
	SMTPFQDN         string `mapstructure:"SMTP_FQDN" validate:"required"`
	SMTPPlain        bool   `mapstructure:"SMTP_PLAIN"`
	KeycloakRealmURL string `mapstructure:"KEYCLOAK_REALM_URL" validate:"required"`
	// The Keycloak client whose roles SkyMail's permissions are: skymail when unset.
	KeycloakClientID            string `mapstructure:"KEYCLOAK_CLIENT_ID"`
	KeycloakServiceClientID     string `mapstructure:"KEYCLOAK_SERVICE_CLIENT_ID" validate:"required"`
	KeycloakServiceClientSecret string `mapstructure:"KEYCLOAK_SERVICE_CLIENT_SECRET" validate:"required"`
	AppPort                     int    `mapstructure:"APP_PORT"`
}

func LoadConfig(vld validator.StructValidator) (config Config, err error) {
	if err = LoadEnv(); err != nil {
		return config, err
	}

	// Automatically bind environment variables for all fields in the struct.
	// This is required for Unmarshal to work when the .env file is missing.
	bindEnvs(reflect.TypeOf(config))

	err = viper.Unmarshal(&config)
	if err != nil {
		return config, err
	}

	if err = vld.Validate(&config); err != nil {
		return config, err
	}
	if config.KeycloakClientID == "" {
		config.KeycloakClientID = DefaultKeycloakClientID
	}

	return config, err
}

// LoadEnv sets up the source Value reads, and LoadConfig with it: the
// environment, over a .env in the working directory when there is one. A
// maintenance command run in the server's container calls it alone, so it
// reads what the server reads without needing the server's whole Config.
func LoadEnv() error {
	viper.AddConfigPath(".")
	viper.SetConfigName(".env")
	viper.SetConfigType("env")

	if err := viper.ReadInConfig(); err != nil {
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			return err
		}
	}

	viper.AutomaticEnv()
	return nil
}

func bindEnvs(t reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if tagValue, ok := field.Tag.Lookup("mapstructure"); ok {
			viper.BindEnv(tagValue)
		}
	}
}

// Value returns a feature-specific value from the same environment/.env source
// initialized by LoadConfig. It lets strict optional feature parsers preserve
// the application's existing local .env behavior.
func Value(key string) string {
	return viper.GetString(key)
}
