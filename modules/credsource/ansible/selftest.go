package ansible

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof the module can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the module would silently degrade to "no
// test is implemented" — honest, but a dialog that promises a parse and a decrypt and performs neither.
//
// THE PROBE LIVES ON THE RESOLVER, NOT ON THE SOURCE, and that is the whole design decision in this file.
// The Source holds the Tree and can count hosts; only the Resolver holds the vault-password reference. A
// probe on the Source would read the filesystem, report a cheerful host count, and pass with a WRONG VAULT
// PASSWORD — which is this module's one secret, the one field the console writes, and the only thing whose
// failure is invisible until a governed action tries to sudo. The composition root must therefore offer the
// resolver (modules/bootstrap.buildAnsibleSource builds both from one Tree); see the module report.
var _ selftest.Tester = (*Resolver)(nil)

// probeHostScan bounds how many hosts are inspected while looking for a vaulted value.
//
// The search is over the FILESYSTEM, and each host costs a group_vars/host_vars read; an estate tree with
// two thousand hosts would otherwise turn a settings dialog into a directory walk inside a 30-second bound.
// One vaulted value is all the probe needs — the password either decrypts it or does not — so the scan stops
// at the first one and never gets near this cap on a tree that has any.
const probeHostScan = 64

// SelfTest reads the tree the module actually syncs and then DECRYPTS one inline ansible-vault value with
// the password resolved the real way.
//
// WHY THE DECRYPT IS THE POINT. Everything before it — the root exists, the inventory parses, N hosts are
// declared — is a filesystem check that passes with a wrong vault password, an unreadable password file, and
// a rotated secret that never reached the backend. Those are exactly the faults an operator presses TEST to
// rule out, and this module's password is EffectLive (Resolver.decrypt resolves the reference on every
// call), so a save in this dialog is meant to work immediately: this button is the only place that claim is
// checked before a governed action depends on it. The decrypt runs through ResolveRef — the real use-time
// path, re-reading the tree and resolving the password reference exactly as an ansible-vault: Bundle
// reference does — rather than through a shortcut that would prove a different code path.
//
// WHY A WRONG PASSWORD CANNOT PASS. Ansible's VaultAES256 payload is authenticated: Decrypt verifies an
// HMAC over the ciphertext before returning anything, so a wrong password fails the MAC rather than yielding
// garbage. There is no way for this probe to report a decrypt it did not achieve.
//
// THE PLAINTEXT IS NEVER RETURNED, LOGGED, OR MEASURED. It is discarded at the assignment; the Summary names
// the host and the VAR (both already public in bundles and logs), never the value, and not even its length —
// Result is rendered in a dialog and pasted into tickets.
//
// WHAT A GREEN RESULT PROVES: the root is a readable directory, the inventory parses, and the saved vault
// password decrypts a real value in this tree. WHAT IT DOES NOT PROVE: that every vaulted value uses the
// same password (a 1.2 payload may carry a vault-id whose own password is configured separately), nor that
// the hosts carry usable connection identities — a host with no key reference is skipped at sync time and no
// read here can predict that.
//
// operator is ignored: this probe has no outward side effect, so there is no event in anyone's console that
// would need a named author.
func (r *Resolver) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if r == nil || r.tree == nil {
		return selftest.Result{
				Summary: "no Ansible tree is wired",
				Detail:  "the module resolved to nothing — nothing was read. This is a TG wiring fault.",
			},
			fmt.Errorf("ansible: selftest: nil tree")
	}
	root := r.tree.Root()

	// The root is re-checked rather than trusted from construction: NewTree validated it at BOOT, and the
	// failure this catches is a bind mount or NFS export that has gone away since — which presents as a sync
	// that quietly stops learning hosts.
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return selftest.Result{
			Summary: "the Ansible tree root " + root + " is not a readable directory",
			Detail: "the configured root no longer stats as a directory. On this deploy that usually means a " +
				"bind mount or a volume that is no longer attached to the worker, rather than a deleted tree " +
				"— check the mount before editing the path.",
		}, fmt.Errorf("ansible: selftest: root %q is not a readable directory", root)
	}

	inv, err := r.tree.parseInventory()
	if err != nil {
		return selftest.Result{
			Summary: "could not parse the inventory under " + root,
			Detail: "the inventory file could not be read or parsed, so Sync fails closed and no host identity " +
				"is learned from this tree. Raw: " + err.Error(),
		}, fmt.Errorf("ansible: selftest: %w", err)
	}

	host, field, scanned, err := r.findVaulted(ctx, inv)
	if err != nil {
		return selftest.Result{
			Summary: fmt.Sprintf("read the inventory under %s (%s), but a vars file could not be read",
				root, plural(len(inv.hosts), "host")),
			Detail: "a group_vars/host_vars file failed to load. Sync fails closed on exactly this, so no " +
				"host identity is learned at all. Raw: " + err.Error(),
		}, fmt.Errorf("ansible: selftest: %w", err)
	}

	base := fmt.Sprintf("read the Ansible tree at %s: %s declared in %s",
		root, plural(len(inv.hosts), "host"), r.tree.inventory)

	if host == "" {
		// Nothing to decrypt. This is a PASS — the tree is readable and the sync will work — but the module's
		// one secret was NOT exercised, and a Summary that stopped at the host count would imply it was.
		return selftest.Result{
			Summary: base + "; no ansible-vault value was found to decrypt",
			Detail: fmt.Sprintf("the vault password was NOT exercised: no inline !vault value exists under the "+
				"first %s of this tree, so nothing could be decrypted. This test therefore proves the tree is "+
				"readable, NOT that the saved vault password is correct. If this tree is expected to carry "+
				"vaulted become passwords, check that the group_vars/host_vars files really hold them.",
				plural(scanned, "host")),
		}, nil
	}

	ref := Scheme + ":" + host + "#" + field
	// The plaintext is discarded immediately and deliberately: it is a real credential, and Result is
	// rendered in a dialog and pasted into tickets.
	if _, err := r.ResolveRef(ref); err != nil {
		return selftest.Result{
			Summary: base + fmt.Sprintf("; the vault value at %s#%s did NOT decrypt", host, field),
			Detail:  classifySelfTestFailure(err),
		}, fmt.Errorf("ansible: selftest: decrypt %s#%s: %w", host, field, err)
	}

	return selftest.Result{
		Summary: base + fmt.Sprintf("; decrypted %s#%s with the saved vault password", host, field),
	}, nil
}

