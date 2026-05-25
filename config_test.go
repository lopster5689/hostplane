package main

import (
	"os"
	"testing"
)

// LoadConfig is not unit-tested directly because mustEnv calls log.Fatalf,
// which would terminate the test process if API_KEY is missing. The helpers
// it calls are covered here; LoadConfig is exercised through integration tests
// (Phase 7) which run the full process with a real env block.

func TestGetEnv(t *testing.T) {
	t.Setenv("HOSTPLANE_TEST_VAR", "set-value")
	if got := getEnv("HOSTPLANE_TEST_VAR", "fallback"); got != "set-value" {
		t.Errorf("getEnv with value set: got %q want %q", got, "set-value")
	}

	os.Unsetenv("HOSTPLANE_TEST_VAR_2")
	if got := getEnv("HOSTPLANE_TEST_VAR_2", "fallback"); got != "fallback" {
		t.Errorf("getEnv with no value: got %q want %q", got, "fallback")
	}

	// Empty string in env is treated as unset (matches getEnv behavior).
	t.Setenv("HOSTPLANE_TEST_VAR_3", "")
	if got := getEnv("HOSTPLANE_TEST_VAR_3", "fallback"); got != "fallback" {
		t.Errorf("getEnv with empty value: got %q want %q (must fall through to fallback)", got, "fallback")
	}
}

func TestGetEnvBool(t *testing.T) {
	cases := []struct {
		name     string
		val      string
		fallback bool
		want     bool
	}{
		{"true-literal", "true", false, true},
		{"True-mixed", "True", false, true},
		{"TRUE-upper", "TRUE", false, true},
		{"1", "1", false, true},
		{"yes", "yes", false, true},
		{"YES", "YES", false, true},
		{"false-literal", "false", true, false},
		{"0", "0", true, false},
		{"no", "no", true, false},
		{"empty-fallback-true", "", true, true},
		{"empty-fallback-false", "", false, false},
		{"whitespace-empty", "   ", true, true}, // trimmed, falls back
		{"junk-falls-back-to-false-output", "maybe", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.val == "" {
				os.Unsetenv("HOSTPLANE_TEST_BOOL")
			} else {
				t.Setenv("HOSTPLANE_TEST_BOOL", c.val)
			}
			got := getEnvBool("HOSTPLANE_TEST_BOOL", c.fallback)
			if got != c.want {
				t.Errorf("getEnvBool(%q, fallback=%v) = %v, want %v", c.val, c.fallback, got, c.want)
			}
		})
	}
}

func TestDBHost(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"standard", "user:pass@tcp(***REMOVED***:3306)/", "***REMOVED***:3306"},
		{"alt-port", "user:pass@tcp(host.example.com:33060)/db", "host.example.com:33060"},
		// Fallback path: DSN that lacks tcp(...) returns the hard-coded default.
		// This is the safety net, not the happy path.
		{"malformed-fallback", "garbage", "***REMOVED***:3306"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{WordPressDSN: c.dsn}
			if got := cfg.DBHost(); got != c.want {
				t.Errorf("DBHost(%q) = %q, want %q", c.dsn, got, c.want)
			}
		})
	}
}
