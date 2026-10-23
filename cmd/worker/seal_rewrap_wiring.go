package main

// seal_rewrap_wiring.go — the composition-root adapter for the operator-driven DEK rewrap lane (TG-163).
//
// temporal/ does not import core/db (the dependency direction the whole tree keeps), so configwrite
// declares its own WrappedDEKRow and the pgx store is adapted HERE, the same way opClassOverlayBackend
// adapts the ratified-grant store. Two identical structs is the price of that direction; the translation
// is three fields and it is in one place.

import (
	"context"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/configwrite"
)

// sealRewrapStore adapts *db.SealedSecretStore to configwrite.SecretRewrapper.
type sealRewrapStore struct{ s *db.SealedSecretStore }

func (b sealRewrapStore) ListWrappedDEKs(ctx context.Context, afterName string) ([]configwrite.WrappedDEKRow, error) {
	rows, err := b.s.ListWrappedDEKs(ctx, afterName)
	if err != nil {
		return nil, err
	}
	out := make([]configwrite.WrappedDEKRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, configwrite.WrappedDEKRow{Name: r.Name, WrappedDEK: r.WrappedDEK, DEKNonce: r.DEKNonce})
	}
	return out, nil
}

// RewrapDEK passes the conditional swap straight through — the CONDITION (old bytes must still be the
// stored bytes) lives in the SQL, which is the only place it can be atomic with the write.
func (b sealRewrapStore) RewrapDEK(ctx context.Context, name string, oldWrapped, oldNonce, newWrapped, newNonce []byte) (bool, error) {
	return b.s.RewrapDEK(ctx, name, oldWrapped, oldNonce, newWrapped, newNonce)
}
