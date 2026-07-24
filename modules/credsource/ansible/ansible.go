// Package ansible is the native-Go UN-CONTROLLED Ansible credential connector (spec/016 T-016-8, REQ-1612,
// TG-88) — the FILE/vault case, distinct from the AWX-controller case (modules/credsource/awx). Many estates
// run "un-controlled" Ansible: plain inventory + group_vars/host_vars files on disk with NO AWX/Semaphore/
// Tower controller and NO REST API. This connector reads that layout READ-ONLY and provides two capabilities
// on the machine → host plane:
//
//  1. A credential.CredentialSource (+ credential.MembershipSource): Sync parses the inventory and the
//     group_vars/host_vars tree into one machine-plane SourceEntry per host, whose Bundle carries SecretRef
//     REFERENCES only — never a raw secret. The per-host sudo/become secret is carried as an ansible-vault:
//     SecretRef (below); the ssh private key as a file: reference derived from ansible_ssh_private_key_file.
//     Inventory group membership (including [g:children] nesting) is exposed as MembershipSource so a
//     group-selector bundle resolves for a host in that group (REQ-1605).
//  2. The ansible-vault: SecretRef scheme (RegisterResolver): a Bundle SecretRef like
//     "ansible-vault:librespeed01#ansible_become_pass" is dereferenced at USE TIME by re-reading the host's
//     vars from the tree (INV-05, re-read by id) and NATIVELY decrypting the inline `!vault`-tagged
//     $ANSIBLE_VAULT;1.1;AES256 payload in pure Go. TG holds only the vault PASSWORD (itself a SecretRef);
//     the plaintext exists only in memory at use time and is never persisted or logged (INV-13). It FAILS
//     CLOSED — a missing host/field, an unreadable tree, a wrong password, or a failed HMAC → an error, never
//     a blank or default secret.
//
// WHY the become secret is an ansible-vault: ref and NOT copied into the Bundle: even if the operator left a
// var in PLAINTEXT, the Source stores the LOCATOR (ansible-vault:<host>#<var>), never the value, so no secret
// material ever enters the converged store, the drift record, or the ledger (INV-13). Only the ssh key PATH
// (a non-secret file: reference) and the connection metadata (user/port/scheme) are read at sync time.
//
// DISTROLESS-SAFE / NO SUBPROCESS (INV-02): the vault payload is decrypted with the Go standard library only
// (crypto/pbkdf2 · crypto/aes · crypto/cipher · crypto/hmac · crypto/sha256 · crypto/subtle · encoding/hex).
// It does NOT shell out to the `ansible-vault` CLI (the worker is distroless and has no shell), and it does
// NOT vendor a third-party ansible-vault library — the format is small and stable, so the ~50-line decrypt is
// in-tree and audited here.
//
// GROUNDED IN THE ANSIBLE VAULT FORMAT (VaultAES256), lib/ansible/parsing/vault/__init__.py:
//   - Envelope: a first line `$ANSIBLE_VAULT;<version>;AES256[;<vault-id>]` (version 1.1 or 1.2), then a hex
//     body. Unhexlify(body) yields UTF-8 text of three '\n'-separated hex fields: hexlify(salt), the HMAC
//     (hex), hexlify(ciphertext). Each field is unhexlified again to bytes.
//   - Key derivation (_gen_key_initctr): PBKDF2-HMAC-SHA256(password, salt, iterations=10000, dkLen=80). The
//     80 derived bytes split as key1=[:32] (AES-256 key), key2=[32:64] (HMAC-SHA256 key), iv=[64:80] (16-byte
//     CTR nonce/counter).
//   - Integrity: expected = HMAC-SHA256(key2, ciphertext); verified with a constant-time compare (subtle);
//     a mismatch (wrong password or tampering) fails closed.
//   - Confidentiality: AES-256 in CTR mode over the ciphertext, then PKCS#7 unpadding (block size 16) — the
//     encrypter PKCS#7-pads the plaintext before the (stream) CTR step, so the decrypter unpads.
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1612), TG-88.
package ansible

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "ansible"

// Scheme is the SecretRef scheme this connector's resolver dereferences (natively decrypting ansible-vault).
const Scheme = "ansible-vault"

// becomeVar is the ansible var that carries the per-host privilege-escalation (sudo) password — the canonical
// per-host secret an un-controlled estate ansible-vault-encrypts. It maps to the Bundle's become slot.
const becomeVar = "ansible_become_pass"

// ---------------------------------------------------------------------------------------------------------
// NATIVE ANSIBLE-VAULT DECRYPT (VaultAES256) — pure Go stdlib, no subprocess, no third-party vault lib.
// ---------------------------------------------------------------------------------------------------------

