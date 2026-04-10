package config

import (
	"os"
	"testing"
	"time"
)

// TestLoadUsesDefaults verifies optional fields fall back to their defaults.
// DIRECTORY_SERVER_URL has no default and must always be supplied explicitly.
func TestLoadUsesDefaults(t *testing.T) {
	unsetEnv(t, "PORT")
	unsetEnv(t, "NODE_TYPE")
	setEnv(t, "DIRECTORY_SERVER_URL", "http://directory-server:8080") // required — no default
	unsetEnv(t, "HEARTBEAT_CLEANUP_INTERVAL")
	unsetEnv(t, "HEARTBEAT_TIMEOUT")

	cfg := Load()

	if cfg.Port != "8080" {
		t.Fatalf("Port = %q, want %q", cfg.Port, "8080")
	}
	if cfg.NodeType != "unknown" {
		t.Fatalf("NodeType = %q, want %q", cfg.NodeType, "unknown")
	}
	if cfg.DirectoryServerURL != "http://directory-server:8080" {
		t.Fatalf("DirectoryServerURL = %q, want %q", cfg.DirectoryServerURL, "http://directory-server:8080")
	}
	if cfg.HeartbeatCleanupInterval != 15*time.Second {
		t.Fatalf("HeartbeatCleanupInterval = %s, want %s", cfg.HeartbeatCleanupInterval, 15*time.Second)
	}
	if cfg.HeartbeatTimeout != 30*time.Second {
		t.Fatalf("HeartbeatTimeout = %s, want %s", cfg.HeartbeatTimeout, 30*time.Second)
	}
}

func TestLoadUsesEnvironmentOverrides(t *testing.T) {
	setEnv(t, "PORT", "9090")
	setEnv(t, "NODE_TYPE", "directory-server")
	setEnv(t, "DIRECTORY_SERVER_URL", "http://directory-server:8080")
	setEnv(t, "HEARTBEAT_CLEANUP_INTERVAL", "20s")
	setEnv(t, "HEARTBEAT_TIMEOUT", "45s")

	cfg := Load()

	if cfg.Port != "9090" {
		t.Fatalf("Port = %q, want %q", cfg.Port, "9090")
	}
	if cfg.NodeType != "directory-server" {
		t.Fatalf("NodeType = %q, want %q", cfg.NodeType, "directory-server")
	}
	if cfg.DirectoryServerURL != "http://directory-server:8080" {
		t.Fatalf("DirectoryServerURL = %q, want %q", cfg.DirectoryServerURL, "http://directory-server:8080")
	}
	if cfg.HeartbeatCleanupInterval != 20*time.Second {
		t.Fatalf("HeartbeatCleanupInterval = %s, want %s", cfg.HeartbeatCleanupInterval, 20*time.Second)
	}
	if cfg.HeartbeatTimeout != 45*time.Second {
		t.Fatalf("HeartbeatTimeout = %s, want %s", cfg.HeartbeatTimeout, 45*time.Second)
	}
}

func TestGetEnvAndGetDurationEnvFallbacks(t *testing.T) {
	unsetEnv(t, "MISSING_VALUE")
	if got := getEnv("MISSING_VALUE", "fallback"); got != "fallback" {
		t.Fatalf("getEnv() = %q, want %q", got, "fallback")
	}

	setEnv(t, "INVALID_DURATION", "not-a-duration")
	if got := getDurationEnv("INVALID_DURATION", 5*time.Second); got != 5*time.Second {
		t.Fatalf("getDurationEnv() = %s, want %s", got, 5*time.Second)
	}

	unsetEnv(t, "EMPTY_DURATION")
	setEnv(t, "EMPTY_DURATION", "")
	if got := getDurationEnv("EMPTY_DURATION", 7*time.Second); got != 7*time.Second {
		t.Fatalf("getDurationEnv() = %s, want %s", got, 7*time.Second)
	}
}

func setEnv(t *testing.T, key, value string) {
	t.Helper()
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("os.Setenv(%q) error = %v", key, err)
	}
	t.Cleanup(func() {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v", key, err)
		}
	})
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	oldValue, hadValue := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q) error = %v", key, err)
	}
	t.Cleanup(func() {
		var err error
		if hadValue {
			err = os.Setenv(key, oldValue)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Fatalf("env cleanup for %q error = %v", key, err)
		}
	})
}
