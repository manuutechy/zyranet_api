package handlers

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// cyzoraKeys are the platform's Cyzora credentials in the settings table.
// Like the shared Daraja keys they are platform-only (see platformOnlyKeys):
// never readable or writable through an ISP's /settings.
var cyzoraKeys = []string{services.CyzoraSecretKeySetting, services.CyzoraWebhookSecretSetting}

// cyzoraSvc is the Cyzora client the platform "test key" button uses. Tests
// point it at a fake.
var cyzoraSvc = services.NewCyzoraService()

func cyzoraStatus() fiber.Map {
	key := services.CyzoraSecretKey()
	return fiber.Map{
		"secret_key_set":     key != "",
		"webhook_secret_set": services.CyzoraWebhookSecret() != "",
		"mode":               services.CyzoraKeyMode(key), // live | test | ""
		"webhook_url":        services.CyzoraWebhookURL,
	}
}

// PlatformCyzoraShow — GET /platform/cyzora. Secrets are never returned, only
// whether they are set.
func PlatformCyzoraShow(c *fiber.Ctx) error {
	return utils.SuccessResponse(c, cyzoraStatus(), "")
}

// PlatformCyzoraUpdate — POST /platform/cyzora (platform owner). A blank or
// missing field keeps the stored value.
func PlatformCyzoraUpdate(c *fiber.Ctx) error {
	var body struct {
		SecretKey     string `json:"secret_key"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	updates := map[string]string{}
	if k := strings.TrimSpace(body.SecretKey); k != "" {
		if !services.ValidCyzoraSecretKey(k) {
			return utils.ErrorResponse(c, "That doesn't look like a Cyzora secret key (it starts with sk_live_ or sk_test_).", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
		updates[services.CyzoraSecretKeySetting] = k
	}
	if w := strings.TrimSpace(body.WebhookSecret); w != "" {
		if len(w) < 16 {
			return utils.ErrorResponse(c, "The webhook secret looks too short. Copy it from the Cyzora dashboard (Webhooks).", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
		updates[services.CyzoraWebhookSecretSetting] = w
	}
	if len(updates) > 0 {
		UpsertSettings(updates)
	}
	return utils.SuccessResponse(c, cyzoraStatus(), "Cyzora settings saved.")
}

// PlatformCyzoraTest — POST /platform/cyzora/test. Checks the saved key with a
// read-only call; no prompt is sent and no money moves.
func PlatformCyzoraTest(c *fiber.Ctx) error {
	ok, msg := cyzoraSvc.TestKey()
	return utils.SuccessResponse(c, fiber.Map{"ok": ok, "message": msg}, msg)
}

// CyzoraWebhook — POST /payments/cyzora/webhook (public). Cyzora signs the raw
// body with the webhook secret (X-Cyzora-Signature, hex HMAC-SHA256); nothing
// is read before that signature checks out. Charge events complete or fail
// the payment through the same code a Daraja callback uses. Repeated
// deliveries are no-ops. Unknown charges are acknowledged (200) and logged so
// Cyzora stops retrying; only a database error answers 500 (Cyzora retries).
func CyzoraWebhook(c *fiber.Ctx) error {
	raw := c.Body()
	secret := services.CyzoraWebhookSecret()
	if secret == "" {
		log.Printf("[cyzora] webhook rejected: no webhook secret configured")
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "error": "Unauthorized"})
	}
	if !services.VerifyCyzoraSignature(raw, c.Get("X-Cyzora-Signature"), secret) {
		log.Printf("[cyzora] webhook rejected: bad or missing signature from %s", c.IP())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "error": "Unauthorized"})
	}

	var evt services.CyzoraWebhookEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		log.Printf("[cyzora] webhook: unreadable body: %v", err)
		return c.JSON(fiber.Map{"success": true, "note": "unreadable body"})
	}
	if evt.Event == "" {
		evt.Event = c.Get("X-Cyzora-Event")
	}
	if mpesaSvcGlobal == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "error": "Not ready"})
	}
	note, err := mpesaSvcGlobal.HandleCyzoraWebhook(evt)
	if err != nil {
		log.Printf("[cyzora] webhook %s for charge %s failed: %v", evt.Event, evt.ChargeID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": "Try again"})
	}
	resp := fiber.Map{"success": true}
	if note != "" {
		resp["note"] = note
	}
	return c.JSON(resp)
}
