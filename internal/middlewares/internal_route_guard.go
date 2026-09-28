package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
)

// InternalRouteGuard keeps the routes under /internal to callers on the
// Docker network (ADR-0016). Traefik adds forwarding headers to everything it
// passes on, so a request that carries one came through the public ingress
// and gets a bare 404 before any token is read. The token check behind it
// stays the real boundary; an internal caller must not set these headers.
func InternalRouteGuard() fiber.Handler {
	return func(c fiber.Ctx) error {
		if cameThroughIngress(c) {
			c.Status(fiber.StatusNotFound)
			return nil
		}
		return c.Next()
	}
}

func cameThroughIngress(c fiber.Ctx) bool {
	for name := range c.GetReqHeaders() {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "x-forwarded-") || name == "forwarded" || name == "x-real-ip" {
			return true
		}
	}
	return false
}