const (
	vaultHeaderTag  = "$ANSIBLE_VAULT"               // the envelope's first-line marker
	vaultCipher     = "AES256"                       // the only cipher this connector supports (the ansible default)
	pbkdf2Iters     = 10000                          // ansible VaultAES256 iteration count (fixed)
	aesKeyLen       = 32                             // AES-256
	hmacKeyLen      = 32                             // HMAC-SHA256 key
	ivLen           = 16                             // AES block size / CTR nonce length
	derivedKeyLen   = aesKeyLen + hmacKeyLen + ivLen // 80: [key1||key2||iv]
	pkcs7BlockBytes = 16                             // AES block size for PKCS#7 unpadding
)

// ErrNotVault is returned when a payload is not an $ANSIBLE_VAULT envelope. ErrMACMismatch is the fail-closed
// integrity failure — a wrong password or a tampered payload. Both wrap so callers fail closed uniformly.
var (
	ErrNotVault    = errors.New("ansible: not an $ANSIBLE_VAULT payload")
	ErrMACMismatch = errors.New("ansible: vault HMAC verification failed (wrong password or tampered payload)")
)

// IsEncrypted reports whether b begins with the $ANSIBLE_VAULT envelope marker (leading whitespace allowed).
func IsEncrypted(b []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(b)), vaultHeaderTag)
}

// vaultEnvelope is the parsed header + the three decoded fields of a VaultAES256 payload.
type vaultEnvelope struct {
	version string // "1.1" / "1.2"
	vaultID string // optional label (1.2); "" for 1.1
	salt    []byte
	hmac    []byte
	cipher  []byte
}

// parseVaultEnvelope parses an $ANSIBLE_VAULT;<ver>;AES256[;<id>] payload into its decoded fields. It is
// strict: a non-vault input, an unsupported cipher, or a malformed body is an error (fail closed).
func parseVaultEnvelope(payload []byte) (vaultEnvelope, error) {
	text := strings.TrimSpace(string(payload))
	if !strings.HasPrefix(text, vaultHeaderTag) {
		return vaultEnvelope{}, ErrNotVault
	}
	lines := strings.Split(text, "\n")
	// The first non-empty line is the header; the remainder is the hex body.
	header := strings.TrimSpace(lines[0])
	hf := strings.Split(header, ";")
	if len(hf) < 3 || strings.TrimSpace(hf[0]) != vaultHeaderTag {
		return vaultEnvelope{}, fmt.Errorf("ansible: malformed vault header %q", header)
	}
	env := vaultEnvelope{version: strings.TrimSpace(hf[1])}
	if c := strings.TrimSpace(hf[2]); !strings.EqualFold(c, vaultCipher) {
		return vaultEnvelope{}, fmt.Errorf("ansible: unsupported vault cipher %q (only %s)", c, vaultCipher)
	}
	if len(hf) >= 4 {
		env.vaultID = strings.TrimSpace(hf[3])
	}
	// Body: join all remaining lines, strip ALL whitespace, hex-decode → the inner '\n'-joined hex triple.
	var body strings.Builder
	for _, ln := range lines[1:] {
		for _, r := range ln {
			if r != ' ' && r != '\t' && r != '\r' && r != '\n' {
				body.WriteRune(r)
			}
		}
	}
	inner, err := hex.DecodeString(body.String())
	if err != nil {
		return vaultEnvelope{}, fmt.Errorf("ansible: vault body is not valid hex: %w", err)
	}
	parts := strings.Split(string(inner), "\n")
	if len(parts) != 3 {
		return vaultEnvelope{}, fmt.Errorf("ansible: vault body has %d fields, want 3 (salt/hmac/ciphertext)", len(parts))
	}
	if env.salt, err = hex.DecodeString(strings.TrimSpace(parts[0])); err != nil {
		return vaultEnvelope{}, fmt.Errorf("ansible: vault salt not hex: %w", err)
	}
	if env.hmac, err = hex.DecodeString(strings.TrimSpace(parts[1])); err != nil {
		return vaultEnvelope{}, fmt.Errorf("ansible: vault hmac not hex: %w", err)
	}
	if env.cipher, err = hex.DecodeString(strings.TrimSpace(parts[2])); err != nil {
		return vaultEnvelope{}, fmt.Errorf("ansible: vault ciphertext not hex: %w", err)
	}
	if len(env.salt) == 0 || len(env.hmac) == 0 || len(env.cipher) == 0 {
		return vaultEnvelope{}, errors.New("ansible: vault payload has an empty salt/hmac/ciphertext field")
	}
	return env, nil
}

