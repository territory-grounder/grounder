package awx

import (
	"sort"
	"strings"
	"sync"
)

// CredentialBinding is one row of the credential-onboarding FIRST SCREEN (TG-274): an AWX Machine
// credential, the inventory it governs, how many hosts that covers, and whether TG can actually use it.
//
// ★ WHY THIS TYPE EXISTS. The connector already resolves all of this during Sync — the job-template walk
// knows the credential's name, its inventory and whether the operator mapped it — and then throws it away.
// Skipped() recorded the failures and `grep -rn '\.Skipped()'` over non-test code returned NOTHING: the
// coverage record was computed on every sync and reached no operator surface. Measured on this estate
// 2026-08-04: AWX holds 11 Machine credentials and TG_AWX_CRED_REF_MAP maps exactly ONE. The other ten are
// invisible, so a deployment that is blind to most of the fleet looks configured.
//
// The consequence is not only visibility. Because there was no surface on which to say "this credential
// needs its key", the key had to be a FILE the operator placed by hand — which is how a private key came
// to live in a bind mount readable by the worker's uid (TG-153).
type CredentialBinding struct {
	// CredentialName is the AWX Machine credential's name — the exact string TG_AWX_CRED_REF_MAP keys on,
	// so an operator can copy it rather than guess at it.
	CredentialName string
	// Inventory is the AWX inventory the binding applies to, via a job template.
	Inventory string
	// JobTemplate is the template that binds them. Named because that is where an operator changes it.
	JobTemplate string
	// Hosts is how many AWX hosts this inventory covers — the blast radius of supplying this key, and the
	// cost of NOT supplying it.
	Hosts int
	// Mapped reports whether TG holds a SecretRef for this credential name. False means TG discovered the
	// binding, understood it, and can do nothing with it.
	Mapped bool
	// Ref is the mapped SecretRef, or "" when unmapped. A REFERENCE only, never key material (INV-13).
	Ref string
}

// Usable reports whether this binding can actually produce a login identity today.
func (b CredentialBinding) Usable() bool { return b.Mapped && strings.TrimSpace(b.Ref) != "" }

// discovery accumulates bindings during a Sync. Separate from skips because the two answer different
// questions: a skip says why one host was dropped; a binding says what the operator could turn on.
type discovery struct {
	mu   sync.Mutex
	rows map[string]CredentialBinding
}

func newDiscovery() *discovery { return &discovery{rows: map[string]CredentialBinding{}} }

func (d *discovery) reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows = map[string]CredentialBinding{}
}

// note records one (credential, inventory) binding. Keyed on the pair so a credential governing several
// inventories yields a row each — an operator supplying one key needs to see everything it unlocks.
func (d *discovery) note(b CredentialBinding) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows[b.CredentialName+"\x00"+b.Inventory] = b
}

func (d *discovery) all() []CredentialBinding {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]CredentialBinding, 0, len(d.rows))
	for _, b := range d.rows {
		out = append(out, b)
	}
	// UNMAPPED FIRST, then widest blast radius. The rows an operator must act on lead; a long tail of
	// working credentials must never push the actionable ones off the screen.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Mapped != out[j].Mapped {
			return !out[i].Mapped
		}
		if out[i].Hosts != out[j].Hosts {
			return out[i].Hosts > out[j].Hosts
		}
		if out[i].CredentialName != out[j].CredentialName {
			return out[i].CredentialName < out[j].CredentialName
		}
		return out[i].Inventory < out[j].Inventory
	})
	return out
}

// Discovered returns what the most recent Sync learned about AWX's credential→inventory bindings, mapped
// and unmapped alike.
//
// It reports the UNMAPPED ones deliberately. A source that listed only what it could already use would
// answer "everything I can see works" while being blind to most of the fleet — the precise shape of a
// surface that looks configured and is not.
func (s *Source) Discovered() []CredentialBinding {
	if s == nil {
		return nil
	}
	return s.disc.all()
}
