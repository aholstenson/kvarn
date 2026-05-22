package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// tokenPrefix marks tokens as belonging to kvarn so secret scanners (GitHub
// secret scanning, trufflehog) can recognize a leaked credential.
const tokenPrefix = "kvarn"

// keyIDBytes / secretBytes are the CSPRNG widths of each token component. The
// secret carries 160 bits of entropy; the key ID only needs to be a collision-
// resistant lookup handle.
const (
	keyIDBytes  = 10
	secretBytes = 20
)

// b32 is lower-cased, unpadded base32. base64url contains '_', which would
// collide with the '_' token delimiter; base32 is stdlib and underscore-free.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateToken mints a new token. It returns the full token string (shown to
// the user once), the key ID (persisted as the lookup handle), and the hex
// SHA-256 of the secret part (persisted in place of the secret).
func GenerateToken() (token, keyID, hash string, err error) {
	idBytes := make([]byte, keyIDBytes)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", "", err
	}
	secBytes := make([]byte, secretBytes)
	if _, err = rand.Read(secBytes); err != nil {
		return "", "", "", err
	}

	keyID = strings.ToLower(b32.EncodeToString(idBytes))
	secret := strings.ToLower(b32.EncodeToString(secBytes))
	hash = HashSecret(secret)
	token = tokenPrefix + "_" + keyID + "_" + secret
	return token, keyID, hash, nil
}

// ParseToken splits a token into its key ID and secret. ok is false unless the
// token has the exact `kvarn_<keyid>_<secret>` shape with non-empty parts.
func ParseToken(token string) (keyID, secret string, ok bool) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != tokenPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// HashSecret returns the hex-encoded SHA-256 of the secret part. Plain SHA-256
// (no salt/KDF) is correct for a high-entropy random secret; bcrypt/argon2 only
// add value against low-entropy human passwords.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