// Decrypt natively decrypts an ansible-vault VaultAES256 payload with the given password (INV-02, no CLI). It
// fails closed on a non-vault input, an unsupported cipher, a malformed body, a wrong password / failed HMAC
// (ErrMACMismatch), or a bad PKCS#7 padding. The returned plaintext exists only in the caller's memory.
func Decrypt(payload, password []byte) ([]byte, error) {
	env, err := parseVaultEnvelope(payload)
	if err != nil {
		return nil, err
	}
	// PBKDF2-HMAC-SHA256(password, salt, 10000, 80) → key1 || key2 || iv.
	dk, err := pbkdf2.Key(sha256.New, string(password), env.salt, pbkdf2Iters, derivedKeyLen)
	if err != nil {
		return nil, fmt.Errorf("ansible: pbkdf2: %w", err)
	}
	key1, key2, iv := dk[:aesKeyLen], dk[aesKeyLen:aesKeyLen+hmacKeyLen], dk[aesKeyLen+hmacKeyLen:]

	// Integrity FIRST (encrypt-then-MAC): verify HMAC-SHA256(key2, ciphertext) in constant time before touching
	// the ciphertext, so a wrong password / tamper fails closed and never yields garbage plaintext.
	mac := hmac.New(sha256.New, key2)
	mac.Write(env.cipher)
	if subtle.ConstantTimeCompare(mac.Sum(nil), env.hmac) != 1 {
		return nil, ErrMACMismatch
	}

	// AES-256-CTR decrypt, then PKCS#7 unpad (the encrypter pads to the 16-byte block before CTR).
	block, err := aes.NewCipher(key1)
	if err != nil {
		return nil, fmt.Errorf("ansible: aes: %w", err)
	}
	if len(env.cipher)%pkcs7BlockBytes != 0 {
		return nil, fmt.Errorf("ansible: vault ciphertext length %d is not a multiple of the block size", len(env.cipher))
	}
	padded := make([]byte, len(env.cipher))
	cipher.NewCTR(block, iv).XORKeyStream(padded, env.cipher)
	plain, err := pkcs7Unpad(padded)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

// pkcs7Unpad removes PKCS#7 padding validated in constant style (block size 16). A pad byte outside [1,16] or
// an inconsistent pad tail is a hard error (fail closed) — never silently trimmed.
func pkcs7Unpad(b []byte) ([]byte, error) {
	n := len(b)
	if n == 0 || n%pkcs7BlockBytes != 0 {
		return nil, fmt.Errorf("ansible: pkcs7: invalid input length %d", n)
	}
	pad := int(b[n-1])
	if pad == 0 || pad > pkcs7BlockBytes {
		return nil, fmt.Errorf("ansible: pkcs7: invalid padding byte %d", pad)
	}
	ok := 1
	for i := 0; i < pad; i++ {
		if b[n-1-i] != byte(pad) {
			ok = 0
		}
	}
	if ok != 1 {
		return nil, errors.New("ansible: pkcs7: inconsistent padding")
	}
	return b[:n-pad], nil
}

// ---------------------------------------------------------------------------------------------------------
// ANSIBLE TREE (inventory + group_vars/host_vars) — read-only parse of the un-controlled layout.
// ---------------------------------------------------------------------------------------------------------

// scalar is one resolved var value: its text plus whether the text is an inline `!vault` payload (vaulted) or
// a plaintext scalar. A vaulted value's text is the $ANSIBLE_VAULT envelope; only the ansible-vault: resolver
// (which holds the password) ever decrypts it — the tree never does (INV-13).
type scalar struct {
	text    string
	vaulted bool
}

// Tree is a read-only view of an un-controlled Ansible directory (inventory + group_vars/host_vars). It is
// stateless: every accessor RE-READS the files (INV-05, re-read by id — no cached mutable copy), so the Source
// and the resolver always see the current on-disk state. Construct with NewTree.
type Tree struct {
	root      string
	inventory string // resolved inventory file path
}

// maxVarsBytes bounds a single vars file (real connection vars are tiny; a larger file is a misconfig/attack
// and is refused before decode — the same defense the AWX connector applies to a host's .variables blob).
const maxVarsBytes = 1 << 20 // 1 MiB

// inventoryNames are the conventional un-controlled inventory filenames searched at the tree root when the
// operator does not pin one explicitly.
var inventoryNames = []string{"inventory.ini", "inventory", "hosts.ini", "hosts"}

// NewTree builds a Tree over root. InventoryPath (optional) pins the inventory file; otherwise the
// conventional names are searched at the root. It fails closed if the root or a resolvable inventory is
// absent — an unreadable estate is not a source of blank identities.
func NewTree(root, inventoryPath string) (*Tree, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("ansible: tree requires a root directory")
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("ansible: tree root %q is not a readable directory", root)
	}
	inv := strings.TrimSpace(inventoryPath)
	if inv == "" {
		for _, name := range inventoryNames {
			cand := filepath.Join(root, name)
			if st, e := os.Stat(cand); e == nil && !st.IsDir() {
				inv = cand
				break
			}
		}
	} else if !filepath.IsAbs(inv) {
		inv = filepath.Join(root, inv)
	}
	if inv == "" {
		return nil, fmt.Errorf("ansible: no inventory file found under %q (looked for %v)", root, inventoryNames)
	}
	if st, e := os.Stat(inv); e != nil || st.IsDir() {
		return nil, fmt.Errorf("ansible: inventory %q is not a readable file", inv)
	}
	return &Tree{root: root, inventory: inv}, nil
}

