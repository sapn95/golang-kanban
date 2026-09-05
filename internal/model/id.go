package model

import (
	"crypto/rand"
	"strings"
)

// NewID returns a random ID: 26 lower-case base32 characters, 130 bits of
// entropy, safe in URLs and element ids. Every backend stores it as-is.
func NewID() ID {
	return ID(strings.ToLower(rand.Text()))
}
