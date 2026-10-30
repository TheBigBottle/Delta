package main

import (
	"os"
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
	if err := m.SetInitialPassword("short"); err == nil {
		t.Fatal("too short password accepted")
	}
	if err := m.SetInitialPassword("first-password"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetInitialPassword("takeover-attempt"); err == nil {
		t.Fatal("setup must not work once a password is set")
	}
	reloaded, _ := loadConfig(path)
	if !verifyPassword("first-password", reloaded.PasswordHash) {
		t.Fatal("password not persisted")
	}
}
