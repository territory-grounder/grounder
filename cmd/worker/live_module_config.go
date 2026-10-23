package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/cpconfig"
)

// LIVE MODULE CONFIG — the half that makes a Save take effect.
//
// Module settings are read once at boot in this file's neighbour, so a console write was durable and
// inert: the operator saw "saved" and nothing changed until the next deploy. A Save button on top of that
// is worse than no button, because it reports success it did not achieve.
//
// This reads the committed console overrides at USE time for the fields a module declares as live
// (desc.EffectLive), falling back to the boot value when no override exists.
//
// WHY A CACHE. The Matrix approver set is consulted on every inbound event and the room routing on every
// notice. Querying Postgres per call would put the config store on the hot path of the notification lane
// — the lane whose whole job is to work during an incident, which is exactly when the database is least
// healthy. A short TTL keeps a save visible within seconds while a burst costs one query.
//
// WHY THE STALE VALUE SURVIVES AN ERROR. If the store is unreachable the last known values stand rather
// than collapsing to empty. An empty approver set is not a safe default: it would refuse every vote, and
// a database blip would silently make every approval poll unanswerable.
type liveModuleConfig struct {
	store cpconfig.ConsoleStore
	ttl   time.Duration

	mu     sync.Mutex
	cached map[string]string
	at     time.Time
	ok     bool // a successful read has happened at least once
}

func newLiveModuleConfig(store cpconfig.ConsoleStore, ttl time.Duration) *liveModuleConfig {
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return &liveModuleConfig{store: store, ttl: ttl}
}

// overrides returns the current console overrides, refreshing at most once per TTL.
func (l *liveModuleConfig) overrides() map[string]string {
	if l == nil || l.store == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ok && time.Since(l.at) < l.ttl {
		return l.cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := l.store.Overrides(ctx)
	if err != nil {
		// Keep the last good values. See the type doc: an empty approver set would refuse every vote.
		return l.cached
	}
	l.cached, l.at, l.ok = got, time.Now(), true
	return l.cached
}

// value returns the override for a module config key, or the boot fallback.
//
// The override is honoured ONLY when the key is registered and console-writable. The store is a database
// table; a row that should not be there must not become configuration. cpconfig.Resolve applies the same
// clamp for the control plane, and this is the module-side equivalent — the clamp is the law, enforced
// wherever the value is read rather than only where it is written.
func (l *liveModuleConfig) value(key, fallback string) string {
	k, ok := cpconfig.Lookup(key)
	if !ok || k.Law || !k.ConsoleWritable {
		return fallback
	}
	if v, present := l.overrides()[key]; present && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// list resolves a comma-separated list key (e.g. the matrix approver set).
func (l *liveModuleConfig) list(key string, fallback []string) []string {
	raw := l.value(key, "")
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	out := make([]string, 0, 8)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// kvMap resolves a "name=value, name=value" key (e.g. routed rooms).
func (l *liveModuleConfig) kvMap(key string, fallback map[string]string) map[string]string {
	raw := l.value(key, "")
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		if i := strings.IndexByte(pair, '='); i > 0 {
			name := strings.TrimSpace(pair[:i])
			val := strings.TrimSpace(pair[i+1:])
			if name != "" && val != "" {
				out[name] = val
			}
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
