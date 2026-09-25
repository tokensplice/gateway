package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// BYOK key material is encrypted at rest with AES-256-GCM under a key derived
// per user, so a database dump on its own never exposes a customer's upstream
// credential and one user's derived key cannot decrypt another user's record.
//
// Derivation is HKDF-SHA256(masterKey=CRYPTO_SECRET, salt=big-endian user id,
// info="byok-v1", length=32). The same user id bytes are passed as GCM
// additional authenticated data, which binds every ciphertext to its owner:
// copying a row between users fails authentication instead of decrypting.
//
// OPERATIONAL REQUIREMENT: CRYPTO_SECRET must be a stable, configured secret.
// When it is unset the process falls back to a random per-boot value (see
// init.go), and every stored BYOK key becomes permanently undecryptable after
// a restart. The ciphertext format is versioned through the HKDF info string;
// bump byokHKDFInfo rather than changing the layout in place.
const (
	byokHKDFInfo = "byok-v1"
	byokKeyBytes = 32 // AES-256
	byokNonceLen = 12 // 96-bit nonce, the GCM standard size

	// ByokMaxSecretLen bounds the plaintext a caller may submit, so a large
	// body cannot be turned into an unbounded encryption allocation.
	ByokMaxSecretLen = 4096
	// ByokKeyHintChars is how many trailing characters are kept for display.
	ByokKeyHintChars = 4
)

var (
	errByokNoMasterSecret   = errors.New("byok: CRYPTO_SECRET is not configured")
	errByokInvalidUserID    = errors.New("byok: user id must be positive")
	errByokEmptySecret      = errors.New("byok: secret is empty")
	errByokSecretTooLong    = fmt.Errorf("byok: secret exceeds %d characters", ByokMaxSecretLen)
	errByokMalformedCipher  = errors.New("byok: stored ciphertext is malformed")
	errByokDecryptFailed    = errors.New("byok: decryption failed (record is corrupted or CRYPTO_SECRET changed)")
	errByokHintInputInvalid = errors.New("byok: cannot build a hint from an empty secret")
)

// byokUserIDBytes is the HKDF salt and the GCM AAD. Both use one encoding so
// the binding cannot drift apart.
func byokUserIDBytes(userID int64) []byte {
	buffer := make([]byte, 8)
	binary.BigEndian.PutUint64(buffer, uint64(userID))
	return buffer
}

func byokAEAD(userID int64) (cipher.AEAD, []byte, error) {
	if userID <= 0 {
		return nil, nil, errByokInvalidUserID
	}
	if CryptoSecret == "" {
		return nil, nil, errByokNoMasterSecret
	}
	derived, err := hkdf.Key(sha256.New, []byte(CryptoSecret), byokUserIDBytes(userID), byokHKDFInfo, byokKeyBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("byok: failed to derive encryption key: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, nil, fmt.Errorf("byok: failed to initialize cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("byok: failed to initialize GCM: %w", err)
	}
	return aead, byokUserIDBytes(userID), nil
}

// ByokEncryptSecret seals an upstream API key for the given user and returns
// base64(nonce || ciphertext || tag). The plaintext is never logged and never
// returned.
func ByokEncryptSecret(userID int64, secret string) (string, error) {
	aead, aad, err := byokAEAD(userID)
	if err != nil {
		return "", err
	}
	plaintext := strings.TrimSpace(secret)
	if plaintext == "" {
		return "", errByokEmptySecret
	}
	if len(plaintext) > ByokMaxSecretLen {
		return "", errByokSecretTooLong
	}
	nonce := make([]byte, byokNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("byok: failed to generate nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), aad)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// ByokDecryptSecret reverses ByokEncryptSecret. Callers must treat the result
// as sensitive: use it only to build an upstream request and never write it to
// a log, an audit record, or an API response.
func ByokDecryptSecret(userID int64, encoded string) (string, error) {
	aead, aad, err := byokAEAD(userID)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errByokMalformedCipher
	}
	if len(raw) < byokNonceLen+aead.Overhead() {
		return "", errByokMalformedCipher
	}
	plaintext, err := aead.Open(nil, raw[:byokNonceLen], raw[byokNonceLen:], aad)
	if err != nil {
		// Deliberately generic: distinguishing "wrong user" from "wrong
		// master secret" would give an attacker a decryption oracle.
		return "", errByokDecryptFailed
	}
	return string(plaintext), nil
}

// ByokKeyHint returns the trailing characters of a secret, safe to store
// beside the ciphertext and show in a key list. Short secrets are fully
// masked so the hint can never reconstruct the whole value.
func ByokKeyHint(secret string) (string, error) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return "", errByokHintInputInvalid
	}
	runes := []rune(trimmed)
	if len(runes) <= ByokKeyHintChars*2 {
		return strings.Repeat("*", len(runes)), nil
	}
	return string(runes[len(runes)-ByokKeyHintChars:]), nil
}
