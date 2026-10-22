package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr          string
	ExternalURL         string
	DataDir             string
	SecretKey           string // 32+ bytes, signs session cookies
	SyncInterval        time.Duration
	MaxEpisodeBytes     int64
	MaxStorageBytes     int64 // global disk budget; 0 = unlimited. Downloads pause when exceeded.
	MaxFeedsPerUser     int
	DefaultUserMaxBytes int64 // per-user quota default; 0 = unlimited
	SSRFAllowPrivate    bool  // dev only; never enable in production

	// OIDC (Authentik). Empty Issuer disables auth (single-user local mode).
	OIDCIssuer        string
	OIDCClientID      string
	OIDCClientSecret  string
	OIDCRedirectURL   string
	OIDCAllowedGroups string // comma-separated allowlist; empty = all authenticated users
	OIDCAdminGroups   string // comma-separated; members get admin rights
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:       getenv("PA_LISTEN_ADDR", ":8080"),
		ExternalURL:      strings.TrimRight(getenv("PA_EXTERNAL_URL", "http://localhost:8080"), "/"),
		DataDir:          getenv("PA_DATA_DIR", "./data"),
		SecretKey:        os.Getenv("PA_SECRET_KEY"),
		SyncInterval:     6 * time.Hour,
		MaxEpisodeBytes:  1024 * 1024 * 1024, // 1 GiB per file
		MaxFeedsPerUser:  100,
		SSRFAllowPrivate: os.Getenv("PA_SSRF_ALLOW_PRIVATE") == "true",

		OIDCIssuer:        strings.TrimRight(os.Getenv("PA_OIDC_ISSUER"), "/"),
		OIDCClientID:      os.Getenv("PA_OIDC_CLIENT_ID"),
		OIDCClientSecret:  os.Getenv("PA_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:   os.Getenv("PA_OIDC_REDIRECT_URL"),
		OIDCAllowedGroups: os.Getenv("PA_OIDC_ALLOWED_GROUPS"),
		OIDCAdminGroups:   os.Getenv("PA_OIDC_ADMIN_GROUPS"),
	}

	if v := os.Getenv("PA_SYNC_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, errors.New("PA_SYNC_INTERVAL: " + err.Error())
		}
		if d < time.Minute {
			return nil, errors.New("PA_SYNC_INTERVAL must be >= 1m")
		}
		cfg.SyncInterval = d
	}
	if v := os.Getenv("PA_MAX_EPISODE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, errors.New("PA_MAX_EPISODE_BYTES must be a positive integer")
		}
		cfg.MaxEpisodeBytes = n
	}
	for _, e := range []struct {
		env string
		dst *int64
	}{
		{"PA_MAX_STORAGE_BYTES", &cfg.MaxStorageBytes},
		{"PA_USER_MAX_BYTES", &cfg.DefaultUserMaxBytes},
	} {
		if v := os.Getenv(e.env); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New(e.env + " must be a non-negative integer")
			}
			*e.dst = n
		}
	}

	if cfg.OIDCIssuer != "" {
		if cfg.OIDCClientID == "" || cfg.SecretKey == "" {
			return nil, errors.New("PA_OIDC_CLIENT_ID and PA_SECRET_KEY are required when PA_OIDC_ISSUER is set")
		}
		if cfg.OIDCRedirectURL == "" {
			cfg.OIDCRedirectURL = cfg.ExternalURL + "/auth/callback"
		}
	} else if cfg.SecretKey == "" {
		return nil, errors.New("PA_SECRET_KEY is required (use `openssl rand -hex 32`)")
	}

	if len(cfg.SecretKey) > 0 && len(cfg.SecretKey) < 32 {
		return nil, errors.New("PA_SECRET_KEY must be at least 32 characters")
	}
	return cfg, nil
}

func (c *Config) AuthEnabled() bool { return c.OIDCIssuer != "" }

func (c *Config) AllowedGroups() map[string]bool { return splitGroups(c.OIDCAllowedGroups) }
func (c *Config) AdminGroups() map[string]bool   { return splitGroups(c.OIDCAdminGroups) }

func splitGroups(v string) map[string]bool {
	out := map[string]bool{}
	for _, g := range strings.Split(v, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out[g] = true
		}
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
