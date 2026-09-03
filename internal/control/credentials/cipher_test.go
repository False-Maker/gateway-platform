package credentials

import (
	"bytes"
	"errors"
	"testing"
)

func TestCipherRoundTripAndAccountBinding(t *testing.T) {
	cipher, err := NewCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Encrypt("account-1", []byte("refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := cipher.Decrypt("account-1", ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "refresh-token" {
		t.Fatalf("got %q", plaintext)
	}
	if _, err := cipher.Decrypt("account-2", ciphertext); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("cross-account decrypt got %v, want invalid ciphertext", err)
	}
}

func TestCipherRejectsTamperingAndInvalidKey(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v, want invalid key", err)
	}
	cipher, err := NewCipher(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.Encrypt("account-1", nil); !errors.Is(err, ErrInvalidPlaintext) {
		t.Fatalf("empty plaintext got %v, want invalid plaintext", err)
	}
	if _, err := cipher.Encrypt("account-1", make([]byte, MaxPlaintextBytes+1)); !errors.Is(err, ErrPlaintextTooLarge) {
		t.Fatalf("oversized plaintext got %v, want plaintext too large", err)
	}
	ciphertext, err := cipher.Encrypt("account-1", []byte("refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 0xff
	if _, err := cipher.Decrypt("account-1", ciphertext); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("tampered decrypt got %v, want invalid ciphertext", err)
	}
}
