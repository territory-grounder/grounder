package db

import (
	"context"
	"fmt"
)

type ActuationDispatchStore struct{ p *Pool }

func NewActuationDispatchStore(p *Pool) *ActuationDispatchStore { return &ActuationDispatchStore{p: p} }

func (s *ActuationDispatchStore) ClaimDispatch(ctx context.Context, dispatchID, actionID, externalRef string) (bool, error) {
	tag, err := s.p.Exec(ctx, `
		INSERT INTO actuation_dispatch (dispatch_id, action_id, external_ref)
		VALUES ($1, $2, $3)
		ON CONFLICT (dispatch_id) DO NOTHING`, dispatchID, actionID, externalRef)
	if err != nil {
		return false, fmt.Errorf("db: claim dispatch %s: %w", dispatchID, err)
	}
	return tag.RowsAffected() == 1, nil
}
