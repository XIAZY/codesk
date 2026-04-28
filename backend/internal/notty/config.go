package notty

import (
	"fmt"
	"os"
)

type Config struct {
	Port             string
	DatabaseURL      string
	DataFile         string
	MaxLiveEditBytes int
	PprofAddr        string
}

func LoadConfig() Config {
	return Config{
		Port:             getenv("NOTTY_PORT", "8080"),
		DatabaseURL:      getenv("NOTTY_DATABASE_URL", ""),
		DataFile:         getenv("NOTTY_DATA_FILE", "/data/state.json"),
		MaxLiveEditBytes: getenvInt("NOTTY_MAX_LIVE_EDIT_BYTES", 1500),
		PprofAddr:        getenv("NOTTY_PPROF_ADDR", ""),
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