// Root returns the tree's root directory (non-secret; used for provenance in the Source id log).
func (t *Tree) Root() string { return t.root }

// inventoryData is the parsed inventory: host membership + child-group nesting + inline vars.
type inventoryData struct {
	hosts       []string                     // every declared host, sorted, de-duplicated
	directGroup map[string]map[string]bool   // host → set of directly-listed groups
	children    map[string][]string          // group → its child groups ([g:children])
	groupVars   map[string]map[string]scalar // group → vars from [g:vars] (plaintext scalars)
	hostVars    map[string]map[string]scalar // host → inline vars from the host line
}

// parseInventory parses the INI-subset un-controlled inventory: `[group]` host lists (each host line may carry
// inline `k=v` vars), `[group:vars]` group variables, and `[group:children]` group nesting. Lines before any
// section, and any `[group]` member, also belong to the implicit "all" group (added at membership time).
func (t *Tree) parseInventory() (inventoryData, error) {
	raw, err := os.ReadFile(t.inventory)
	if err != nil {
		return inventoryData{}, fmt.Errorf("ansible: read inventory: %w", err)
	}
	d := inventoryData{
		directGroup: map[string]map[string]bool{},
		children:    map[string][]string{},
		groupVars:   map[string]map[string]scalar{},
		hostVars:    map[string]map[string]scalar{},
	}
	hostSet := map[string]bool{}
	section, kind := "ungrouped", "hosts"
	for _, ln := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(ln)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			switch {
			case strings.HasSuffix(name, ":vars"):
				section, kind = strings.TrimSuffix(name, ":vars"), "vars"
			case strings.HasSuffix(name, ":children"):
				section, kind = strings.TrimSuffix(name, ":children"), "children"
			default:
				section, kind = name, "hosts"
			}
			continue
		}
		switch kind {
		case "hosts":
			fields := strings.Fields(line)
			host := fields[0]
			hostSet[host] = true
			if d.directGroup[host] == nil {
				d.directGroup[host] = map[string]bool{}
			}
			d.directGroup[host][section] = true
			for _, kv := range fields[1:] {
				if k, v, ok := strings.Cut(kv, "="); ok {
					if d.hostVars[host] == nil {
						d.hostVars[host] = map[string]scalar{}
					}
					d.hostVars[host][strings.TrimSpace(k)] = scalar{text: strings.TrimSpace(v)}
				}
			}
		case "vars":
			if k, v, ok := strings.Cut(line, "="); ok {
				if d.groupVars[section] == nil {
					d.groupVars[section] = map[string]scalar{}
				}
				d.groupVars[section][strings.TrimSpace(k)] = scalar{text: strings.TrimSpace(v)}
			}
		case "children":
			d.children[section] = append(d.children[section], strings.Fields(line)[0])
		}
	}
	for h := range hostSet {
		d.hosts = append(d.hosts, h)
	}
	sort.Strings(d.hosts)
	return d, nil
}

