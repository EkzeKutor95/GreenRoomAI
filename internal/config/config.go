// Package config loads local configuration without logging secret values.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	BaseURL, AppID, OrganizationID, ClientSecret, APIKey, TelegramToken string
	AllowedUserIDs                                                      []int64
	ReportChatID                                                        int64
	Location                                                            *time.Location
	PollInterval, HTTPTimeout, RequestInterval, OLAPDelay               time.Duration
	AutoReports, DailyReports                                           bool
	StateFile, OpenAIKey, OpenAIModel, WebhookListen, WebhookSecret     string
	OpenAIVisionModel                                                   string
	DailyHour, RecoveryDays                                             int
	DessertGroups, DessertProductIDs                                    []string
}

// LoadEnv reads literal KEY=value lines. It never invokes a shell or expands values.
// Existing environment variables take precedence, including explicitly empty ones.
func LoadEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer f.Close() // Read-only file; close cannot lose data.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		key, value, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || !envKey(key) {
			return fmt.Errorf("env line %d: %w", line, ErrInvalid)
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return fmt.Errorf("env line %d: %w", line, ErrInvalid)
			}
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set env key: %w", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	return nil
}

func Load() (Config, error) {
	c := Config{BaseURL: os.Getenv("IIKO_BASE_URL"), AppID: os.Getenv("IIKO_APP_ID"), OrganizationID: os.Getenv("IIKO_ORGANIZATION_ID"), ClientSecret: os.Getenv("IIKO_CLIENT_SECRET"), APIKey: os.Getenv("IIKO_API_KEY"), TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"), OpenAIKey: os.Getenv("OPENAI_API_KEY"), OpenAIModel: optional("OPENAI_MODEL", "gpt-4.1-mini"), StateFile: optional("STATE_FILE", "run/state.json"), WebhookListen: os.Getenv("WEBHOOK_LISTEN"), WebhookSecret: os.Getenv("WEBHOOK_SECRET"), DessertGroups: list(optional("DESSERT_GROUPS", "Десерты,Десерты из кухни")), DessertProductIDs: list(os.Getenv("DESSERT_PRODUCT_IDS"))}
	c.OpenAIVisionModel = optional("OPENAI_VISION_MODEL", c.OpenAIModel)

	required := []struct{ name, value string }{{"IIKO_BASE_URL", c.BaseURL}, {"IIKO_APP_ID", c.AppID}, {"IIKO_ORGANIZATION_ID", c.OrganizationID}, {"IIKO_CLIENT_SECRET", c.ClientSecret}, {"IIKO_API_KEY", c.APIKey}, {"TELEGRAM_BOT_TOKEN", c.TelegramToken}, {"TELEGRAM_ALLOWED_USER_IDS", os.Getenv("TELEGRAM_ALLOWED_USER_IDS")}, {"TELEGRAM_REPORT_CHAT_ID", os.Getenv("TELEGRAM_REPORT_CHAT_ID")}, {"IIKO_TIMEZONE", os.Getenv("IIKO_TIMEZONE")}}
	for _, item := range required {
		if strings.TrimSpace(item.value) == "" {
			return Config{}, fmt.Errorf("%s is required: %w", item.name, ErrInvalid)
		}
	}
	for _, secret := range []struct{ name, value string }{{"IIKO_CLIENT_SECRET", c.ClientSecret}, {"IIKO_API_KEY", c.APIKey}, {"TELEGRAM_BOT_TOKEN", c.TelegramToken}, {"OPENAI_API_KEY", c.OpenAIKey}, {"WEBHOOK_SECRET", c.WebhookSecret}} {
		if strings.ContainsAny(secret.value, "\r\n") {
			return Config{}, fmt.Errorf("%s must not contain line breaks: %w", secret.name, ErrInvalid)
		}
	}
	if strings.ContainsAny(c.TelegramToken, "/ ?#\t") {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN contains invalid characters: %w", ErrInvalid)
	}
	if c.OpenAIKey != "" && strings.TrimSpace(c.OpenAIModel) == "" {
		return Config{}, fmt.Errorf("OPENAI_MODEL is required when OPENAI_API_KEY is set: %w", ErrInvalid)
	}
	if c.OpenAIKey != "" && strings.TrimSpace(c.OpenAIVisionModel) == "" {
		return Config{}, fmt.Errorf("OPENAI_VISION_MODEL is required when OPENAI_API_KEY is set: %w", ErrInvalid)
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("IIKO_BASE_URL must be an HTTPS origin: %w", ErrInvalid)
	}
	if u.Path != "" && u.Path != "/" {
		return Config{}, fmt.Errorf("IIKO_BASE_URL must not contain a path: %w", ErrInvalid)
	}
	if !uuid(c.AppID) || !uuid(c.OrganizationID) {
		return Config{}, fmt.Errorf("IIKO_APP_ID and IIKO_ORGANIZATION_ID must be UUIDs: %w", ErrInvalid)
	}
	seen := map[int64]bool{}
	for _, value := range list(os.Getenv("TELEGRAM_ALLOWED_USER_IDS")) {
		id, e := strconv.ParseInt(value, 10, 64)
		if e != nil || id <= 0 {
			return Config{}, fmt.Errorf("TELEGRAM_ALLOWED_USER_IDS must contain positive integers: %w", ErrInvalid)
		}
		if !seen[id] {
			c.AllowedUserIDs = append(c.AllowedUserIDs, id)
			seen[id] = true
		}
	}
	if len(c.AllowedUserIDs) == 0 {
		return Config{}, fmt.Errorf("TELEGRAM_ALLOWED_USER_IDS is empty: %w", ErrInvalid)
	}
	c.ReportChatID, err = strconv.ParseInt(os.Getenv("TELEGRAM_REPORT_CHAT_ID"), 10, 64)
	if err != nil || c.ReportChatID == 0 {
		return Config{}, fmt.Errorf("TELEGRAM_REPORT_CHAT_ID must be a nonzero integer: %w", ErrInvalid)
	}
	// Automatic reports may intentionally target a configured staff group. Interactive
	// reports stay in private chats and require an allowlisted sender.
	if c.ReportChatID > 0 && !seen[c.ReportChatID] {
		return Config{}, fmt.Errorf("private TELEGRAM_REPORT_CHAT_ID must be allowlisted: %w", ErrInvalid)
	}
	c.Location, err = time.LoadLocation(os.Getenv("IIKO_TIMEZONE"))
	if err != nil {
		return Config{}, fmt.Errorf("IIKO_TIMEZONE is unknown: %w", ErrInvalid)
	}
	for _, d := range []struct {
		name, def string
		min, max  time.Duration
		dest      *time.Duration
	}{{"SHIFT_POLL_INTERVAL", "5m", time.Minute, time.Hour, &c.PollInterval}, {"HTTP_TIMEOUT", "60s", time.Second, 5 * time.Minute, &c.HTTPTimeout}, {"IIKO_REQUEST_INTERVAL", "500ms", 100 * time.Millisecond, time.Minute, &c.RequestInterval}, {"OLAP_DELAY", "30m", 0, 24 * time.Hour, &c.OLAPDelay}} {
		v, e := time.ParseDuration(optional(d.name, d.def))
		if e != nil || v < d.min || v > d.max {
			return Config{}, fmt.Errorf("%s is outside allowed duration range: %w", d.name, ErrInvalid)
		}
		*d.dest = v
	}
	c.AutoReports, err = strconv.ParseBool(optional("AUTO_REPORTS_ENABLED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("AUTO_REPORTS_ENABLED must be boolean: %w", ErrInvalid)
	}
	c.DailyReports, err = strconv.ParseBool(optional("DAILY_REPORTS_ENABLED", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("DAILY_REPORTS_ENABLED must be boolean: %w", ErrInvalid)
	}
	c.DailyHour, err = strconv.Atoi(optional("DAILY_REPORT_HOUR", "1"))
	if err != nil || c.DailyHour < 1 || c.DailyHour > 23 {
		return Config{}, fmt.Errorf("DAILY_REPORT_HOUR must be 1..23 (after the planned evening window): %w", ErrInvalid)
	}
	// REPORT_RECOVERY_DAYS is a legacy setting; daily mode never replays old dates.

	if filepath.IsAbs(c.StateFile) || strings.HasPrefix(filepath.Clean(c.StateFile), "..") || filepath.Clean(c.StateFile) == "." {
		return Config{}, fmt.Errorf("STATE_FILE must be a relative path inside the project: %w", ErrInvalid)
	}
	if c.WebhookListen != "" {
		host, portText, e := net.SplitHostPort(c.WebhookListen)
		port, portErr := strconv.Atoi(portText)
		if e != nil || portErr != nil || port < 1 || port > 65535 || (host != "127.0.0.1" && host != "::1") || len(c.WebhookSecret) < 32 {
			return Config{}, fmt.Errorf("WEBHOOK_LISTEN must be a loopback address with port 1..65535 and WEBHOOK_SECRET at least 32 bytes: %w", ErrInvalid)
		}
	}
	return c, nil
}
func optional(key, def string) string {
	if s, ok := os.LookupEnv(key); ok {
		return s
	}
	return def
}

// Comma-separated configuration accepts surrounding whitespace and empty separators.
// Individual opaque values (tokens, identifiers) are never normalized.
func list(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func uuid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func envKey(key string) bool {
	if key == "" {
		return false
	}
	for index, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
