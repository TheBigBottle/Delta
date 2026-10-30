package config

// Passwords are never stored in plain text. The config file only contains a
// salted PBKDF2-SHA256 hash. Hashing is one-way: Delta can check whether a
// typed password is correct, but nobody can read the password back from the file.
//
// Stored format: "pbkdf2-sha256$<iterations>$<salt, base64>$<hash, base64>"

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	passwordHashScheme     = "pbkdf2-sha256"
	passwordHashIterations = 600_000 // OWASP recommendation for PBKDF2-SHA256; ~0.2 s per check
	passwordSaltBytes      = 16
	passwordKeyBytes       = 32
	minPasswordLength      = 8
)

// HashPassword returns the encoded hash of password with a new random salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordHashIterations, passwordKeyBytes)
	if err != nil {
		return "", err
	}
	encode := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("%s$%d$%s$%s", passwordHashScheme, passwordHashIterations, encode(salt), encode(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash.
// It returns false for an empty or malformed hash.
func VerifyPassword(password, encodedHash string) bool {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 4 || parts[0] != passwordHashScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return false
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(parts[2])
	expectedKey, keyErr := base64.RawStdEncoding.DecodeString(parts[3])
	if saltErr != nil || keyErr != nil || len(expectedKey) == 0 {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expectedKey))
	return err == nil && subtle.ConstantTimeCompare(key, expectedKey) == 1
}

// CheckNewPassword enforces the minimum requirements for a new password.
func CheckNewPassword(password string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("the password must have at least %d characters", minPasswordLength)
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("the password must not be only spaces")
	}
	return nil
}
