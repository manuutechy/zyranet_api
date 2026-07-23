// Command generate_platform_invoices creates one Draft PlatformInvoice per
// active ISP tenant with a configured billing rate, for the previous
// calendar month. Intended to run on a monthly systemd timer alongside the
// main API process; an SA can also trigger this on demand from the
// platform app (POST /platform/invoices/generate), which shares the exact
// same generation logic.
//
// Usage:
//
//	go run ./cmd/generate_platform_invoices
package main

import (
	"log"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/handlers"
)

func main() {
	config.Load()
	config.ConnectDatabase()

	periodStart, periodEnd := handlers.PreviousCalendarMonth()
	invoices, err := handlers.GeneratePlatformInvoices(periodStart, periodEnd)
	if err != nil {
		log.Fatalf("[generate_platform_invoices] failed: %v", err)
	}
	log.Printf("[generate_platform_invoices] Generated %d invoice(s) for %s to %s",
		len(invoices), periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
}
