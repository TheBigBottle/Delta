package config

import (
	"os"
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse") || !strings.HasPrefix(hash, passwordHashScheme+"$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if !VerifyPassword("correct horse", hash) {
		t.Fatal("correct password rejected")
	}
	for _, wrong := range []string{"", "correct hors", "Correct horse"} {
		if VerifyPassword(wrong, hash) {
			t.Fatalf("wrong password %q accepted", wrong)
		}
	}
	if VerifyPassword("anything", "") || VerifyPassword("anything", "garbage$1$x$y") {
		t.Fatal("empty or malformed hash must never match")
	}
	if other, _ := HashPassword("correct horse"); other == hash {
		t.Fatal("hashes of the same password must differ (random salt)")
	}
}

// A plain text password written into the config (old versions, or a manual
// reset) is replaced by its hash when Delta starts.
func TestPlainTextPasswordIsHashedOnLoad(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	os.WriteFile(path, []byte(`{"password": "my-secret-pw"}`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "" || !VerifyPassword("my-secret-pw", cfg.PasswordHash) {
		t.Fatalf("password not migrated: %+v", cfg)
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "my-secret-pw") || !strings.Contains(string(saved), "password_hash") {
		t.Fatalf("config file still contains the plain text password:\n%s", saved)
	}
}
