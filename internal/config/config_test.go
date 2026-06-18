package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestGetenvDurationRejectsNonPositiveValues(t *testing.T) {
	t.Setenv("MOMENT_TEST_DURATION", "0")
	if got := getenvDuration("MOMENT_TEST_DURATION", 30*time.Second); got != 30*time.Second {
		t.Fatalf("zero duration = %v, want fallback", got)
	}
	t.Setenv("MOMENT_TEST_DURATION", "-1s")
	if got := getenvDuration("MOMENT_TEST_DURATION", 30*time.Second); got != 30*time.Second {
		t.Fatalf("negative duration = %v, want fallback", got)
	}
	t.Setenv("MOMENT_TEST_DURATION", "5s")
	if got := getenvDuration("MOMENT_TEST_DURATION", 30*time.Second); got != 5*time.Second {
		t.Fatalf("positive duration = %v, want 5s", got)
	}
}

func TestConfigLogLevelNormalizesValue(t *testing.T) {
	if got := (Config{Log: LogConfig{Level: " WARN "}}).LogLevel(); got != slog.LevelWarn {
		t.Fatalf("LogLevel() = %v, want warn", got)
	}
}
