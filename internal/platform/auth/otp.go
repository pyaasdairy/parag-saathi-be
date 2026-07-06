package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
)

// GenerateNumericOTP returns a cryptographically random n-digit code.
func GenerateNumericOTP(n int) (string, error) {
	if n <= 0 || n > 10 {
		return "", fmt.Errorf("otp length out of range: %d", n)
	}
	max := big.NewInt(1)
	for i := 0; i < n; i++ {
		max.Mul(max, big.NewInt(10))
	}
	v, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("otp entropy: %w", err)
	}
	return fmt.Sprintf("%0*d", n, v), nil
}

// HMACHash returns hex(HMAC-SHA256(secret, parts joined by '|')). Used for
// OTP codes (keyed to the phone) and refresh tokens, so a DB leak alone
// reveals no usable credentials.
func HMACHash(secret string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for i, p := range parts {
		if i > 0 {
			mac.Write([]byte{'|'})
		}
		mac.Write([]byte(p))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// ConstantTimeEqual compares two hex digests without timing leaks.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RandomToken returns a URL-safe random token with nBytes of entropy —
// used for opaque refresh tokens.
func RandomToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("token entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
