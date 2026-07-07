package regression

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const regressionDeadlineScaleEnv = "NOTTY_REGRESSION_DEADLINE_SCALE"

func TestRegressionDeadlineScaleFromEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(regressionDeadlineScaleEnv, "")
		scale, err := regressionDeadlineScaleFromEnv()
		if err != nil {
			t.Fatalf("default scale returned error: %v", err)
		}
		if scale != 1 {
			t.Fatalf("default scale = %v, want 1", scale)
		}
	})

	t.Run("explicit", func(t *testing.T) {
		t.Setenv(regressionDeadlineScaleEnv, "3.5")
		scale, err := regressionDeadlineScaleFromEnv()
		if err != nil {
			t.Fatalf("explicit scale returned error: %v", err)
		}
		if scale != 3.5 {
			t.Fatalf("explicit scale = %v, want 3.5", scale)
		}
	})

	t.Run("rejects shrink", func(t *testing.T) {
		t.Setenv(regressionDeadlineScaleEnv, "0.5")
		if _, err := regressionDeadlineScaleFromEnv(); err == nil {
			t.Fatalf("expected error for scale below 1")
		}
	})

	t.Run("rejects invalid", func(t *testing.T) {
		t.Setenv(regressionDeadlineScaleEnv, "slow")
		if _, err := regressionDeadlineScaleFromEnv(); err == nil {
			t.Fatalf("expected error for invalid scale")
		}
	})
}

func TestRegressionScaledTimeout(t *testing.T) {
	t.Setenv(regressionDeadlineScaleEnv, "4")
	if got, want := regressionScaledTimeout(t, 15*time.Second), time.Minute; got != want {
		t.Fatalf("scaled timeout = %s, want %s", got, want)
	}
}

func regressionDeadlineScaleFromEnv() (float64, error) {
	raw := strings.TrimSpace(os.Getenv(regressionDeadlineScaleEnv))
	if raw == "" {
		return 1, nil
	}
	scale, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	if scale < 1 {
		return 0, errRegressionDeadlineScaleBelowOne
	}
	return scale, nil
}

var errRegressionDeadlineScaleBelowOne = errors.New("scale must be at least 1")

func regressionScaledTimeout(t *testing.T, timeout time.Duration) time.Duration {
	t.Helper()
	scale, err := regressionDeadlineScaleFromEnv()
	if err != nil {
		t.Fatalf("invalid %s: %v", regressionDeadlineScaleEnv, err)
	}
	if scale == 1 || timeout <= 0 {
		return timeout
	}
	return time.Duration(float64(timeout) * scale)
}
