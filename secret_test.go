package cf_configuration

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	cf_logs "github.com/caerus-framework/caerus-framework-logs"
)

type secretSample struct {
	Host     string `json:"host"`
	Password string `json:"password" secret:"redact"`
	APIKey   string `json:"api_key" secret:"redact"`
	Port     int    `json:"port"`
}

func TestLogArgsNeverCleartext(t *testing.T) {
	cfg := secretSample{Host: "db.example", Password: "s3cret-value", APIKey: "re_live", Port: 5432}
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	l.Info("summary", LogArgs(cfg)...)
	out := buf.String()
	if strings.Contains(out, "s3cret-value") || strings.Contains(out, "re_live") {
		t.Fatalf("cleartext leaked: %s", out)
	}
	if !strings.Contains(out, cf_logs.RedactedPlaceholder) {
		t.Fatalf("want %s in %s", cf_logs.RedactedPlaceholder, out)
	}
	if !strings.Contains(out, "host=db.example") || !strings.Contains(out, "port=5432") {
		t.Fatalf("unmarked fields should stay visible: %s", out)
	}
	if !strings.Contains(out, "password_set=true") || !strings.Contains(out, "api_key_set=true") {
		t.Fatalf("want presence flags: %s", out)
	}
}

func TestSecretPresenceOmitsUnmarked(t *testing.T) {
	cfg := secretSample{Host: "db.example", Password: "s3cret-value"}
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	l.Info("summary", SecretPresence(cfg)...)
	out := buf.String()
	if strings.Contains(out, "s3cret-value") {
		t.Fatalf("cleartext leaked: %s", out)
	}
	if strings.Contains(out, "host=") {
		t.Fatalf("presence helper should not dump unmarked fields: %s", out)
	}
	if !strings.Contains(out, "password_set=true") {
		t.Fatalf("want password_set: %s", out)
	}
}

func TestLogArgsEmptySecret(t *testing.T) {
	cfg := secretSample{Host: "db.example"}
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	l.Info("summary", LogArgs(cfg)...)
	out := buf.String()
	if strings.Contains(out, cf_logs.RedactedPlaceholder) {
		t.Fatalf("empty secret should not print placeholder: %s", out)
	}
	if !strings.Contains(out, "password_set=false") {
		t.Fatalf("want password_set=false: %s", out)
	}
}
