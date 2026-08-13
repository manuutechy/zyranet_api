package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// AppConfig holds all application configuration loaded from environment variables.
type AppConfig struct {
	// App
	AppEnv  string
	AppPort string

	// Database
	DBHost string
	DBPort string
	DBName string
	DBUser string
	DBPass string

	// JWT
	JWTSecret string
	JWTExpiry time.Duration

	// M-Pesa
	MpesaConsumerKey    string
	MpesaConsumerSecret string
	MpesaShortcode      string
	MpesaPasskey        string
	MpesaCallbackURL    string
	MpesaEnv            string
	// MpesaCallbackSecret, when set, must be supplied as a `?token=` query
	// parameter on inbound STK callback / C2B validation / C2B confirmation
	// requests (see handlers/mpesa.go mpesaCallbackAuthorized). Append it to
	// the CallBackURL/ValidationURL/ConfirmationURL you register with Daraja.
	MpesaCallbackSecret string

	// MikroTik router connection safety toggles — see services/mikrotik.go.
	// Both default to the secure behavior and are meant to be flipped on
	// only for local/dev testing against self-signed certs or routers on
	// private/loopback ranges (e.g. a LAN test rig or Docker network).
	MikroTikInsecureSkipVerify bool
	MikroTikAllowPrivateIPs    bool

	// SMS Provider ("hostpinnacle" is the default)
	SmsProvider string

	// Hostpinnacle
	HostpinnacleBaseURL  string
	HostpinnacleApiKey   string
	HostpinnacleUsername string
	HostpinnacleSenderID string

	// CORS Origins
	AllowedOrigins []string

	// Error monitoring (Sentry). Empty DSN disables reporting entirely.
	SentryDSN string

	// CookieDomain scopes the auth session cookie. Empty (host-only) is right
	// for local dev; production sets ".zyranet.co.ke" so the cookie set by
	// the API is also sent to admin./portal.zyranet.co.ke.
	CookieDomain string
}

var Config AppConfig

// Load reads the .env file (if present) and populates Config.
func Load() {
	// .env is optional in production (Railway injects env vars directly)
	if err := godotenv.Load(); err != nil {
		log.Println("[config] No .env file found, reading from environment")
	}

	expiry := parseDuration(getEnv("JWT_EXPIRY", "24h"), 24*time.Hour)

	Config = AppConfig{
		AppEnv:  getEnv("APP_ENV", "production"),
		AppPort: getEnv("PORT", getEnv("APP_PORT", "8080")),

		DBHost: getEnv("DB_HOST", "localhost"),
		DBPort: getEnv("DB_PORT", "3306"),
		DBName: getEnv("DB_NAME", "zyranet"),
		DBUser: getEnv("DB_USER", "root"),
		DBPass: getEnv("DB_PASS", ""),

		JWTSecret: getEnv("JWT_SECRET", "change-me-in-production"),
		JWTExpiry: expiry,

		MpesaConsumerKey:    getEnv("MPESA_CONSUMER_KEY", ""),
		MpesaConsumerSecret: getEnv("MPESA_CONSUMER_SECRET", ""),
		MpesaShortcode:      getEnv("MPESA_SHORTCODE", "174379"),
		MpesaPasskey:        getEnv("MPESA_PASSKEY", ""),
		MpesaCallbackURL:    getEnv("MPESA_CALLBACK_URL", ""),
		MpesaEnv:            getEnv("MPESA_ENV", "sandbox"),
		MpesaCallbackSecret: getEnv("MPESA_CALLBACK_SECRET", ""),

		MikroTikInsecureSkipVerify: getEnvBool("MIKROTIK_INSECURE_SKIP_VERIFY", false),
		MikroTikAllowPrivateIPs:    getEnvBool("MIKROTIK_ALLOW_PRIVATE_IPS", false),

		SmsProvider: getEnv("SMS_PROVIDER", "hostpinnacle"),

		HostpinnacleBaseURL:  getEnv("HOSTPINNACLE_BASE_URL", "https://smsportal.hostpinnacle.co.ke/SMSApi/send"),
		HostpinnacleApiKey:   getEnv("HOSTPINNACLE_API_KEY", ""),
		HostpinnacleUsername: getEnv("HOSTPINNACLE_USERNAME", ""),
		HostpinnacleSenderID: getEnv("HOSTPINNACLE_SENDER_ID", ""),

		AllowedOrigins: allowedOrigins(getEnv("APP_ENV", "production")),

		SentryDSN: getEnv("SENTRY_DSN", ""),

		CookieDomain: getEnv("COOKIE_DOMAIN", ""),
	}

	// Refuse to boot with an unsafe JWT secret regardless of AppEnv. Gating
	// this only on `AppEnv == "production"` (the previous behavior) meant a
	// typo'd or unexpected APP_ENV value (e.g. "prod", "Production", a
	// blank string) would silently skip the check and let the API sign
	// tokens with a secret anyone can read off GitHub. A short/default/empty
	// secret is unsafe in every environment, not just the one spelled
	// exactly "production".
	if Config.JWTSecret == "" || Config.JWTSecret == "change-me-in-production" || len(Config.JWTSecret) < 32 {
		log.Fatal("[config] JWT_SECRET is missing, the default placeholder, or too short (<32 chars) — set a long random secret (openssl rand -base64 48) before starting the API")
	}

	fmt.Printf("[config] Loaded — env=%s port=%s db=%s@%s/%s\n",
		Config.AppEnv, Config.AppPort, Config.DBUser, Config.DBHost, Config.DBName)
}

// allowedOrigins returns the CORS allow-list. Real production traffic should
// only ever come from the two known frontends — localhost is only needed
// when testing a non-production deployment against this API.
func allowedOrigins(appEnv string) []string {
	origins := []string{
		"https://admin.zyranet.co.ke",
		"https://portal.zyranet.co.ke",
	}
	if custom := getEnv("ALLOWED_ORIGINS", ""); custom != "" {
		for _, o := range strings.Split(custom, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				origins = append(origins, o)
			}
		}
	}
	if appEnv != "production" {
		origins = append(origins,
			"http://localhost:5173",
			"http://localhost:5174",
			"http://localhost:4173",
			"http://localhost:3000",
			"http://127.0.0.1:5173",
			"http://127.0.0.1:5174",
			"http://127.0.0.1:4173",
			"http://127.0.0.1:3000",
		)
	}
	return origins
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	b, err := strconv.ParseBool(val)
	if err != nil {
		return fallback
	}
	return b
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// GetDBPort returns the DB port as int.
func GetDBPort() int {
	p, _ := strconv.Atoi(Config.DBPort)
	if p == 0 {
		return 3306
	}
	return p
}
