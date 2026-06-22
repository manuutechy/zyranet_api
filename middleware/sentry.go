package middleware

import (
	"fmt"
	"log"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
)

// InitSentry initialises error reporting. With no DSN configured (e.g. local
// dev) this is a deliberate no-op — every sentry-go call below becomes a
// cheap no-op too, so the rest of the app never has to check "is Sentry on?".
func InitSentry() {
	if config.Config.SentryDSN == "" {
		log.Println("[sentry] No SENTRY_DSN configured, error reporting disabled")
		return
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              config.Config.SentryDSN,
		Environment:      config.Config.AppEnv,
		AttachStacktrace: true,
	})
	if err != nil {
		log.Printf("[sentry] Failed to initialise: %v", err)
		return
	}
	log.Println("[sentry] Error reporting enabled")
}

// FlushSentry waits briefly for any buffered events to be sent. Call this on shutdown.
func FlushSentry() {
	sentry.Flush(2 * time.Second)
}

// SentryMiddleware gives every request its own Sentry hub (so concurrent
// requests don't leak tags/context into each other) and reports any error
// returned by a handler, or any 5xx response, with request context attached.
// Register this BEFORE recover.New() — the recover middleware converts a
// panic into a returned error, which then flows back through here too, so
// panics get reported without any extra wiring.
func SentryMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		hub := sentry.CurrentHub().Clone()
		hub.Scope().SetTags(map[string]string{
			"method": c.Method(),
			"path":   c.Path(),
		})

		err := c.Next()

		status := c.Response().StatusCode()
		if err != nil {
			hub.CaptureException(err)
		} else if status >= fiber.StatusInternalServerError {
			hub.CaptureMessage(fmt.Sprintf("HTTP %d on %s %s", status, c.Method(), c.Path()))
		}
		return err
	}
}
