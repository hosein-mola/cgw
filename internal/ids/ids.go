package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a cryptographically random, URL-safe identifier.
func New(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
