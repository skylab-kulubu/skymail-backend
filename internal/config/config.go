package config

import (
	"errors"
	"reflect"

	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
	"github.com/spf13/viper"
)

type Config struct {
	DatabaseURL      string `mapstructure:"DATABASE_URL" validate:"required"`
	SMTPFrom         string `mapstructure:"SMTP_FROM" validate:"required"`
	SMTPHost         string `mapstructure:"SMTP_HOST" validate:"required"`
	SMTPPort         int    `mapstructure:"SMTP_PORT" validate:"required"`
	SMTPUser         string `mapstructure:"SMTP_USER" validate:"required"`
	SMTPPass         string `mapstructure:"SMTP_PASS" validate:"required"`
	SMTPFQDN         string `mapstructure:"SMTP_FQDN" validate:"required"`
	KeycloakRealmURL string `mapstructure:"KEYCLOAK_REALM_URL" validate:"required"`
	AppSecret        string `mapstructure:"APP_SECRET" validate:"required"`
	EurekaServer     string `mapstructure:"EUREKA_SERVER"`
	AppName          string `mapstructure:"APP_NAME"`
	AppPort          int    `mapstructure:"APP_PORT"`
}

func LoadConfig(vld validator.StructValidator) (config Config, err error) {
	viper.AddConfigPath(".")
	viper.SetConfigName(".env")
	viper.SetConfigType("env")

	if err := viper.ReadInConfig(); err != nil {
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			return config, err
		}
	}

	viper.AutomaticEnv()

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

	return config, err
}

func bindEnvs(t reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if tagValue, ok := field.Tag.Lookup("mapstructure"); ok {
			viper.BindEnv(tagValue)
		}
	}
}
