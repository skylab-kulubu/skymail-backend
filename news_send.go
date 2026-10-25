package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/news"
)

// NewsCMSURLEnv is inscribed's address for a News send's read, without the
// /cms path (http://inscribed:8080 on the Docker network, or the public
// https://api.yildizskylab.com/api/cms's base https://api.yildizskylab.com/api).
const NewsCMSURLEnv = "SKYMAIL_NEWS_CMS_URL"

// NewsImageHostsEnv names the hosts a News record's cover may come from,
// comma-separated; each also allows its subdomains. Unset: yildizskylab.com.
const NewsImageHostsEnv = "SKYMAIL_NEWS_IMAGE_HOSTS"

// newsSendConfig is what a News send needs (README, "Haberden gönderim").
type newsSendConfig struct {
	Options handlers.NewsSendOptions
	CMSURL  string
}

// newsSendConfigFromEnv reads SKYMAIL_NEWS_SEND, SKYMAIL_NEWS_READ_MORE,
// SKYMAIL_NEWS_CMS_URL and SKYMAIL_NEWS_IMAGE_HOSTS. On needs the CMS's
// address, coreURL (CORE_URL), SKYMAIL_LIST_UNSUBSCRIBE and
// SKYMAIL_MAIL_PURPOSE on (lanes): a News mail never goes without its
// one-click unsubscribe header, nor a send without its purpose, category and
// the sender's statement. Without any of them startup stops, as it does for
// a value other than the ones named.
func newsSendConfigFromEnv(getenv func(string) string, coreURL string, lanes mailer.SendLanes) (newsSendConfig, error) {
	var cfg newsSendConfig
	switch raw := strings.ToLower(strings.TrimSpace(getenv(handlers.NewsSendEnv))); raw {
	case "", "off":
	case "on":
		cfg.Options.Enabled = true
	default:
		return cfg, fmt.Errorf("%s must be off or on, not %q", handlers.NewsSendEnv, raw)
	}
	switch raw := strings.ToLower(strings.TrimSpace(getenv(handlers.NewsReadMoreEnv))); raw {
	case "", news.ReadMoreHome:
		cfg.Options.ReadMore = news.ReadMoreHome
	case news.ReadMorePages:
		cfg.Options.ReadMore = news.ReadMorePages
	default:
		return cfg, fmt.Errorf("%s must be home or pages, not %q", handlers.NewsReadMoreEnv, raw)
	}
	cfg.CMSURL = strings.TrimRight(strings.TrimSpace(getenv(NewsCMSURLEnv)), "/")
	if cfg.CMSURL != "" {
		parsed, err := url.Parse(cfg.CMSURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
			return cfg, fmt.Errorf("%s must be an http or https address without a query", NewsCMSURLEnv)
		}
	}
	cfg.Options.ImageHosts = news.DefaultImageHosts
	if raw := strings.TrimSpace(getenv(NewsImageHostsEnv)); raw != "" {
		cfg.Options.ImageHosts = nil
		for _, host := range strings.Split(raw, ",") {
			host = strings.ToLower(strings.TrimSpace(host))
			if host == "" || strings.ContainsAny(host, "/:@ ") || !strings.Contains(host, ".") {
				return cfg, fmt.Errorf("%s must be host names separated by commas, such as yildizskylab.com", NewsImageHostsEnv)
			}
			cfg.Options.ImageHosts = append(cfg.Options.ImageHosts, host)
		}
	}
	if !cfg.Options.Enabled {
		return cfg, nil
	}
	if cfg.CMSURL == "" {
		return cfg, fmt.Errorf("%s=on needs %s, inscribed's address", handlers.NewsSendEnv, NewsCMSURLEnv)
	}
	if strings.TrimSpace(coreURL) == "" {
		return cfg, errors.New(handlers.NewsSendEnv + "=on needs CORE_URL, core's address on the Docker network (e.g. http://core-backend:8080)")
	}
	if !lanes.ListUnsubscribe {
		return cfg, errors.New(handlers.NewsSendEnv + "=on needs SKYMAIL_LIST_UNSUBSCRIBE=on: a News mail goes with its one-click unsubscribe header (List-Unsubscribe, RFC 8058)")
	}
	if !lanes.Purpose {
		return cfg, errors.New(handlers.NewsSendEnv + "=on needs SKYMAIL_MAIL_PURPOSE=on: a News send is written with its purpose, category and commercial statement")
	}
	return cfg, nil
}

func logNewsSend(cfg newsSendConfig) {
	log.Info().Bool("on", cfg.Options.Enabled).Str("read_more", cfg.Options.ReadMore).Str("cms_url", cfg.CMSURL).
		Strs("image_hosts", cfg.Options.ImageHosts).
		Msg("News sends (SKYMAIL_NEWS_SEND, SKYMAIL_NEWS_READ_MORE, SKYMAIL_NEWS_CMS_URL, SKYMAIL_NEWS_IMAGE_HOSTS)")
}

// registerNewsSendRoutes serves POST /v1/news_sends (hub ticket duyuru-haber
// 06): a person with skymail:announcements:send mails a published News
// record directly.
func registerNewsSendRoutes(api fiber.Router, auth middlewares.AuthMiddleware, sends handlers.NewsSendHandler) {
	api.Post("/news_sends", auth.RequireAnyPermission(handlers.AnnouncementsSendRole), sends.Send)
}
