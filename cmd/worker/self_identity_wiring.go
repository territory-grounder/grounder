package main

// TG'S OWN SSH IDENTITY ACROSS THE PLANE SPLIT (TG-684, owner ruling 2026-11-07).
//
// The triage plane runs attribution, but the credential split withholds the actuation private key from it, so it
// could not derive TG's own `root!SHA256:<fp>` and every TG heal login read attributed-suspicious the moment the
// journal reader could read (measured: TG's actuation key logs in 4–7 times a day on the sampled pool hosts). The
// owner's ruling keeps the identity DERIVED FROM THE KEY — never typed config, never a ruleset entry:
//
//   - the plane that HOLDS the key derives the identity at boot with the existing resolveSelfSSHActor and appends
//     it to self_identity (migration 0129; append-only; the triage DB role cannot write it, plane_roles.go);
//   - the triage plane reads the newest published identity and refreshes it on a cadence, so a worker that booted
//     before the actuation plane published picks it up without a restart;
//   - and a journal record naming that identity counts as TG's own only when TG's own execution record explains it
//     (journal.WithSelfCorrelation) — a fingerprint alone is not proof.
//
// THE SAME SPLIT HIT THE PVE DOMAIN (TG-693, live 2026-11-08 07:07Z). The pve self-actor is derived from the
// actuation TOKEN (resolveSelfActor), which the triage plane is also denied, so on a split deployment every task TG's
// own heal left in the PVE task log read as an unknown principal — attributed-suspicious — for the whole 30-minute
// lookback, and a pool guest re-faulted inside it was HELD instead of healed. The boot log said so on every start
// ("domain \"pve\" has an ARMED reader but NO self-actor"). The remedy is the journal's, domain for domain: the
// actuation plane publishes the token's principal (`user@realm!tokenid` — an identifier, never the secret), and a
// task under it counts as TG's own only where a TG-recorded Proxmox execution on the same guest explains it
// (pve.WithSelfCorrelation).

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
	pveattr "github.com/territory-grounder/grounder/modules/actorevidence/pve"
)

// selfIdentityRefresh is how often the triage plane re-reads the published identity. A key rotation reaches it
// within this; until it does, a rotated key's logins read unexplained (suspicious) — the safe direction.
const selfIdentityRefresh = time.Minute

// selfIdentityPublisher is the write seam (db.SelfIdentityStore in production).
type selfIdentityPublisher interface {
	Publish(ctx context.Context, domain, actor, plane string) (bool, error)
}

// selfIdentityReader is the read seam (db.SelfIdentityStore in production).
type selfIdentityReader interface {
	Latest(ctx context.Context, domain string) (string, bool, error)
}

// publishSelfIdentities appends TG's identity in each domain it actuates through when THIS process holds that
// domain's actuation credential (the actuation plane, or a single-process `both` deployment): the journal identity
// from the SSH key, the pve identity from the Proxmox token. On the triage plane both credentials are withheld, the
// identities resolve to "" and nothing is published — that plane only reads.
func publishSelfIdentities(pool *db.Pool, get func(k, def string) string) {
	var pub selfIdentityPublisher
	if pool != nil {
		pub = db.NewSelfIdentityStore(pool)
	}
	publishSelfIdentitiesTo(context.Background(), pub, get)
}

// publishSelfIdentitiesTo derives and publishes each domain's identity through pub. The pve identity is the token's
// PRINCIPAL (`user@realm!tokenid`, selfPrincipalFromToken) — the part of the token before "=", never its secret.
func publishSelfIdentitiesTo(ctx context.Context, pub selfIdentityPublisher, get func(k, def string) string) {
	startSelfIdentityPublisher(ctx, pub, "journal", resolveSelfSSHActor(get), string(credentialPlane), selfIdentityRetry)
	startSelfIdentityPublisher(ctx, pub, "pve", resolveSelfActor(get), string(credentialPlane), selfIdentityRetry)
}

