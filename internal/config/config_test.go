package config_test

import (
	"greenroom-ai/internal/config"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLiteralDotEnvPreservesEnvironmentAndSecrets(t *testing.T) {
	t.Setenv("EXISTING_VALUE", "environment")
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("EXISTING_VALUE=file\nNEW_LITERAL='$(do-not-execute)'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("NEW_LITERAL") })
	if err := config.LoadEnv(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("EXISTING_VALUE") != "environment" || os.Getenv("NEW_LITERAL") != "$(do-not-execute)" {
		t.Fatal("values must be read literally with environment precedence")
	}
}
func TestMissingSecretsFailWithoutEchoingTheirValues(t *testing.T) {
	t.Setenv("IIKO_BASE_URL", "")
	_, err := config.Load()
	if err == nil {
		t.Fatal("missing required endpoint must fail")
	}
}

func validEnvironment(t *testing.T) {
	t.Helper()
	values := map[string]string{
		"IIKO_BASE_URL": "https://api-ru.iiko.services", "IIKO_APP_ID": "1b965b7e-ecea-4484-a071-a5cf4b642107", "IIKO_ORGANIZATION_ID": "0b1f501f-abe3-49ad-a62f-69a2ab249ff9", "IIKO_CLIENT_SECRET": "client-secret", "IIKO_API_KEY": "iiko-secret", "TELEGRAM_BOT_TOKEN": "123:test-token", "TELEGRAM_ALLOWED_USER_IDS": "12, 12, ,34", "TELEGRAM_REPORT_CHAT_ID": "12", "IIKO_TIMEZONE": "Europe/Moscow", "SHIFT_POLL_INTERVAL": "5m", "AUTO_REPORTS_ENABLED": "false", "DAILY_REPORTS_ENABLED": "true", "DAILY_REPORT_HOUR": "1", "OLAP_DELAY": "30m", "REPORT_RECOVERY_DAYS": "3", "HTTP_TIMEOUT": "60s", "IIKO_REQUEST_INTERVAL": "500ms", "STATE_FILE": "run/state.json", "DESSERT_GROUPS": "Десерты, Десерты из кухни", "DESSERT_PRODUCT_IDS": "", "OPENAI_API_KEY": "", "OPENAI_MODEL": "gpt-4.1-mini", "OPENAI_VISION_MODEL": "gpt-4.1-mini", "WEBHOOK_LISTEN": "", "WEBHOOK_SECRET": "",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}
func TestValidConfigurationDeduplicatesAllowedUsers(t *testing.T) {
	validEnvironment(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedUserIDs) != 2 || cfg.AllowedUserIDs[0] != 12 || cfg.AllowedUserIDs[1] != 34 {
		t.Fatal("allowed IDs must be deduplicated in input order")
	}
	if len(cfg.DessertGroups) != 2 || cfg.DessertGroups[1] != "Десерты из кухни" {
		t.Fatal("comma-separated category whitespace must be explicit syntax")
	}
}
func TestInvalidConfigurationNamesFieldsWithoutEchoingValues(t *testing.T) {
	cases := []struct{ name, key, value string }{
		{"required secret", "IIKO_CLIENT_SECRET", ""}, {"HTTPS endpoint", "IIKO_BASE_URL", "http://secret-user:secret-password@example.invalid"},
		{"UUID app", "IIKO_APP_ID", "not-an-app-id-secret"}, {"nonzero recipient", "TELEGRAM_REPORT_CHAT_ID", "0"}, {"allowlisted recipient", "TELEGRAM_REPORT_CHAT_ID", "9999"},
		{"positive allowed users", "TELEGRAM_ALLOWED_USER_IDS", "-100"}, {"known timezone", "IIKO_TIMEZONE", "Unknown/SecretCity"},
		{"bounded polling", "SHIFT_POLL_INTERVAL", "500ms"}, {"bounded HTTP", "HTTP_TIMEOUT", "0s"}, {"bounded retry interval", "IIKO_REQUEST_INTERVAL", "0s"}, {"nonnegative OLAP delay", "OLAP_DELAY", "-1m"},
		{"boolean enabled", "AUTO_REPORTS_ENABLED", "certainly"}, {"boolean daily", "DAILY_REPORTS_ENABLED", "certainly"}, {"daily after evening", "DAILY_REPORT_HOUR", "0"},
		{"local state", "STATE_FILE", "../outside-secret.json"}, {"private webhook", "WEBHOOK_LISTEN", "0.0.0.0:8090"}, {"numeric webhook port", "WEBHOOK_LISTEN", "127.0.0.1:secret-port"},
		{"token line breaks", "TELEGRAM_BOT_TOKEN", "secret-token\nline"}, {"invalid token path", "TELEGRAM_BOT_TOKEN", "secret/token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tc.key, tc.value)
			_, err := config.Load()
			if err == nil {
				t.Fatal("invalid configuration was accepted")
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error exposed config value for %s", tc.key)
			}
		})
	}
}
func TestOptionalAssistantNeedsModelOnlyWhenEnabled(t *testing.T) {
	validEnvironment(t)
	t.Setenv("OPENAI_MODEL", "")
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	if _, err := config.Load(); err == nil {
		t.Fatal("enabled AI needs a model")
	}
}
func TestWebhookRequiresPrivateAddressPortAndSecret(t *testing.T) {
	validEnvironment(t)
	t.Setenv("WEBHOOK_LISTEN", "127.0.0.1:8090")
	t.Setenv("WEBHOOK_SECRET", strings.Repeat("s", 32))
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBHOOK_SECRET", "short")
	if _, err := config.Load(); err == nil {
		t.Fatal("webhook must reject a short secret")
	}
}
func TestDotEnvRejectsInvalidKeysWithoutEchoingSecretLines(t *testing.T) {
	for _, line := range []string{"9INVALID=hidden-secret", "BAD-KEY=hidden-secret", "MISSING_SEPARATOR_hidden-secret", "KEY='hidden-secret"} {
		t.Run(strconv.Itoa(len(line)), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(p, []byte(line+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := config.LoadEnv(p)
			if err == nil {
				t.Fatal("invalid env syntax accepted")
			}
			if strings.Contains(err.Error(), "hidden-secret") {
				t.Fatal("secret leaked in parser error")
			}
		})
	}
}

func TestVisionModelIsOptionalUntilAIIsEnabled(t *testing.T) {
	validEnvironment(t)
	t.Setenv("OPENAI_VISION_MODEL", "")
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	if _, err := config.Load(); err == nil {
		t.Fatal("enabled photo recognition requires a model")
	}
	t.Setenv("OPENAI_VISION_MODEL", "gpt-4.1-mini")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIVisionModel != "gpt-4.1-mini" {
		t.Fatal("photo model was not retained")
	}
}
func TestVisionModelDefaultsToConfiguredTextModel(t *testing.T) {
	validEnvironment(t)
	if err := os.Unsetenv("OPENAI_VISION_MODEL"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_MODEL", "gpt-4.1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIVisionModel != cfg.OpenAIModel {
		t.Fatal("absent photo model must inherit configured main model")
	}
}

func TestLegacyRecoverySettingDoesNotEnableMultiDayReports(t *testing.T) {
	validEnvironment(t)
	t.Setenv("REPORT_RECOVERY_DAYS", "obsolete-unused-value")
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
}
