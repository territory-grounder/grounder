package main

import (
	"context"
	"time"
)

// memNonces is a throwaway in-memory nonce store for the signer oracles (a fresh map per value, so every test
// sees every nonce as fresh).
type memNonces map[string]bool

func (m memNonces) Claim(_ context.Context, nonce, _ string, _ time.Time) (bool, error) {
	if m[nonce] {
		return false, nil
	}
	m[nonce] = true
	return true, nil
}
