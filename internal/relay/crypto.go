package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// DeriveKey derives a 32-byte AES-256-GCM key from the shared secret using HKDF-SHA256.
// The agentID is used as the salt.
func DeriveKey(secret, agentID string) ([]byte, error) {
	info := []byte("transitive-e2e-v1")
	salt := []byte(agentID)

	r := hkdf.New(sha256.New, []byte(secret), salt, info)

	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("hkdf derive key: %w", err)
	}
	return key, nil
}

// Encrypt encrypts plaintext using AES-256-GCM with a random nonce.
// Returns base64-encoded ciphertext and base64-encoded nonce.
func Encrypt(key, plaintext []byte) (ct string, iv string, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", fmt.Errorf("aes new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("gcm new: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	ct = base64.StdEncoding.EncodeToString(ciphertext)
	iv = base64.StdEncoding.EncodeToString(nonce)
	return ct, iv, nil
}

// Decrypt decrypts base64-encoded ciphertext and nonce using AES-256-GCM.
func Decrypt(key []byte, ct, iv string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}

	nonce, err := base64.StdEncoding.DecodeString(iv)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("gcm decrypt: %w", err)
	}

	return plaintext, nil
}
