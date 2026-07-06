// Package auth owns tokens and credentials: JWT issue/verify, OTP generation
// and HMAC hashing, opaque refresh tokens. Nothing secret is ever stored in
// plaintext — OTPs and refresh tokens live in the DB only as HMAC-SHA256
// digests.
package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/pyaas/saathi-backend/internal/domain"
)

// Token kinds. A SESSION token proves "this phone logged in"; a ROLE token
// additionally pins one active role assignment + org scope (the role switcher
// simply issues a fresh ROLE token).
const (
	TokenKindSession = "session"
	TokenKindRole    = "role"
)

// Claims is the JWT payload.
type Claims struct {
	PartyID          string `json:"pid"`
	Phone            string `json:"phn"`
	KYCTier          string `json:"kyc"`
	Kind             string `json:"knd"`
	RoleAssignmentID string `json:"rid,omitempty"`
	RoleCode         string `json:"rol,omitempty"`
	OrgUnitID        string `json:"org,omitempty"`
	OrgType          string `json:"ogt,omitempty"`
	jwt.RegisteredClaims
}

// JWTManager issues and parses access tokens.
type JWTManager struct {
	secret []byte
	ttl    time.Duration
}

// NewJWTManager builds a manager with the shared HS256 secret and access TTL.
func NewJWTManager(secret string, ttl time.Duration) *JWTManager {
	return &JWTManager{secret: []byte(secret), ttl: ttl}
}

// IssueSessionToken returns a SESSION-kind access token for a logged-in party.
// ObjectIDs travel in claims as hex strings (JWTs are JSON).
func (m *JWTManager) IssueSessionToken(p domain.Party) (string, error) {
	return m.sign(Claims{
		PartyID: p.ID.Hex(),
		Phone:   p.Phone,
		KYCTier: p.KYCTier,
		Kind:    TokenKindSession,
	})
}

// IssueRoleToken returns a ROLE-kind access token pinned to one assignment.
func (m *JWTManager) IssueRoleToken(p domain.Party, ra domain.RoleAssignment, orgType string) (string, error) {
	return m.sign(Claims{
		PartyID:          p.ID.Hex(),
		Phone:            p.Phone,
		KYCTier:          p.KYCTier,
		Kind:             TokenKindRole,
		RoleAssignmentID: ra.ID.Hex(),
		RoleCode:         ra.RoleCode,
		OrgUnitID:        ra.OrgUnitID.Hex(),
		OrgType:          orgType,
	})
}

func (m *JWTManager) sign(c Claims) (string, error) {
	now := time.Now()
	c.RegisteredClaims = jwt.RegisteredClaims{
		Issuer:    "saathi-backend",
		Subject:   c.PartyID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
		ID:        uuid.NewString(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(m.secret)
}

// Parse validates signature+expiry and returns the claims.
func (m *JWTManager) Parse(token string) (*Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{},
		func(t *jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("saathi-backend"),
	)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}
