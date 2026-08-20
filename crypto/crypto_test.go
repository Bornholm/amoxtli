package crypto

import (
	"bytes"
	"strings"
	"testing"
)

const testKey = "a-test-key-with-at-least-32-bytes!"

func TestSealOpen_RoundTrip(t *testing.T) {
	cipher, err := NewCipher(testKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	clear := []byte("the content of a personal note")

	sealed, err := cipher.Seal(clear)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("personal")) {
		t.Error("sealed value still carries the clear text")
	}
	if !IsSealed(sealed) {
		t.Error("sealed value does not carry the marker")
	}

	opened, err := cipher.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, clear) {
		t.Errorf("opened = %q, want %q", opened, clear)
	}
}

// A store written before encryption was enabled holds clear values: they
// must keep reading as is, or enabling the setting would silence history.
func TestOpen_PassesClearValuesThrough(t *testing.T) {
	cipher, err := NewCipher(testKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	clear := []byte("legacy clear content")
	opened, err := cipher.Open(clear)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, clear) {
		t.Errorf("opened = %q, want the untouched clear value", opened)
	}
}

func TestOpen_RejectsWrongKey(t *testing.T) {
	cipher, _ := NewCipher(testKey)
	other, _ := NewCipher(strings.Repeat("x", 32))

	sealed, err := cipher.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := other.Open(sealed); err == nil {
		t.Fatal("opening with the wrong key succeeded")
	}
}

func TestSeal_EmptyStaysEmpty(t *testing.T) {
	cipher, _ := NewCipher(testKey)

	sealed, err := cipher.Seal(nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(sealed) != 0 {
		t.Errorf("sealed empty value = %q, want empty", sealed)
	}
}

func TestNewCipher_RejectsShortKey(t *testing.T) {
	if _, err := NewCipher("short"); err == nil {
		t.Fatal("a short key was accepted")
	}
}
