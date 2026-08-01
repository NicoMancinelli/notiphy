// Package config loads notiphy settings from flags, environment, and an
// optional YAML file. Precedence is flags > env > file > defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full server configuration.
type Config struct {
	// Listen is the bind address for the HTTP server.
	Listen string `yaml:"listen"`
	// BaseURL is the externally reachable origin, used to build approval and
	// live-activity links that land in notifications. Getting this wrong is
	// the single most common misconfiguration, so it is validated at boot.
	BaseURL string `yaml:"base_url"`
	// DB is the SQLite file path.
	DB string `yaml:"db"`

	// AdminToken protects the dashboard and device registration. Leaving it
	// empty leaves those endpoints open, which is only safe on a private
	// network — anyone who can reach the server could otherwise register their
	// own device and receive every notification. The server warns at boot.
	AdminToken string `yaml:"admin_token"`

	// RateLimit is off by default: this is your server, not a SaaS tier. The
	// machinery exists for wire parity with Hark's 429 behaviour.
	RateLimitPerMinute int `yaml:"rate_limit_per_minute"`

	// DefaultResponseTTL applies when a caller omits expiresInSeconds.
	DefaultResponseTTL time.Duration `yaml:"default_response_ttl"`
	// DefaultActivityTTL applies when a caller omits an expiry.
	DefaultActivityTTL time.Duration `yaml:"default_activity_ttl"`
	// DefaultStaleAfter applies when a caller omits a staleness window.
	DefaultStaleAfter time.Duration `yaml:"default_stale_after"`

	// ActivityProgressStep is how much `progress` must advance before a
	// milestone notification fires on transports that cannot update in place.
	// 0.25 means notify at roughly every quarter.
	ActivityProgressStep float64 `yaml:"activity_progress_step"`

	// VAPIDPublicKey and VAPIDPrivateKey sign Web Push requests. Generated on
	// first boot and persisted to the DB if empty.
	VAPIDPublicKey  string `yaml:"vapid_public_key"`
	VAPIDPrivateKey string `yaml:"vapid_private_key"`
	VAPIDSubject    string `yaml:"vapid_subject"`

	// NtfyDefaultServer is used for devices that do not name their own.
	NtfyDefaultServer string `yaml:"ntfy_default_server"`

	// APNs (phase 2). Empty means the transport stays unregistered.
	APNsKeyFile  string `yaml:"apns_key_file"`
	APNsKeyID    string `yaml:"apns_key_id"`
	APNsTeamID   string `yaml:"apns_team_id"`
	APNsTopic    string `yaml:"apns_topic"`
	APNsProduct  bool   `yaml:"apns_production"`
	CallbackUA   string `yaml:"callback_user_agent"`
	TrustProxyIP bool   `yaml:"trust_proxy_ip"`
}

// Default returns the baseline configuration.
func Default() Config {
	return Config{
		Listen:               ":8080",
		BaseURL:              "http://localhost:8080",
		DB:                   "notiphy.db",
		RateLimitPerMinute:   0,
		DefaultResponseTTL:   5 * time.Minute,
		DefaultActivityTTL:   8 * time.Hour,
		DefaultStaleAfter:    4 * time.Hour,
		ActivityProgressStep: 0.25,
		VAPIDSubject:         "mailto:admin@localhost",
		NtfyDefaultServer:    "https://ntfy.sh",
		CallbackUA:           "notiphy/1.0",
	}
}

// Load reads defaults, then the YAML file at path (if it exists and path is
// non-empty), then environment variables prefixed NOTIPHY_.
func Load(path string) (Config, error) {
	c := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &c); err != nil {
				return c, fmt.Errorf("parse %s: %w", path, err)
			}
		case !os.IsNotExist(err):
			return c, fmt.Errorf("read %s: %w", path, err)
		}
	}

	c.applyEnv()

	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) applyEnv() {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv("NOTIPHY_" + key); ok {
			*dst = v
		}
	}
	num := func(key string, dst *int) {
		if v, ok := os.LookupEnv("NOTIPHY_" + key); ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	flt := func(key string, dst *float64) {
		if v, ok := os.LookupEnv("NOTIPHY_" + key); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*dst = f
			}
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := os.LookupEnv("NOTIPHY_" + key); ok {
			if d, err := time.ParseDuration(v); err == nil {
				*dst = d
			}
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv("NOTIPHY_" + key); ok {
			*dst = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		}
	}

	str("LISTEN", &c.Listen)
	str("BASE_URL", &c.BaseURL)
	str("DB", &c.DB)
	str("ADMIN_TOKEN", &c.AdminToken)
	num("RATE_LIMIT_PER_MINUTE", &c.RateLimitPerMinute)
	dur("DEFAULT_RESPONSE_TTL", &c.DefaultResponseTTL)
	dur("DEFAULT_ACTIVITY_TTL", &c.DefaultActivityTTL)
	dur("DEFAULT_STALE_AFTER", &c.DefaultStaleAfter)
	flt("ACTIVITY_PROGRESS_STEP", &c.ActivityProgressStep)
	str("VAPID_PUBLIC_KEY", &c.VAPIDPublicKey)
	str("VAPID_PRIVATE_KEY", &c.VAPIDPrivateKey)
	str("VAPID_SUBJECT", &c.VAPIDSubject)
	str("NTFY_DEFAULT_SERVER", &c.NtfyDefaultServer)
	str("APNS_KEY_FILE", &c.APNsKeyFile)
	str("APNS_KEY_ID", &c.APNsKeyID)
	str("APNS_TEAM_ID", &c.APNsTeamID)
	str("APNS_TOPIC", &c.APNsTopic)
	boolean("APNS_PRODUCTION", &c.APNsProduct)
	boolean("TRUST_PROXY_IP", &c.TrustProxyIP)
}

// Validate checks the settings that cause silent, hard-to-debug breakage.
func (c *Config) Validate() error {
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.BaseURL == "" {
		return fmt.Errorf("base_url must be set: notifications embed absolute links back to this server")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("base_url must include a scheme, got %q", c.BaseURL)
	}
	if c.DB == "" {
		return fmt.Errorf("db path must be set")
	}
	if c.ActivityProgressStep <= 0 || c.ActivityProgressStep > 1 {
		return fmt.Errorf("activity_progress_step must be in (0,1], got %v", c.ActivityProgressStep)
	}
	if dir := filepath.Dir(c.DB); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	return nil
}

// APNsEnabled reports whether enough APNs settings are present to register the
// transport. Phase 2 turns on purely by filling these in.
func (c *Config) APNsEnabled() bool {
	return c.APNsKeyFile != "" && c.APNsKeyID != "" && c.APNsTeamID != "" && c.APNsTopic != ""
}
