package discovery

import (
	"github.com/rs/zerolog/log"
	eureka "github.com/xuanbo/eureka-client"
)

type EurekaClient struct {
	client *eureka.Client
}

func NewEurekaClient(server, appName string, port int) *EurekaClient {
	if server == "" {
		return nil
	}

	if appName == "" {
		appName = "skymail"
	}

	if port == 0 {
		port = 3000
	}

	client := eureka.NewClient(&eureka.Config{
		DefaultZone:           server,
		App:                   appName,
		Port:                  port,
		RetryIntervalInSecs:   15,
		RenewalIntervalInSecs: 30,
		DurationInSecs:        90,
	})

	return &EurekaClient{
		client: client,
	}
}

func (e *EurekaClient) Start() {
	if e == nil || e.client == nil {
		return
	}

	log.Info().Msg("starting eureka client")
	e.client.Start()
}
