// Package crypto encrypts content at rest with AES-256-GCM.
//
// It protects the *content* of documents and blobs — the text of a note,
// the bytes of an image — never their envelope: identifiers, sources,
// metadata and timestamps stay clear, because stores filter and join on
// them. Encrypting the envelope would either break those queries or force
// deterministic encryption, which leaks equality.
//
// The encryption is applicative, not engine level: sealed values travel to
// the database as opaque bytes, so the same key protects a SQLite file
// today and a PostgreSQL cluster tomorrow.
//
// Threat model: a stolen database file, a misplaced backup, a resold disk.
// A compromised process holds the key and is out of scope.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"io"

	"github.com/pkg/errors"
	"golang.org/x/crypto/hkdf"
)

// marker prefixes every sealed value. It makes reads tolerant: a store
// written before encryption was enabled holds clear and sealed values side
// by side, and both keep working during a migration.
var marker = []byte("amx1:")

// derivationInfo binds the derived key to this usage: the same secret used
// elsewhere (session signing, another store) can never produce this key.
const derivationInfo = "amoxtli/content/v1"

// Cipher seals and opens content values.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher derives a Cipher from key. The key must carry at least 32
// bytes of material; it is stretched through HKDF-SHA256, never used raw.
func NewCipher(key string) (*Cipher, error) {
	if len(key) < 32 {
		return nil, errors.New("crypto: key too short (at least 32 bytes required)")
	}

	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(key), nil, []byte(derivationInfo)), derived); err != nil {
		return nil, errors.Wrap(err, "crypto: could not derive key")
	}

	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, errors.Wrap(err, "crypto: could not initialize cipher")
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.Wrap(err, "crypto: could not initialize GCM")
	}

	return &Cipher{aead: aead}, nil
}

// Seal encrypts value and prefixes it with the marker. Empty values stay
// empty: there is nothing to protect, and a marker would advertise a
// content where there is none.
func (c *Cipher) Seal(value []byte) ([]byte, error) {
	if len(value) == 0 {
		return value, nil
	}

	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.Wrap(err, "crypto: could not read randomness")
	}

	sealed := c.aead.Seal(nonce, nonce, value, nil)

	out := make([]byte, 0, len(marker)+len(sealed))
	out = append(out, marker...)
	out = append(out, sealed...)

	return out, nil
}

// Open decrypts a value produced by Seal. A value without the marker is
// returned as is: it predates encryption and is already clear.
func (c *Cipher) Open(value []byte) ([]byte, error) {
	if !IsSealed(value) {
		return value, nil
	}

	raw := value[len(marker):]

	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return nil, errors.New("crypto: truncated value")
	}

	clear, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return nil, errors.Wrap(err, "crypto: could not decrypt value (was the key changed?)")
	}

	return clear, nil
}

// IsSealed reports whether value carries the encryption marker. Migrations
// rely on it to never seal a value twice.
func IsSealed(value []byte) bool {
	return bytes.HasPrefix(value, marker)
}
