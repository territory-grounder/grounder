package confighash

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// volatileKeys are the PVE guest-config keys that change WITHOUT a human acting — the machine writes
// them into the config file in the course of its own scheduled or lifecycle operations. Including any
// of them would let an ORGANIC event move the hash, which is the exact INV-09 failure this package
// exists to refuse (a spurious "mutation observed" on a covered-but-empty attribution floods
// attributed-suspicious and pauses auto-heal). Grounded against live cluster configs read 2026-08-14,
// not imagined (the three-inert-controls lesson: grep the estate):
//
//   - digest: PVE's own SHA1 of the config FILE. Redundant with the hash this package computes, and
//     poisoned by the keys below — `lock` is written into the file mid-backup, so digest moves with it.
//   - lock: set and CLEARED BY THE MACHINE mid-operation. A scheduled vzdump writes `lock: backup`
//     with no human anywhere; on backup nights every guest would otherwise read "changed" twice.
//   - parent: the current snapshot pointer. LXC snapshot-mode vzdump creates and deletes a temporary
//     snapshot, moving `parent` organically.
//   - snapstate, snaptime: snapshot-machinery bookkeeping written during create/abort.
//   - runningmachine, runningcpu: runtime pins PVE writes at start/migrate/resume — coupled to the
//     guest LIFECYCLE, so an organic crash+restart must not move the hash (INV-09 exactly).
//   - vmstate, vmstatestorage: suspend-to-disk artifacts managed by the hibernate machinery.
//
// Everything else is INCLUDED — deliberately inclusive per TG-466: tags, description, onboot, disks,
// nets, memory, cicustom … are all deliberate acts, and for a key this list wrongly includes the cost
// is a one-shot over-signal on a deliberate estate event (safe direction), never a per-backup flood.
var volatileKeys = map[string]bool{
	"digest":         true,
	"lock":           true,
	"parent":         true,
	"snapstate":      true,
	"snaptime":       true,
	"runningmachine": true,
	"runningcpu":     true,
	"vmstate":        true,
	"vmstatestorage": true,
}

// hashScheme versions the canonicalization below. Baseline comparison is pure equality, so any change
// to the exclusion set or the frame format flips every guest once; the prefix makes that a visible,
// attributable scheme change instead of a silent wave of "changed".
const hashScheme = "ch1:"

// HashConfig is the pure signal core (TG-466 slice 1): a stable content hash over a guest's config
// keys/values with the volatile keys excluded. Deterministic by construction — keys are sorted, and
// each pair is length-prefix framed so no key/value concatenation can collide with another. The same
// config always hashes the same; two sweeps across an organic stop/start hash the same; only an edit
// to a non-volatile key moves it.
func HashConfig(cfg map[string]string) string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		if volatileKeys[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		v := cfg[k]
		fmt.Fprintf(h, "%d:%s=%d:%s;", len(k), k, len(v), v)
	}
	return hashScheme + hex.EncodeToString(h.Sum(nil))
}

// NormalizeGuestConfig flattens the decoded /config JSON object into the text map HashConfig consumes.
// PVE's ground truth is a TEXT config file; the API's JSON types are a per-version projection of it
// (measured live: QEMU `memory` arrives as the string "16384" while LXC `memory` arrives as the number
// 4096). Normalizing toward the text form makes the hash immune to that projection flapping across PVE
// upgrades: a JSON string contributes its decoded text, everything else (numbers, bools, the LXC `lxc`
// array of raw config lines) contributes its compact wire text — so "16384" and 16384 hash identically.
func NormalizeGuestConfig(raw map[string]json.RawMessage) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = normalizeValue(v)
	}
	return out
}

func normalizeValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		// Not valid JSON at all — hash the trimmed raw bytes rather than dropping the key: dropping
		// would make a key the parser dislikes invisible to the signal (an exclusion nobody chose).
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}

// Observed is one guest's config-hash observation as the sweep presents it to the baseline store:
// the identity attributes ride along so the row stays resolvable by the attribution subject (guest
// name) in slice 2, but the baseline is KEYED per vmid — the stable PVE identity.
type Observed struct {
	VMID  int64
	Guest string
	Node  string
	Kind  string // "qemu" | "lxc"
	Hash  string
}

// Outcome is the store's answer to one Record: exactly one of the three shapes —
// first sighting (no baseline existed; one was recorded), unchanged, or changed with the prior hash.
type Outcome struct {
	FirstSighting bool
	Changed       bool
	PreviousHash  string
}

// Signal maps an Outcome to the observed-mutation signal with the TG-466 fail-safe semantics in ONE
// place: a FIRST SIGHTING is a baseline, never a change — there was nothing to diff against, and
// minting "changed" from an absent baseline would fire on every new guest and every fresh deployment.
func (o Outcome) Signal() (changed bool, previousHash string) {
	if o.FirstSighting {
		return false, ""
	}
	return o.Changed, o.PreviousHash
}

// Baselines is the persistence seam for the per-vmid hash baseline. The estate-derived projection in
// core/db (guest_config_baseline, migration 0091) mirrors this shape field-for-field — slice 2 binds
// the two with a composition-root adapter, the guest-liveness feed pattern — and tests inject an
// in-memory one. Record must be atomic per vmid: compare the stored hash, persist the new one, and
// answer which of the three Outcome shapes occurred.
type Baselines interface {
	Record(ctx context.Context, obs Observed) (Outcome, error)
}

// Diff is the TG-466 slice-1 signal function: record the guest's current config hash against its
// baseline and answer whether the CONFIG changed — the grounded positive observed-mutation signal
// slice 2 threads into AttributeInput.Observation (never wired here; this package feeds nothing yet).
//
// FAIL-CLOSED BY CONSTRUCTION, in the INV-09 direction: a store error returns changed=false — a
// broken read must NEVER fabricate a mutation signal, because downstream that signal escalates a
// covered-but-empty attribution to attributed-suspicious and pauses auto-heal. The error is dropped
// here deliberately (the signal is the contract); the Collector tallies failures loudly so a
// permanently-broken store is visible as tg_pve_confighash_errored_total, never as silence.
func Diff(ctx context.Context, store Baselines, obs Observed) (changed bool, previousHash string) {
	out, err := store.Record(ctx, obs)
	if err != nil {
		return false, ""
	}
	return out.Signal()
}
