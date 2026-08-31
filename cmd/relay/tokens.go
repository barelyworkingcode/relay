package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var (
	ErrNoToken      = errors.New("no token provided")
	ErrInvalidToken = errors.New("invalid token")
)

func hashToken(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}
