package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/routes"
	"github.com/zyranet/zyranet-api/services"
	"strings"
	"fmt"
)

func main() {
	// Load configuration and connect database
	config.Load()
	middleware.InitSentry()
	defer middleware.FlushSentry()
	config.ConnectDatabase()

	// Auto-migrate tables to generate missing columns (e.g. deleted_at)
	// Disable FK checks to handle circular references between users ↔ zones
	log.Println("[database] Running schema auto-migrations...")
	config.DB.Exec("SET FOREIGN_KEY_CHECKS = 0")
	if err := config.DB.AutoMigrate(
		&models.User{},
		&models.Zone{},
		&models.Package{},
		&models.Customer{},
		&models.Ticket{},
		&models.Payment{},
		&models.Session{},
		&models.Setting{},
		&models.Voucher{},
		&models.AuditLog{},
		&models.CreditLog{},
		&models.SmsLog{},
		&models.ZoneAlert{},
		&models.ZoneStat{},
	); err != nil {
		config.DB.Exec("SET FOREIGN_KEY_CHECKS = 1")
		log.Fatalf("[database] AutoMigrate failed: %v", err)
	}
	config.DB.Exec("SET FOREIGN_KEY_CHECKS = 1")

	// Backfill existing account numbers
	log.Println("[database] Backfilling customer account numbers...")
	var customersWithoutAcc []models.Customer
	if err := config.DB.Where("account_number IS NULL OR account_number = '' OR account_number LIKE 'ZN-%'").Order("id ASC").Find(&customersWithoutAcc).Error; err == nil {
		for _, cust := range customersWithoutAcc {
			if cust.Phone != "" {
				cust.AccountNumber = fmt.Sprintf("ZYR#%s", cust.Phone)
			} else {
				var count int64
				config.DB.Model(&models.Customer{}).Where("account_number LIKE ? AND account_number NOT LIKE ?", "ZYR#%", "ZYR#0%").Count(&count)
				cust.AccountNumber = fmt.Sprintf("ZYR#%d", 10001+count)
			}
			if err := config.DB.Model(&cust).Update("account_number", cust.AccountNumber).Error; err != nil {
				log.Printf("[database] Failed to backfill account number for customer ID %d (trying sequential fallback): %v", cust.ID, err)
				// Try sequential fallback to avoid duplicate keys
				var count int64
				config.DB.Model(&models.Customer{}).Where("account_number LIKE ? AND account_number NOT LIKE ?", "ZYR#%", "ZYR#0%").Count(&count)
				cust.AccountNumber = fmt.Sprintf("ZYR#%d", 10001+count)
				if err2 := config.DB.Model(&cust).Update("account_number", cust.AccountNumber).Error; err2 != nil {
					log.Printf("[database] Fallback failed for customer ID %d: %v", cust.ID, err2)
				} else {
					log.Printf("[database] Backfilled customer %d with fallback account number %s", cust.ID, cust.AccountNumber)
				}
			} else {
				log.Printf("[database] Backfilled customer %d with account number %s", cust.ID, cust.AccountNumber)
			}
		}
	} else {
		log.Printf("[database] Failed to query customers for backfill: %v", err)
	}

	// Initialise services
	smsSvc := services.NewSmsService()
	mikrotikSvc := services.NewMikroTikService()
	scriptSvc := services.NewMikroTikScriptService()
	voucherSvc := services.NewVoucherService(smsSvc)
	mpesaSvc := services.NewMpesaService(smsSvc, voucherSvc, mikrotikSvc)

	// Inject services into handlers
	handlers.InitMpesaService(mpesaSvc, smsSvc)
	handlers.InitVoucherService(voucherSvc)
	handlers.InitZoneServices(mikrotikSvc, scriptSvc)
	handlers.InitCustomerAuthSMS(smsSvc)
	handlers.InitScriptService(scriptSvc)

	// Create Fiber app
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			msg := err.Error()
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
				msg = e.Message
			}
			return c.Status(code).JSON(fiber.Map{
				"success": false,
				"error":   msg,
				"message": "An error occurred.",
			})
		},
	})

	// Global middleware — Sentry wraps recover so panics (turned into errors
	// by recover.New) are reported too.
	app.Use(middleware.SentryMiddleware())
	app.Use(recover.New())
	app.Use(logger.New(logger.Config{
		Format: "[${time}] ${method} ${path} → ${status} (${latency})\n",
	}))

	// CORS
	if config.Config.AppEnv == "local" {
		app.Use(func(c *fiber.Ctx) error {
			origin := c.Get("Origin")
			if origin != "" {
				c.Set("Access-Control-Allow-Origin", origin)
			} else {
				c.Set("Access-Control-Allow-Origin", "*")
			}
			c.Set("Access-Control-Allow-Credentials", "true")
			c.Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
			c.Set("Access-Control-Allow-Headers", "Origin,Content-Type,Authorization,Accept")
			
			if c.Method() == "OPTIONS" {
				return c.SendStatus(fiber.StatusNoContent)
			}
			return c.Next()
		})
	} else {
		allowedOrigins := strings.Join(config.Config.AllowedOrigins, ",")
		app.Use(cors.New(cors.Config{
			AllowOrigins:     allowedOrigins,
			AllowMethods:     "GET,POST,PUT,DELETE,OPTIONS",
			AllowHeaders:     "Origin,Content-Type,Authorization,Accept",
			AllowCredentials: true,
		}))
	}

	// Static file serving (for uploaded images)
	app.Static("/uploads", "./public/uploads")

	// Register all API routes
	routes.Register(app)

	// Health check
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "app": "Zyra Net API"})
	})

	// Global 404 — keeps unmatched routes consistent with the JSON error shape
	app.Use(func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"error":   "Not found.",
			"message": "The requested resource does not exist.",
		})
	})

	// Start server
	port := ":" + config.Config.AppPort
	log.Printf("[zyranet-api] Starting on %s", port)

	go func() {
		if err := app.Listen(port); err != nil {
			log.Fatalf("[zyranet-api] Server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[zyranet-api] Shutting down...")
	app.Shutdown()
}
