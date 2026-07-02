package notty

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Port             string
	DatabaseURL      string
	MaxLiveEditBytes int
	PprofAddr        string
	JWTSecret        string
	PublicOrigin     string
	MailgunDomain    string
	MailgunAPIKey    string
	MailgunFrom      string
	RequireEmail     bool

	requireEmailErr error
}

func LoadConfig() Config {
	requireEmail, requireEmailErr := getenvBool("NOTTY_REQUIRE_EMAIL", true)
	if requireEmailErr == nil && !requireEmail {
		requireEmailErr = errors.New("NOTTY_REQUIRE_EMAIL=false is not supported; email delivery is required")
	}
	return Config{
		Port:             getenv("NOTTY_PORT", "8080"),
		DatabaseURL:      getenv("NOTTY_DATABASE_URL", ""),
		MaxLiveEditBytes: getenvInt("NOTTY_MAX_LIVE_EDIT_BYTES", 1500),
		PprofAddr:        getenv("NOTTY_PPROF_ADDR", ""),
		JWTSecret:        getenv("NOTTY_JWT_SECRET", ""),
		PublicOrigin:     getenv("NOTTY_PUBLIC_ORIGIN", getenv("NOTTY_FRONTEND_ORIGIN", "http://localhost:5173")),
		MailgunDomain:    getenv("NOTTY_MAILGUN_DOMAIN", ""),
		MailgunAPIKey:    getenv("NOTTY_MAILGUN_API_KEY", ""),
		MailgunFrom:      getenv("NOTTY_MAILGUN_FROM", ""),
		RequireEmail:     requireEmail,
		requireEmailErr:  requireEmailErr,
	}
}

func (cfg Config) MailgunConfigured() bool {
	return strings.TrimSpace(cfg.MailgunDomain) != "" &&
		strings.TrimSpace(cfg.MailgunAPIKey) != "" &&
		strings.TrimSpace(cfg.MailgunFrom) != ""
}

func (cfg Config) ValidateEmailConfig() error {
	if cfg.requireEmailErr != nil {
		return cfg.requireEmailErr
	}
	if cfg.RequireEmail && !cfg.MailgunConfigured() {
		return errors.New("NOTTY_REQUIRE_EMAIL requires NOTTY_MAILGUN_DOMAIN, NOTTY_MAILGUN_API_KEY, and NOTTY_MAILGUN_FROM")
	}
	return nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvBool(key string, fallback bool) (bool, error) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "":
		return fallback, nil
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean value: use 1/true/yes/on or 0/false/no/off", key)
	}
}

func getenvInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}
