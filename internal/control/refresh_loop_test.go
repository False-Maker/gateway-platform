package control

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/elucid/gateway-platform/internal/control/credentials"
)

func TestNewRefreshLoopValidatesCredentialKey(t *testing.T) {
	if _, err := NewRefreshLoop(nil, "not-base64"); err == nil {
		t.Fatal("invalid base64 key was accepted")
	}
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := NewRefreshLoop(nil, shortKey); !errors.Is(err, credentials.ErrInvalidKey) {
		t.Fatalf("got %v, want invalid key", err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	loop, err := NewRefreshLoop(nil, key)
	if err != nil || loop.Service.Cipher == nil {
		t.Fatalf("loop=%#v err=%v", loop, err)
	}
}