// groupsForHost returns every group a host belongs to: its directly-listed groups, every ancestor group via
// [g:children] nesting (transitive), and the implicit "all". Non-secret; sorted + de-duplicated.
func (d inventoryData) groupsForHost(host string) []string {
	// Invert children into child → parents for ancestor walking.
	parents := map[string][]string{}
	for parent, kids := range d.children {
		for _, k := range kids {
			parents[k] = append(parents[k], parent)
		}
	}
	seen := map[string]bool{"all": true}
	var stack []string
	for g := range d.directGroup[host] {
		stack = append(stack, g)
	}
	for len(stack) > 0 {
		g := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		stack = append(stack, parents[g]...)
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// hostVars merges the vars that apply to a host, low→high precedence (higher overrides lower):
//
//	group_vars/all  <  group_vars/<group> (+ [group:vars])  <  host_vars/<host>  <  inventory inline host vars
//
// Vaulted values are carried through UNDECRYPTED (INV-13). It re-reads the files on every call (INV-05).
func (t *Tree) hostVars(host string, d inventoryData) (map[string]scalar, error) {
	merged := map[string]scalar{}
	apply := func(m map[string]scalar) {
		for k, v := range m {
			merged[k] = v
		}
	}
	groups := d.groupsForHost(host)
	// group_vars/all first, then the remaining groups in sorted order (deterministic).
	ordered := append([]string{"all"}, filterOut(groups, "all")...)
	for _, g := range ordered {
		fromFiles, err := t.readVarsDir(filepath.Join(t.root, "group_vars"), g)
		if err != nil {
			return nil, err
		}
		apply(fromFiles)
		apply(d.groupVars[g]) // inventory [g:vars] override the group_vars/<g> files
	}
	fromFiles, err := t.readVarsDir(filepath.Join(t.root, "host_vars"), host)
	if err != nil {
		return nil, err
	}
	apply(fromFiles)
	apply(d.hostVars[host]) // inventory inline host vars win
	return merged, nil
}

// readVarsDir reads the vars for one name (a group or host) from a vars directory, supporting BOTH the
// file form (<dir>/<name>.yml|.yaml) and the directory form (<dir>/<name>/*.yml|*.yaml, merged in filename
// order). A missing path yields an empty map (not an error) — a host/group simply may have no vars file.
func (t *Tree) readVarsDir(dir, name string) (map[string]scalar, error) {
	out := map[string]scalar{}
	// file form
	for _, ext := range []string{".yml", ".yaml"} {
		p := filepath.Join(dir, name+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			m, err := loadVarsFile(p)
			if err != nil {
				return nil, err
			}
			for k, v := range m {
				out[k] = v
			}
		}
	}
	// directory form
	sub := filepath.Join(dir, name)
	if st, err := os.Stat(sub); err == nil && st.IsDir() {
		entries, err := os.ReadDir(sub)
		if err != nil {
			return nil, fmt.Errorf("ansible: read vars dir %q: %w", sub, err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")) {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			m, err := loadVarsFile(filepath.Join(sub, n))
			if err != nil {
				return nil, err
			}
			for k, v := range m {
				out[k] = v
			}
		}
	}
	return out, nil
}

// loadVarsFile parses a single YAML vars file into flat scalars, detecting inline `!vault`-tagged values by
// their YAML tag (kept UNDECRYPTED as a vaulted scalar). Non-scalar values (nested maps/lists) are ignored —
// this connector reads flat connection vars only. A WHOLE-file-encrypted vars file cannot be introspected
// without the password (which the tree deliberately never holds), so it is skipped with no keys (the Source
// records the coverage gap); it is never a hard error that fails the entire sync. It bounds file size.
func loadVarsFile(path string) (map[string]scalar, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ansible: read vars file %q: %w", path, err)
	}
	if len(raw) > maxVarsBytes {
		return nil, fmt.Errorf("ansible: vars file %q too large (%d bytes > %d)", path, len(raw), maxVarsBytes)
	}
	out := map[string]scalar{}
	if IsEncrypted(raw) {
		// Whole-file vault: keys are opaque without the password → contribute nothing (coverage gap, not error).
		return out, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("ansible: parse vars file %q: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return out, nil // empty document
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("ansible: vars file %q top-level is not a mapping", path)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			continue
		}
		switch {
		case v.Tag == "!vault":
			out[k.Value] = scalar{text: v.Value, vaulted: true}
		case v.Kind == yaml.ScalarNode:
			out[k.Value] = scalar{text: v.Value}
		}
	}
	return out, nil
}

func filterOut(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------------------
// CredentialSource + MembershipSource (REQ-1612/1605).
// ---------------------------------------------------------------------------------------------------------

// SkipRecord records one inventory host that could not be mapped to a valid Bundle and was skipped (fail
// closed: no blank identity injected). The reason is non-secret and safe to log.
type SkipRecord struct {
	Host   string
	Reason string
}

// Source is a read-only credential.CredentialSource over an un-controlled Ansible tree. Each inventory host
// maps to one host-selector machine-plane entry whose Bundle carries SecretRef references only. It also
// implements MembershipSource (inventory group membership) so a group-selector bundle resolves for its hosts.
type Source struct {
	id   string
	tree *Tree

	defaultUser string // login user when a host declares no ansible_user (default "root")
	defaultPort int    // ssh port when a host declares no ansible_port (default 22)

	mu      sync.Mutex
	skipped []SkipRecord
}

// SourceConfig configures a Source. ID and Tree are required. DefaultUser/DefaultPort supply fallbacks for a
// host that declares no ansible_user/ansible_port.
type SourceConfig struct {
	ID          string
	Tree        *Tree
	DefaultUser string
	DefaultPort int
}

// NewSource builds a Source. It fails closed if the tree or id is missing.
func NewSource(cfg SourceConfig) (*Source, error) {
	if cfg.Tree == nil {
		return nil, errors.New("ansible: source requires a tree")
	}
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, errors.New("ansible: source requires an id")
	}
	user := strings.TrimSpace(cfg.DefaultUser)
	if user == "" {
		user = "root"
	}
	port := cfg.DefaultPort
	if port <= 0 || port > 65535 {
		port = 22
	}
	return &Source{id: strings.TrimSpace(cfg.ID), tree: cfg.Tree, defaultUser: user, defaultPort: port}, nil
}

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return s.id }

// Plane implements credential.CredentialSource — an Ansible tree feeds the machine → host plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneMachine }

// Skipped returns the hosts skipped during the most recent Sync (coverage observability); a copy.
func (s *Source) Skipped() []SkipRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkipRecord, len(s.skipped))
	copy(out, s.skipped)
	return out
}

