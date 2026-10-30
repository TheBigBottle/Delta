package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse") || !strings.HasPrefix(hash, passwordHashScheme+"$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if !verifyPassword("correct horse", hash) {
		t.Fatal("correct password rejected")
	}
	for _, wrong := range []string{"", "correct hors", "Correct horse"} {
		if verifyPassword(wrong, hash) {
			t.Fatalf("wrong password %q accepted", wrong)
		}
	}
	if verifyPassword("anything", "") || verifyPassword("anything", "garbage$1$x$y") {
		t.Fatal("empty or malformed hash must never match")
	}
	if other, _ := hashPassword("correct horse"); other == hash {
		t.Fatal("hashes of the same password must differ (random salt)")
	}
}

// A plain text password written into the config (old versions, or a manual
// reset) is replaced by its hash when Delta starts.
func TestPlainTextPasswordIsHashedOnLoad(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	os.WriteFile(path, []byte(`{"password": "my-secret-pw"}`), 0o600)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "" || !verifyPassword("my-secret-pw", cfg.PasswordHash) {
		t.Fatalf("password not migrated: %+v", cfg)
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "my-secret-pw") || !strings.Contains(string(saved), "password_hash") {
		t.Fatalf("config file still contains the plain text password:\n%s", saved)
	}
}

// A new config has no password; the setup can be done exactly once.
func TestInitialPasswordSetup(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(path, cfg)
	if !m.NeedsPasswordSetup() {
		t.Fatal("a new config must require password setup")
	}
	if _, err := m.CompleteSetup(InitialSetup{Password: "short"}); err == nil {
		t.Fatal("too short password accepted")
	}
	if _, err := m.CompleteSetup(InitialSetup{Password: "first-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteSetup(InitialSetup{Password: "takeover-attempt"}); err == nil {
		t.Fatal("setup must not work once a password is set")
	}
	reloaded, _ := loadConfig(path)
	if !verifyPassword("first-password", reloaded.PasswordHash) {
		t.Fatal("password not persisted")
	}
}

// The first-start page saves its settings together with the password; an
// invalid value saves nothing.
func TestInitialSetupSettings(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(path, cfg)
	if _, err := m.CompleteSetup(InitialSetup{Password: "first-password", WebhookURL: "ftp://x"}); err == nil {
		t.Fatal("an invalid webhook URL was accepted")
	}
	if !m.NeedsPasswordSetup() {
		t.Fatal("a failed setup must not set the password")
	}
	restartNeeded, err := m.CompleteSetup(InitialSetup{Password: "first-password", Port: 18999, Bind: "127.0.0.1",
		SessionHours: 8, WebhookURL: "https://example.org/hook", DownAfterSeconds: 60, EventLog: "logs/events.log", EventLogMaxMB: 3})
	if err != nil || !restartNeeded {
		t.Fatalf("setup: err %v, restart needed %v", err, restartNeeded)
	}
	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Port != 18999 || reloaded.Bind != "127.0.0.1" || reloaded.SessionHours != 8 || reloaded.Alerts.WebhookURL != "https://example.org/hook" ||
		reloaded.Alerts.DownAfterSeconds != 60 || reloaded.EventLog != "logs/events.log" || reloaded.EventLogMaxMB != 3 {
		t.Fatalf("settings not saved: %+v", reloaded)
	}
	if got := m.events.FilePath(); got != filepath.Join(filepath.Dir(path), "logs/events.log") {
		t.Fatalf("event log file = %q", got)
	}
	m.events.CloseFile()
}
