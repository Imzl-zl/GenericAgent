package application

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLen is the minimum accepted password length.
const MinPasswordLen = 6

// HashPassword returns a bcrypt hash of the plaintext password.
// Empty passwords are allowed for admin-created test accounts.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword verifies a plaintext password against a bcrypt hash.
// It returns an error for empty passwords when a hash is required.
func CheckPassword(password, hash string) error {
	if hash == "" {
		return fmt.Errorf("password not set")
	}
	if password == "" {
		return fmt.Errorf("password required")
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}
