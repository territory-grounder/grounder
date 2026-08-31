package credential

import "sync"

// ObjectGroup is one operator-authored object group projected into the credential plane: a NAME and its
// host-glob PATTERNS (TG-481, spec/016). It is the credential-package view of core/db's EstateObjectGroupRow
// — the bootstrap loads the store rows and hands them here via SyncEngine.SetObjectGroups, so this package
// never imports core/db (layering stays one-directional).
type ObjectGroup struct {
	Name     string
	Patterns []string // host-glob patterns (leading/trailing '*'); a host matching ANY pattern is a member.
}

// objectGroupStore holds the hand-authored object groups and answers groupsFor(host) by GlobMatch. It is the
// object-group analog of membershipStore, but PATTERN-based (evaluated per host at Resolve) rather than a
// pre-enumerated host→groups map — a group "dc1*" covers every current and future matching host without
// re-enumeration. Its own RWMutex mirrors membership's ordering discipline: the lock order is always
// se.mu → objectGroupStore.mu, and set() takes ONLY this mutex, so it never deadlocks against Resolve.
type objectGroupStore struct {
	mu     sync.RWMutex
	groups []ObjectGroup
}

func newObjectGroupStore() *objectGroupStore { return &objectGroupStore{} }

// set REPLACES the whole group set atomically (the loader hands the full converged list, mirroring how a
// membership fetch replaces the prior index). A nil/empty list disarms the seam — groupsFor then returns
// nothing and Resolve is unchanged.
func (o *objectGroupStore) set(groups []ObjectGroup) {
	o.mu.Lock()
	o.groups = groups
	o.mu.Unlock()
}

// groupsFor returns the name of every object group whose host-glob patterns match the host. It ADDS
// membership (unioned into Target.Groups by the caller); it never masks the sync-derived membership (TG-481's
// union-never-mask semantics). Empty host or no groups → nil, so an unmatched host resolves EXACTLY as before.
func (o *objectGroupStore) groupsFor(host string) []string {
	if o == nil || host == "" {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	var out []string
	for _, g := range o.groups {
		for _, pat := range g.Patterns {
			if GlobMatch(pat, host) {
				out = append(out, g.Name)
				break
			}
		}
	}
	return out
}