// selfIdentityRetry is how often the actuation plane retries a publication that failed. ONE attempt at boot was not
// enough (live, 2026-11-08 00:35 UTC): on the deploy that introduced self_identity the actuation worker booted before
// the grounder applied migration 0129, the single publish failed ("relation does not exist"), and the triage plane
// read every TG heal login as indeterminate until the worker was restarted by hand. A database that is briefly
// unreachable at boot has the same effect.
const selfIdentityRetry = 30 * time.Second

// startSelfIdentityPublisher publishes once now and, while that FAILS, retries every `every` until it succeeds or ctx
// ends. It reports whether the first attempt settled the matter (published, already published, or nothing this plane
// can publish).
func startSelfIdentityPublisher(ctx context.Context, pub selfIdentityPublisher, domain, self, plane string, every time.Duration) bool {
	if publishSelfIdentity(ctx, pub, domain, self, plane) {
		return true
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if publishSelfIdentity(ctx, pub, domain, self, plane) {
					return
				}
			}
		}
	}()
	return false
}

// publishSelfIdentity makes one attempt. It returns false ONLY when a retry could help (the database refused or
// failed the write); a plane without the key, or a process without a database, has nothing a retry could change.
func publishSelfIdentity(ctx context.Context, pub selfIdentityPublisher, domain, self, plane string) bool {
	if self == "" {
		return true
	}
	if pub == nil {
		log.Printf("self identity: TG's %s identity resolved but there is NO database — the triage plane cannot learn it, "+
			"and its %s reads holding TG's own record will be indeterminate (TG-684/TG-693)", domain, domain)
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	added, err := pub.Publish(cctx, domain, self, plane)
	switch {
	case err != nil:
		log.Printf("self identity: publishing TG's %s identity FAILED: %v — retrying every %s; until it lands the triage "+
			"plane keeps the previously published identity, or none, and its %s reads holding TG's own record stay indeterminate (TG-684/TG-693)",
			domain, err, selfIdentityRetry, domain)
		return false
	case added:
		log.Printf("self identity: published TG's %s identity (derived from the actuation credential on the %s plane) — the triage "+
			"plane recognises TG's own %s records only where TG's execution record explains them (TG-684/TG-693)", domain, plane, domain)
	default:
		log.Printf("self identity: TG's %s identity is already published and unchanged (TG-684/TG-693)", domain)
	}
	return true
}

// journalSelfCorrelation is the journal reader's correlation option: the identity (the key's own derivation where
// this process holds it, the published identity otherwise) and the execution record. With no database it still
// arms the check — with no record to consult, any read holding TG's login fails closed.
func journalSelfCorrelation(pool *db.Pool, get func(k, def string) string) journal.Option {
	var (
		rd    selfIdentityReader
		times journal.ExecutionTimes
	)
	if pool != nil {
		rd = db.NewSelfIdentityStore(pool)
		times = explainingRecordTimes(sshLaneExecutionTimes(db.NewActionExecutionStore(pool).ExecutionsOn), db.NewActuationStateReadStore(pool).TimesOn)
	}
	return journal.WithSelfCorrelation(selfIdentityHolder(context.Background(), "journal", resolveSelfSSHActor(get), rd, selfIdentityRefresh), times)
}

// pveSelfCorrelation is the PVE reader's correlation option (TG-693): the identity (the actuation token's principal
// where this process holds the token, the actuation plane's publication otherwise) and the Proxmox-lane execution
// record. With no database it still arms the check — with no record to consult, any read holding TG's task fails
// closed.
func pveSelfCorrelation(pool *db.Pool, get func(k, def string) string) pveattr.Option {
	var (
		rd    selfIdentityReader
		times pveattr.ExecutionTimes
	)
	if pool != nil {
		rd = db.NewSelfIdentityStore(pool)
		times = proxmoxLaneExecutionTimes(db.NewActionExecutionStore(pool).ExecutionsSince)
	}
	return pveattr.WithSelfCorrelation(selfIdentityHolder(context.Background(), "pve", resolveSelfActor(get), rd, selfIdentityRefresh), times)
}

// executionsSinceLister is the read seam under proxmoxLaneExecutionTimes (db.ActionExecutionStore.ExecutionsSince).
type executionsSinceLister func(ctx context.Context, after time.Time, host string, limit int) ([]db.ExecutionSince, error)

// proxmoxLaneExecutionLimit bounds one read: a guest sees a handful of TG executions in an attribution window, so a
// full page means something is wrong and the read fails closed rather than pairing against a truncated record.
const proxmoxLaneExecutionLimit = 500

// proxmoxLaneExecutionTimes keeps only the executions whose effect went through the Proxmox API
// (opschema.EffectProxmoxLifecycle) and whose RESOURCE is the guest: the PVE task log records the guest, and a
// start-guest may be sealed as Target=<node> + params.guest=<guest> or as Target=<guest> (TG-664), so the match is on
// the sealed action's resource path, never on the free-text target. An SSH heal inside the guest never leaves a PVE
// task, so it explains none; an op-class the registry does not know (or a missing manifest) explains nothing either —
// the task then reads unexplained, the safe direction.
func proxmoxLaneExecutionTimes(list executionsSinceLister) pveattr.ExecutionTimes {
	return func(ctx context.Context, guest string, from, to time.Time) ([]time.Time, error) {
		all, err := list(ctx, from, guest, proxmoxLaneExecutionLimit)
		if err != nil {
			return nil, err
		}
		if len(all) >= proxmoxLaneExecutionLimit {
			return nil, fmt.Errorf("pve: %d or more executions on %q since %s — the record is truncated, refusing to pair against it",
				proxmoxLaneExecutionLimit, guest, from.UTC().Format(time.RFC3339))
		}
		want := actuate.ResourceHost(actuate.ResourcePath(manifest.Action{Target: guest}))
		var out []time.Time
		for _, e := range all {
			if e.At.After(to) {
				continue
			}
			spec, ok := opschema.Lookup(e.Action.OpClass)
			if !ok || spec.Kind() != opschema.EffectProxmoxLifecycle {
				continue
			}
			if actuate.ResourceHost(actuate.ResourcePath(e.Action)) == want {
				out = append(out, e.At)
			}
		}
		return out, nil
	}
}

// publishedSelfDomains are the domains whose self-identity reaches the triage plane through the actuation plane's
// publication (self_identity) rather than from a credential this process holds.
var publishedSelfDomains = map[string]bool{"journal": true, "pve": true}

// selfIdentityReaderFor is the publication reader for a pool, or nil (an untyped nil) without one.
func selfIdentityReaderFor(pool *db.Pool) selfIdentityReader {
	if pool == nil {
		return nil
	}
	return db.NewSelfIdentityStore(pool)
}

// domainConfigGaps is attribution.DomainConfigGaps for this process. A domain whose self-identity this process learns
// from the publication is NOT missing a self-actor — but only once an identity IS published: until 2026-11-08 the
// boot log reported both "journal" and "pve" as gaps on the triage plane (true for pve until TG-693, false for
// journal since TG-684), and clearing the gap merely because a database exists would silence it on the one
// deployment where it is real (an actuation plane whose credential never resolved, so it publishes nothing and
// every read holding TG's record stays indeterminate). An unreadable publication keeps the gap.
func domainConfigGaps(cfg attribution.Config, armed []string, rd selfIdentityReader) []attribution.DomainConfigGap {
	var out []attribution.DomainConfigGap
	for _, g := range attribution.DomainConfigGaps(cfg, armed, tgActuatesIn) {
		if g.NoSelfActor && rd != nil && publishedSelfDomains[g.Domain] {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, ok, err := rd.Latest(ctx, g.Domain)
			cancel()
			if err == nil && ok {
				g.NoSelfActor = false
			}
		}
		if g.NoSelfActor || g.NoSanctioned {
			out = append(out, g)
		}
	}
	return out
}

// executionLister is the read seam under sshLaneExecutionTimes (db.ActionExecutionStore.ExecutionsOn in production).
type executionLister func(ctx context.Context, host string, from, to time.Time) ([]db.ExecutionAt, error)

// sshLaneExecutionTimes keeps only the executions whose effect went over SSH (opschema.EffectSSHArgv). The owner's
// rule correlates a login with TG's recorded ACTION, not just a time on the host: a start-guest goes through the
// Proxmox API, an AWX launch through AWX, a gitops change through GitLab — none of them logs into the guest, so a
// login with TG's key beside one of them is not explained by it. An op-class the registry does not know (or a
// manifest that names none) explains nothing either: the login then reads unexplained, the safe direction.
func sshLaneExecutionTimes(list executionLister) journal.ExecutionTimes {
	return func(ctx context.Context, host string, from, to time.Time) ([]time.Time, error) {
		all, err := list(ctx, host, from, to)
		if err != nil {
			return nil, err
		}
		var out []time.Time
		for _, e := range all {
			if spec, ok := opschema.Lookup(e.OpClass); ok && spec.Kind() == opschema.EffectSSHArgv {
				out = append(out, e.At)
			}
		}
		return out, nil
	}
}

// explainingRecordTimes is everything that explains a login under TG's SSH key on a host: the SSH-lane executions and
// the actuation plane's recorded state reads (TG-709). Each record still explains at most one login (PairSelf), so a
// login beyond what TG recorded reads unexplained exactly as before. A failed read of either record fails the whole
// read, never a partial explanation.
func explainingRecordTimes(executions journal.ExecutionTimes, stateReads func(ctx context.Context, host string, from, to time.Time) ([]time.Time, error)) journal.ExecutionTimes {
	return func(ctx context.Context, host string, from, to time.Time) ([]time.Time, error) {
		out, err := executions(ctx, host, from, to)
		if err != nil {
			return nil, err
		}
		reads, err := stateReads(ctx, host, from, to)
		if err != nil {
			return nil, err
		}
		return append(out, reads...), nil
	}
}

// selfIdentityHolder returns the identity source the reader consults on every read. A process that holds the key
// answers its own derivation. Otherwise it answers the newest published identity, refreshed every `every`; a read
// error keeps the last good value (the identity changes only on a key rotation), and "" until one is read.
func selfIdentityHolder(ctx context.Context, domain, local string, rd selfIdentityReader, every time.Duration) func() string {
	if local != "" {
		return func() string { return local }
	}
	var held atomic.Value
	var warned atomic.Bool
	held.Store("")
	if rd == nil {
		log.Printf("self identity: no database — TG's own %s identity is UNKNOWN on this plane; %s reads holding TG's own "+
			"record are indeterminate (TG-684/TG-693)", domain, domain)
		return func() string { return held.Load().(string) }
	}
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		a, ok, err := rd.Latest(rctx, domain)
		switch {
		case err != nil:
			log.Printf("self identity: reading TG's published %s identity failed, keeping the previous value: %v (TG-684/TG-693)", domain, err)
		case !ok:
			if held.Load().(string) == "" && warned.CompareAndSwap(false, true) {
				log.Printf("self identity: TG's %s identity is NOT YET PUBLISHED by the actuation plane — %s reads holding TG's own "+
					"record stay indeterminate until it is (TG-684/TG-693)", domain, domain)
			}
		default:
			if held.Load().(string) != a {
				log.Printf("self identity: TG's %s identity read from the actuation plane's publication (TG-684/TG-693)", domain)
			}
			held.Store(a)
		}
	}
	refresh()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	return func() string { return held.Load().(string) }
}
