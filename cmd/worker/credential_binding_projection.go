package main

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	"github.com/territory-grounder/grounder/modules/credsource/awx"
)

// credentialBindingPublisher is the write half of the credential-onboarding projection (TG-274).
type credentialBindingPublisher interface {
	Publish(ctx context.Context, sourceID string, rows []db.CredentialBindingRow, now time.Time) error
}

// discoverer is what a credential source must expose to appear on the onboarding screen. Kept as a narrow
// interface rather than a concrete *awx.Source so a second inventory source (a future NetBox or Semaphore
// connector) joins the screen by implementing one method, not by editing this file.
type discoverer interface {
	Discovered() []awx.CredentialBinding
}

// credentialBindingRows renders a source's discovery as projection rows. Split out so the oracle can assert
// the mapping without a database.
func credentialBindingRows(bs []awx.CredentialBinding) []db.CredentialBindingRow {
	out := make([]db.CredentialBindingRow, 0, len(bs))
	for _, b := range bs {
		out = append(out, db.CredentialBindingRow{
			Credential: b.CredentialName,
			Scope:      b.Inventory,
			Via:        b.JobTemplate,
			Hosts:      b.Hosts,
			Mapped:     b.Mapped,
			SecretRef:  b.Ref, // a REFERENCE; the material never enters this path (INV-13)
		})
	}
	return out
}

// runCredentialBindingProjection publishes what each inventory source discovered, on a timer.
//
// It publishes UNMAPPED bindings too, and that is the whole point: a screen listing only credentials TG can
// already use would answer "everything I can see works" while blind to the rest of the fleet. On this
// estate AWX holds 11 Machine credentials and TG maps one — the other ten were invisible because the
// connector computed exactly this and discarded it.
func runCredentialBindingProjection(ctx context.Context, sources map[string]discoverer, store credentialBindingPublisher, every time.Duration, logf func(string, ...any)) {
	if store == nil || len(sources) == 0 {
		return
	}
	if every <= 0 {
		every = time.Minute
	}
	publish := func() {
		for id, src := range sources {
			rows := credentialBindingRows(src.Discovered())
			if err := store.Publish(ctx, id, rows, time.Now().UTC()); err != nil {
				logf("credential binding projection: publish %s failed (console shows a stale or empty onboarding screen): %v", id, err)
				continue
			}
			unmapped := 0
			for _, r := range rows {
				if !r.Mapped {
					unmapped++
				}
			}
			if unmapped > 0 {
				// Said at INFO, not debug: an operator whose fleet is mostly unreachable should learn it
				// from the boot log too, not only by opening a page they may not know exists.
				logf("credential onboarding: source %q reports %d credential binding(s), %d WITHOUT a TG SecretRef — those hosts have no usable login identity", id, len(rows), unmapped)
			}
		}
	}
	publish()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			publish()
		}
	}
}

// discoverableSources picks the credential sources that can report their credential→scope bindings.
//
// A type switch rather than a registry: RegisteredCredentialSource.Instance is deliberately `any` (the same
// choice modules.Registration.Adapter makes), so this is where the concrete capability is recovered. A
// source that cannot discover is simply absent from the screen — never rendered as "no bindings", which
// would be a different and false claim.
func discoverableSources(regs []bootstrap.RegisteredCredentialSource) map[string]discoverer {
	out := map[string]discoverer{}
	for _, rs := range regs {
		if d, ok := rs.Instance.(discoverer); ok && rs.ID != "" {
			out[rs.ID] = d
		}
	}
	return out
}
