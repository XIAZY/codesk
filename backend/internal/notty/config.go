package notty

import (
	"fmt"
	"os"
)

type Config struct {
	Port             string
	DatabaseURL      string
	MaxLiveEditBytes int
	PprofAddr        string
	JWTSecret        string
}

func LoadConfig() Config {
	return Config{
		Port:             getenv("NOTTY_PORT", "8080"),
		DatabaseURL:      getenv("NOTTY_DATABASE_URL", ""),
		MaxLiveEditBytes: getenvInt("NOTTY_MAX_LIVE_EDIT_BYTES", 1500),
		PprofAddr:        getenv("NOTTY_PPROF_ADDR", ""),
		JWTSecret:        getenv("NOTTY_JWT_SECRET", ""),
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
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
