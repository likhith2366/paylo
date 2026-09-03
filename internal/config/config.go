// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Env             string
	Port            int
	DatabaseURL     string
	RedisURL        string
	BankSimulatorURL string
	// Hard ceiling on the downstream processor call. Exceeding it produces the
	// ambiguous-timeout case (§24.1): the charge is marked
	// requires_reconciliation rather than blindly retried.
	BankTimeout     time.Duration
	DBMaxConns      int32
	// Salt for card fingerprints. In production this comes from Secrets
	// Manager and is rotated; a fingerprint is only meaningful for velocity
	// rules and blocklists if it is stable and unguessable (§14.5).
	CardHashSalt    string
	LogLevel        string
}

func Load() (Config, error) {
	c := Config{
		Env:              env("PAYLO_ENV", "development"),
		Port:             envInt("PORT", 8080),
		DatabaseURL:      env("DATABASE_URL", "postgres://paylo:paylo@localhost:5432/paylo?sslmode=disable"),
		RedisURL:         env("REDIS_URL", "redis://localhost:6379/0"),
		BankSimulatorURL: env("BANK_SIMULATOR_URL", "http://localhost:8090"),
		BankTimeout:      envDuration("BANK_TIMEOUT", 5*time.Second),
		DBMaxConns:       int32(envInt("DB_MAX_CONNS", 25)),
		CardHashSalt:     env("CARD_HASH_SALT", ""),
		LogLevel:         env("LOG_LEVEL", "info"),
	}

	// Refuse to start in production without a real salt rather than silently
	// falling back to a default — a predictable fingerprint salt would let an
	// attacker confirm whether a given card has been used on the platform.
	if c.CardHashSalt == "" {
		if c.Env == "production" {
			return Config{}, fmt.Errorf("config: CARD_HASH_SALT is required in production")
		}
		c.CardHashSalt = "dev-only-insecure-salt"
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