// Sync performs the READ-ONLY pull (REQ-1612): it parses the inventory + vars tree and maps each host to a
// SourceEntry with a SecretRef-only Bundle. It FAILS CLOSED — an unreadable inventory or a malformed vars
// file aborts with an error, leaving the prior converged state intact. A host that lacks the info to build a
// valid Bundle (no key reference, unsupported connection) is skipped-with-record, NOT a whole-Sync failure.
func (s *Source) Sync(_ context.Context) ([]credential.SourceEntry, error) {
	s.mu.Lock()
	s.skipped = nil
	s.mu.Unlock()

	inv, err := s.tree.parseInventory()
	if err != nil {
		return nil, fmt.Errorf("ansible: source %q: %w", s.id, err)
	}
	entries := make([]credential.SourceEntry, 0, len(inv.hosts))
	for _, host := range inv.hosts {
		vars, err := s.tree.hostVars(host, inv)
		if err != nil {
			return nil, fmt.Errorf("ansible: source %q host %q: %w", s.id, host, err)
		}
		e, ok, reason := s.toEntry(host, vars)
		if !ok {
			s.record(host, reason)
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// toEntry maps one host's merged vars to a SourceEntry, or reports (false, reason) when it cannot build a
// valid, SecretRef-only Bundle. It NEVER copies a secret VALUE into the Bundle: the ssh key is a file:
// reference (a non-secret path) and the become secret is an ansible-vault: LOCATOR resolved at use time.
func (s *Source) toEntry(host string, vars map[string]scalar) (credential.SourceEntry, bool, string) {
	user := firstScalar(vars, "ansible_user", "ansible_ssh_user")
	if user == "" {
		user = s.defaultUser
	}
	port := s.defaultPort
	if p := firstScalar(vars, "ansible_port", "ansible_ssh_port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return credential.SourceEntry{}, false, "invalid ansible_port " + strconv.Quote(p)
		}
		port = n
	}
	scheme, ok := mapScheme(scalarText(vars, "ansible_connection"))
	if !ok {
		return credential.SourceEntry{}, false, "unsupported ansible_connection " + strconv.Quote(scalarText(vars, "ansible_connection"))
	}

	spec := credential.BundleSpec{User: user, Port: port, Scheme: scheme}
	// The per-host sudo/become secret (if declared) is carried as an ansible-vault: LOCATOR — NEVER its value,
	// even when the underlying var is plaintext (INV-13). Resolved (and natively decrypted when vaulted) at use
	// time by this connector's registered scheme resolver.
	if _, has := vars[becomeVar]; has {
		spec.Become = config.SecretRef(Scheme + ":" + host + "#" + becomeVar)
	}
	// The ssh/netconf key is referenced by PATH (a non-secret file: reference), from an explicit sealed
	// tg_secret_ref or from ansible_ssh_private_key_file.
	switch scheme {
	case credential.SchemeSSH, credential.SchemeNetconf:
		ref, reason := keyRef(vars, s.tree.root)
		if ref == "" {
			return credential.SourceEntry{}, false, reason
		}
		spec.SSHKeyRef = ref
	case credential.SchemeAPI:
		ref := strings.TrimSpace(scalarText(vars, "tg_secret_ref"))
		if ref == "" {
			return credential.SourceEntry{}, false, "api connection needs a sealed tg_secret_ref (api token reference)"
		}
		spec.APITokenRef = config.SecretRef(ref)
	case credential.SchemeWinRM:
		if spec.Become == "" {
			return credential.SourceEntry{}, false, "winrm connection needs an " + becomeVar + " (password reference)"
		}
	}

	b, err := credential.NewBundle(spec)
	if err != nil {
		// A non-sealed reference or any other construction failure is a skip — never a blank/literal identity.
		return credential.SourceEntry{}, false, "bundle rejected: " + err.Error()
	}
	return credential.SourceEntry{
		NativeID: "host:" + host,
		Selector: credential.Selector{Kind: credential.KindHost, Pattern: host},
		Bundle:   b,
	}, true, ""
}

// keyRef derives the sealed ssh-key SecretRef for a host: an explicit sealed tg_secret_ref (verbatim) wins,
// else ansible_ssh_private_key_file becomes a file: reference (relative paths resolve against the tree root).
// Returns ("", reason) when no key reference can be derived (fail closed — no guessed identity).
func keyRef(vars map[string]scalar, root string) (config.SecretRef, string) {
	if explicit := strings.TrimSpace(scalarText(vars, "tg_secret_ref")); explicit != "" {
		return config.SecretRef(explicit), "" // NewBundle enforces it is a sealed scheme
	}
	kf := strings.TrimSpace(scalarText(vars, "ansible_ssh_private_key_file"))
	if kf == "" {
		return "", "no key reference (set ansible_ssh_private_key_file or a sealed tg_secret_ref)"
	}
	if !filepath.IsAbs(kf) {
		kf = filepath.Join(root, kf)
	}
	return config.SecretRef("file:" + kf), ""
}

// Membership implements credential.MembershipSource (REQ-1605): host → the inventory groups it belongs to
// (direct + [g:children] ancestors + "all"). READ-ONLY and NON-SECRET (host and group names only). It fails
// closed on an unreadable inventory (returns an error and no partial map), leaving the prior index intact.
func (s *Source) Membership(_ context.Context) (map[string][]string, error) {
	inv, err := s.tree.parseInventory()
	if err != nil {
		return nil, fmt.Errorf("ansible: source %q membership: %w", s.id, err)
	}
	out := make(map[string][]string, len(inv.hosts))
	for _, h := range inv.hosts {
		out[h] = inv.groupsForHost(h)
	}
	return out, nil
}

func (s *Source) record(host, reason string) {
	s.mu.Lock()
	s.skipped = append(s.skipped, SkipRecord{Host: host, Reason: reason})
	s.mu.Unlock()
}

// compile-time proof the source satisfies the stable credential-source interface AND the optional
// membership-source capability (host↔group reconciliation, REQ-1605).
var _ credential.CredentialSource = (*Source)(nil)
var _ credential.MembershipSource = (*Source)(nil)

// ---------------------------------------------------------------------------------------------------------
// ansible-vault: SecretRef scheme resolver (REQ-1612/1603, natively decrypting at use time).
// ---------------------------------------------------------------------------------------------------------

// Resolver dereferences an ansible-vault: SecretRef by re-reading the referenced host var from the tree and
// natively decrypting it when it is an inline `!vault` payload. It holds only the vault PASSWORD reference(s)
// (SecretRefs) — the plaintext exists only in memory at use time (INV-13). Fail-closed on every error path.
type Resolver struct {
	tree        *Tree
	passwordRef config.SecretRef            // the default vault password (a SecretRef: file:/store:/env:)
	idRefs      map[string]config.SecretRef // optional per-vault-id passwords (1.2 header labels)
}

// ResolverConfig configures a Resolver. Tree and PasswordRef are required. IDRefs (optional) maps a 1.2
// vault-id label to its own password SecretRef; the default PasswordRef is always tried as well.
type ResolverConfig struct {
	Tree        *Tree
	PasswordRef config.SecretRef
	IDRefs      map[string]config.SecretRef
}

// NewResolver builds a Resolver. It fails closed if the tree or password reference is missing — an
// ansible-vault: scheme with no password could never decrypt anything, so it must not silently register.
func NewResolver(cfg ResolverConfig) (*Resolver, error) {
	if cfg.Tree == nil {
		return nil, errors.New("ansible: resolver requires a tree")
	}
	if strings.TrimSpace(string(cfg.PasswordRef)) == "" {
		return nil, errors.New("ansible: resolver requires a vault password SecretRef")
	}
	return &Resolver{tree: cfg.Tree, passwordRef: cfg.PasswordRef, idRefs: cfg.IDRefs}, nil
}

// ResolveRef dereferences a full "ansible-vault:<host>#<var>" reference. It re-reads the host's vars from the
// tree (INV-05), reads the named var, and returns its plaintext: natively decrypting an inline `!vault`
// payload (trying the vault-id-matched password first, then the default), or returning a plaintext scalar
// as-is. It FAILS CLOSED — an unknown host/var, an unreadable tree, or a failed decrypt/HMAC returns an error,
// never a blank or default value.
func (r *Resolver) ResolveRef(ref string) (string, error) {
	_, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("ansible: malformed secret reference %q", ref)
	}
	host, field, ok := strings.Cut(rest, "#")
	host, field = strings.TrimSpace(host), strings.TrimSpace(field)
	if !ok || host == "" || field == "" {
		return "", fmt.Errorf("ansible: reference %q must be %s:<host>#<var>", ref, Scheme)
	}
	inv, err := r.tree.parseInventory()
	if err != nil {
		return "", err // fail closed on an unreadable tree
	}
	vars, err := r.tree.hostVars(host, inv)
	if err != nil {
		return "", err
	}
	sc, ok := vars[field]
	if !ok {
		return "", fmt.Errorf("ansible: reference %q: host %q has no var %q (fail closed)", ref, host, field)
	}
	if !sc.vaulted {
		return sc.text, nil // a plaintext scalar — no decrypt needed
	}
	return r.decrypt([]byte(sc.text))
}

// decrypt natively decrypts a vault payload, trying the vault-id-matched password (if the payload carries a
// known 1.2 id) first, then the default password. Every candidate is gated by the HMAC, so trying more than
// one is safe (a wrong password fails the MAC, never yields garbage). Fails closed if none validates.
func (r *Resolver) decrypt(payload []byte) (string, error) {
	env, err := parseVaultEnvelope(payload)
	if err != nil {
		return "", err
	}
	var refs []config.SecretRef
	if env.vaultID != "" {
		if ref, ok := r.idRefs[env.vaultID]; ok {
			refs = append(refs, ref)
		}
	}
	refs = append(refs, r.passwordRef) // the default is always a candidate
	var lastErr error
	for _, pref := range refs {
		pw, err := pref.Resolve()
		if err != nil {
			lastErr = err
			continue
		}
		plain, err := Decrypt(payload, []byte(pw))
		if err == nil {
			return string(plain), nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrMACMismatch
	}
	return "", fmt.Errorf("ansible: decrypt failed (fail closed): %w", lastErr)
}

// RegisterResolver wires a Resolver's ansible-vault: scheme into core/config so a Bundle SecretRef like
// "ansible-vault:librespeed01#ansible_become_pass" is dereferenced (and natively decrypted) at use time.
// Composition-time only. Pass nil to unregister (the scheme then fails closed).
func RegisterResolver(r *Resolver) {
	if r == nil {
		config.RegisterSchemeResolver(Scheme, nil)
		return
	}
	config.RegisterSchemeResolver(Scheme, r.ResolveRef)
}

// ---- small helpers ----------------------------------------------------------------------------------

// mapScheme maps an ansible_connection value to TG's closed ConnectionScheme vocabulary (mirrors the AWX
// connector). An empty connection defaults to ssh (Ansible's own default); "local"/unknown are unsupported.
func mapScheme(conn string) (credential.ConnectionScheme, bool) {
	switch strings.ToLower(strings.TrimSpace(conn)) {
	case "", "ssh", "smart", "paramiko", "paramiko_ssh", "libssh":
		return credential.SchemeSSH, true
	case "network_cli", "netconf", "ansible.netcommon.network_cli", "ansible.netcommon.netconf":
		return credential.SchemeNetconf, true
	case "winrm", "psrp":
		return credential.SchemeWinRM, true
	case "httpapi", "ansible.netcommon.httpapi":
		return credential.SchemeAPI, true
	default:
		return "", false
	}
}

// scalarText returns a var's plaintext value, or "" for an absent OR vaulted var (a vaulted connection var is
// not a usable plaintext scalar here — connection metadata is never vaulted in this connector's model).
func scalarText(vars map[string]scalar, key string) string {
	if sc, ok := vars[key]; ok && !sc.vaulted {
		return sc.text
	}
	return ""
}

// firstScalar returns the first non-empty plaintext value among the keys.
func firstScalar(vars map[string]scalar, keys ...string) string {
	for _, k := range keys {
		if v := scalarText(vars, k); v != "" {
			return v
		}
	}
	return ""
}