// findVaulted walks the inventory in its own deterministic order and returns the FIRST (host, var) pair
// carrying an inline !vault payload, plus how many hosts were inspected.
//
// Deterministic on purpose: the same tree must yield the same probe target on every press, so two operators
// comparing results are comparing the same thing, and a failure names a value they can go and look at. It
// respects ctx (a tree on a stalled NFS mount is the reason the console bounds this at all) and stops at
// probeHostScan.
func (r *Resolver) findVaulted(ctx context.Context, inv inventoryData) (host, field string, scanned int, err error) {
	for _, h := range inv.hosts {
		if err := ctx.Err(); err != nil {
			return "", "", scanned, err
		}
		if scanned >= probeHostScan {
			break
		}
		scanned++
		vars, err := r.tree.hostVars(h, inv)
		if err != nil {
			return "", "", scanned, err
		}
		names := make([]string, 0, len(vars))
		for name, sc := range vars {
			if sc.vaulted {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names) // map iteration is random; the probe target must not be
		return h, names[0], scanned, nil
	}
	return "", "", scanned, nil
}

// classifySelfTestFailure turns a failed decrypt into something an operator can act on. "error" tells them
// nothing; "the password does not match this value's MAC" tells them exactly which field to correct.
//
// It classifies on the SHAPE of the failure — the connector's own sentinel errors and the reference-
// resolution failures ahead of them — rather than on any text the vault payload carries. Anything it cannot
// place falls through to the raw error rather than to an invented diagnosis.
func classifySelfTestFailure(err error) string {
	switch {
	case errors.Is(err, ErrMACMismatch):
		return "the saved vault password DOES NOT DECRYPT this value: the payload's message authentication " +
			"failed, which means a wrong password or a tampered file — never a partially-correct decrypt. " +
			"Save the correct password here (it is re-read on every decrypt, so it takes effect immediately). " +
			"If this tree uses ansible-vault IDs, note that a 1.2 payload labelled with an id needs that id's " +
			"own password configured, not just the default one."
	case errors.Is(err, ErrNotVault):
		return "the value looked like a vaulted scalar but is not a valid $ANSIBLE_VAULT payload — the file " +
			"has most likely been edited by hand or truncated."
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve"), strings.Contains(s, "secret ref"), strings.Contains(s, "no such file"):
		return "the vault password could not be READ from its reference — the reference is wrong, the file is " +
			"missing, or the secret backend is unreachable. NOTHING was decrypted, and every ansible-vault: " +
			"reference in the estate fails closed while this is true."
	case strings.Contains(s, "unsupported cipher"), strings.Contains(s, "malformed"):
		return "the vault payload is not a VaultAES256 envelope this connector can read. TG implements the " +
			"standard ansible-vault format natively (no ansible binary is invoked); a payload written by a " +
			"different tool or cipher cannot be decrypted here. Raw: " + err.Error()
	default:
		return err.Error()
	}
}

// plural renders a count with its noun so the Summary reads as a sentence rather than as a log line: an
// operator reading "1 hosts" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
