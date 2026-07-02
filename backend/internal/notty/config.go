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

	requireEmailSet bool
	requireEmailErr error
}

func LoadConfig() Config {
	requireEmail, requireEmailSet, requireEmailErr := getenvBool("NOTTY_REQUIRE_EMAIL", true)
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
		requireEmailSet:  requireEmailSet,
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
	if cfg.requireEmailSet && !cfg.RequireEmail {
		return errors.New("NOTTY_REQUIRE_EMAIL=false is not supported; email delivery is required")
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

func getenvBool(key string, fallback bool) (bool, bool, error) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "":
		return fallback, false, nil
	case "1", "true", "t", "yes", "y", "on":
		return true, true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("%s must be a boolean value: use 1/true/yes/on or 0/false/no/off", key)
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
