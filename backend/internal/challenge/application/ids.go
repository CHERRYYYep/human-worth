package application

import (
	"crypto/rand"
	"encoding/hex"
)

// newID keeps nondeterministic identity generation outside domain transitions.
func newID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
