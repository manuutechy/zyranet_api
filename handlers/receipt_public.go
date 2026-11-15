package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// esc HTML-escapes a value for the invoice/receipt pages. Customer names
// come from the captive portal, so unescaped they were stored XSS on the
// API's own origin.
func esc(s string) string { return html.EscapeString(s) }

var cssColorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{3,8}$`)

// cssColor lets only a plain #hex colour into a style block.
func cssColor(s string) string {
	if cssColorRe.MatchString(strings.TrimSpace(s)) {
		return strings.TrimSpace(s)
	}
	return "#C2410C"
}

// receiptKey signs public receipt links. RECEIPT_LINK_SECRET if set, else
// derived from the JWT secret (rotating that one invalidates old links).
func receiptKey() []byte {
	if k := strings.TrimSpace(os.Getenv("RECEIPT_LINK_SECRET")); k != "" {
		return []byte(k)
	}
	return []byte("zyra-receipt-link|" + config.Config.JWTSecret)
}

func receiptMAC(paymentID uint) string {
	m := hmac.New(sha256.New, receiptKey())
	fmt.Fprintf(m, "receipt:%d", paymentID)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:24] // 144 bits
}

// ReceiptToken is the unguessable token for a payment's public receipt
// link: "<id>-<HMAC>". Knowing one payment id (they are sequential) is not
// enough to open it.
func ReceiptToken(paymentID uint) string {
	return strconv.FormatUint(uint64(paymentID), 10) + "-" + receiptMAC(paymentID)
}

// paymentFromReceiptToken checks a token and returns its payment id.
func paymentFromReceiptToken(tok string) (uint, bool) {
	idPart, sig, ok := strings.Cut(strings.TrimSpace(tok), "-")
	if !ok || len(sig) != 24 {
		return 0, false
	}
	id, err := strconv.ParseUint(idPart, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	if !hmac.Equal([]byte(sig), []byte(receiptMAC(uint(id)))) {
		return 0, false
	}
	return uint(id), true
}

func publicReceiptURL(c *fiber.Ctx, paymentID uint) string {
	return c.BaseURL() + "/api/v1/public/receipts/" + ReceiptToken(paymentID)
}

// PaymentReceiptPublic — GET /public/receipts/:token. The customer-facing
// receipt behind the SMS link: read-only, no login, only for completed
// payments, and only what a paper receipt shows (ISP, receipt number, date,
// plan, amount, how it was paid, M-Pesa code, the phone masked). No name,
// account number, router zone or staff details.
func PaymentReceiptPublic(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	c.Set("X-Robots-Tag", "noindex, nofollow")
	c.Set("Referrer-Policy", "no-referrer")
	c.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src https: data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	id, ok := paymentFromReceiptToken(c.Params("token"))
	var p models.Payment
	if ok {
		ok = config.DB.Preload("Package").Where("status = ?", "completed").First(&p, id).Error == nil
	}
	if !ok {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Status(fiber.StatusNotFound).SendString(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Receipt not found</title><p style="font-family:system-ui;padding:24px">This receipt link is not valid.</p>`)
	}

	isp := ""
	var z models.Zone
	if config.DB.Unscoped().Select("organization_id").First(&z, p.ZoneID).Error == nil {
		var org models.Organization
		if config.DB.Select("name").First(&org, z.OrganizationID).Error == nil {
			isp = org.Name
		}
	}
	if isp == "" {
		isp = "Zyra Net"
	}
	plan := "WiFi"
	if p.Package != nil && p.Package.Name != "" {
		plan = p.Package.Name
	}
	code := ""
	if p.MpesaReceiptNumber != nil {
		code = *p.MpesaReceiptNumber
	}
	method := map[string]string{"daraja": "M-Pesa", "cyzora": "M-Pesa", "paybill": "M-Pesa Paybill", "cash": "Cash", "credit": "Account credit", "bank": "Bank"}[services.PaymentProvider(p.Provider, p.Method)]
	if method == "" {
		method = "Other"
	}
	row := func(k, v string) string {
		if v == "" {
			return ""
		}
		return `<tr><th>` + esc(k) + `</th><td>` + esc(v) + `</td></tr>`
	}
	body := row("Receipt", fmt.Sprintf("INV-%d", p.ID)) +
		row("Date", utils.Kenya(p.CreatedAt).Format("2 Jan 2006, 15:04")+" EAT") +
		row("Plan", plan) +
		row("Paid by", method) +
		row("M-Pesa code", code) +
		row("Phone", services.MaskPhone(p.Phone))
	page := `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Receipt INV-` + strconv.FormatUint(uint64(p.ID), 10) + `</title>
<style>body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:#f5f5f4;color:#1c1917;margin:0;padding:16px}
.r{max-width:420px;margin:24px auto;background:#fff;border:1px solid #e7e5e4;border-radius:12px;padding:24px}
h1{font-size:18px;margin:0 0 4px}p{margin:0;color:#57534e;font-size:14px}.amt{font-size:32px;font-weight:700;margin:20px 0 8px;font-variant-numeric:tabular-nums}
.ok{display:inline-block;background:#dcfce7;color:#166534;border-radius:6px;padding:2px 8px;font-size:13px;font-weight:600}
table{width:100%;border-collapse:collapse;margin-top:20px;font-size:14px}th{text-align:left;color:#57534e;font-weight:500;padding:8px 0;border-top:1px solid #f5f5f4}
td{text-align:right;padding:8px 0;border-top:1px solid #f5f5f4;font-variant-numeric:tabular-nums}
button{margin-top:20px;width:100%;padding:12px;border:0;border-radius:8px;background:#C2410C;color:#fff;font-size:15px;font-weight:600}
@media print{button{display:none}body{background:#fff}.r{border:0}}</style></head><body><div class="r">
<h1>` + esc(isp) + `</h1><p>Payment receipt</p>
<div class="amt">KES ` + esc(formatAmount(p.Amount)) + `</div><span class="ok">Paid</span>
<table>` + body + `</table></div></body></html>`
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(page)
}
