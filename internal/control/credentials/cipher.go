package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
)

const formatVersion byte = 1

const MaxPlaintextBytes = 1 << 20

var (
	ErrInvalidKey        = errors.New("credential encryption key must be 32 bytes")
	ErrInvalidPlaintext  = errors.New("credential plaintext is empty")
	ErrPlaintextTooLarge = errors.New("credential plaintext exceeds 1 MiB")
	ErrInvalidCiphertext = errors.New("invalid credential ciphertext")
)

type Cipher struct {
	aead cipher.AEAD
}

// NewCipher constructs an AES-256-GCM credential cipher. Key retrieval and
// rotation belong to the deployment boundary, not this package.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt binds the ciphertext to an account so encrypted_secret values
// cannot be swapped between account rows without detection.
func (c *Cipher) Encrypt(accountID string, plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil || strings.TrimSpace(accountID) == "" {
		return nil, ErrInvalidCiphertext
	}
	if len(plaintext) == 0 {
		return nil, ErrInvalidPlaintext
	}
	if len(plaintext) > MaxPlaintextBytes {
		return nil, ErrPlaintextTooLarge
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	out := make([]byte, 1, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	out[0] = formatVersion
	out = append(out, nonce...)
	out = c.aead.Seal(out, nonce, plaintext, additionalData(accountID))
	return out, nil
}

func (c *Cipher) Decrypt(accountID string, ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil || strings.TrimSpace(accountID) == "" || len(ciphertext) < 1+c.aead.NonceSize()+c.aead.Overhead() || len(ciphertext) > 1+c.aead.NonceSize()+MaxPlaintextBytes+c.aead.Overhead() {
		return nil, ErrInvalidCiphertext
	}
	if ciphertext[0] != formatVersion {
		return nil, ErrInvalidCiphertext
	}
	nonceEnd := 1 + c.aead.NonceSize()
	plaintext, err := c.aead.Open(nil, ciphertext[1:nonceEnd], ciphertext[nonceEnd:], additionalData(accountID))
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

func additionalData(accountID string) []byte {
	return []byte("gateway-platform/credential/v1/" + accountID)
}
