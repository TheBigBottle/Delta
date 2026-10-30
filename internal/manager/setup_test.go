package manager

import (
	"path/filepath"
	"testing"

	"delta/internal/config"
)

// A new config has no password; the setup can be done exactly once.
func TestInitialPasswordSetup(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(path, cfg)
	if !m.NeedsPasswordSetup() {
		t.Fatal("a new config must require password setup")
	}
	if _, err := m.CompleteSetup(config.InitialSetup{Password: "short"}); err == nil {
		t.Fatal("too short password accepted")
	}
	if _, err := m.CompleteSetup(config.InitialSetup{Password: "first-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteSetup(config.InitialSetup{Password: "takeover-attempt"}); err == nil {
		t.Fatal("setup must not work once a password is set")
	}
	reloaded, _ := config.Load(path)
	if !config.VerifyPassword("first-password", reloaded.PasswordHash) {
		t.Fatal("password not persisted")
	}
}

// The first-start page saves its settings together with the password; an
// invalid value saves nothing.
func TestInitialSetupSettings(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(path, cfg)
	if _, err := m.CompleteSetup(config.InitialSetup{Password: "first-password", WebhookURL: "ftp://x"}); err == nil {
		t.Fatal("an invalid webhook URL was accepted")
	}
	if !m.NeedsPasswordSetup() {
		t.Fatal("a failed setup must not set the password")
	}
	restartNeeded, err := m.CompleteSetup(config.InitialSetup{Password: "first-password", Port: 18999, Bind: "127.0.0.1",
		SessionHours: 8, WebhookURL: "https://example.org/hook", DownAfterSeconds: 60, EventLog: "logs/events.log", EventLogMaxMB: 3})
	if err != nil || !restartNeeded {
		t.Fatalf("setup: err %v, restart needed %v", err, restartNeeded)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Port != 18999 || reloaded.Bind != "127.0.0.1" || reloaded.SessionHours != 8 || reloaded.Alerts.WebhookURL != "https://example.org/hook" ||
		reloaded.Alerts.DownAfterSeconds != 60 || reloaded.EventLog != "logs/events.log" || reloaded.EventLogMaxMB != 3 {
		t.Fatalf("settings not saved: %+v", reloaded)
	}
	if got := m.Events.FilePath(); got != filepath.Join(filepath.Dir(path), "logs/events.log") {
		t.Fatalf("event log file = %q", got)
	}
	m.Events.CloseFile()
}
