package config

import (
	"errors"

	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
	"github.com/spf13/viper"
)

type Config struct {
	DatabaseURL string `mapstructure:"DATABASE_URL" validate:"required"`
	SMTPFrom    string `mapstructure:"SMTP_FROM" validate:"required"`
	SMTPHost    string `mapstructure:"SMTP_HOST" validate:"required"`
	SMTPPort    int    `mapstructure:"SMTP_PORT" validate:"required"`
	SMTPUser    string `mapstructure:"SMTP_USER" validate:"required"`
	SMTPPass    string `mapstructure:"SMTP_PASS" validate:"required"`
}

func LoadConfig(vld validator.StructValidator) (config Config, err error) {
	viper.AddConfigPath(".")
	viper.SetConfigName(".env")
	viper.SetConfigType("env")

	var notFoundError viper.ConfigFileNotFoundError

	if err := viper.ReadInConfig(); err != nil {
		if !errors.Is(err, &notFoundError) {
			return config, err
		}
	}

	viper.SetEnvPrefix("SKYMAIL")
	viper.AutomaticEnv()

	err = viper.Unmarshal(&config)
	if err != nil {
		return config, err
	}

	if err = vld.Validate(&config); err != nil {
		return config, err
	}

	return config, err
}
