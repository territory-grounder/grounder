package actuate

import "context"

const RefusalDuplicateDispatch = "duplicate-dispatch"

type DispatchClaim interface {
	ClaimDispatch(ctx context.Context, dispatchID, actionID, externalRef string) (bool, error)
}

func (i *Interceptor) WithDispatchClaim(c DispatchClaim, identity func(context.Context) (string, bool)) *Interceptor {
	if c != nil && identity != nil {
		i.dispatchClaim, i.dispatchIdentity = c, identity
	}
	return i
}
