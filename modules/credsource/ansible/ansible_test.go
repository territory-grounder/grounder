package ansible

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// ---------------------------------------------------------------------------------------------------------
// REAL ansible-vault fixtures. Produced by the actual `ansible-vault encrypt_string` CLI (ansible-core
// 2.19.7) with the demo password below; they are the SAME payloads placed in the on-box un-controlled
// Ansible demo tree (/srv/ansible-uncontrolled-demo). The salt is embedded, so each is a DETERMINISTIC known
// vector: Decrypt must round-trip it to the known plaintext with the demo password, and MUST fail closed with
// any other password (HMAC) or on any tamper.
// ---------------------------------------------------------------------------------------------------------

const demoVaultPassword = "tg-demo-vault-pass"

// librespeed01 ansible_become_pass -> "librespeed01-sudo-pw"
const vaultLibrespeed = `$ANSIBLE_VAULT;1.1;AES256
36643866386639623436666233626463363139643466663034666332383764343039323133376339
3561326362343130633964363261613063626263343433350a396337356235336166663564323534
38316538303363623264366536616264656461383437393166356232363139393230346330383731
3961393637613038660a326231373831636330336132313131393462346463313364663066333733
33336134636538393365373364623735393030653761656634323932353964386532`

// myspeed01 ansible_become_pass -> "myspeed01-sudo-pw"
const vaultMyspeed = `$ANSIBLE_VAULT;1.1;AES256
30383565363362323430626637636335613639353035333434343161646539383935656639316237
6364626533373238653734323166646339363966393464640a376531346466343132333436373537
30356439343737633535323062323965363261383439653038396632393634346263343939666638
6362303639376432350a393262316333623663616237373136653262333439393863383032313539
38313332373735336238393962626335333265666130623039626666666432333761`

const (
	plainLibrespeed = "librespeed01-sudo-pw"
	plainMyspeed    = "myspeed01-sudo-pw"
)

// TestDecryptRealPayloadKnownVector proves the native Go decrypter round-trips a REAL ansible-vault AES256
// payload (created by the ansible-vault CLI) to its known plaintext — no subprocess, stdlib crypto only.
func TestDecryptRealPayloadKnownVector(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"librespeed01", vaultLibrespeed, plainLibrespeed},
		{"myspeed01", vaultMyspeed, plainMyspeed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decrypt([]byte(tc.payload), []byte(demoVaultPassword))
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("plaintext = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecryptFailClosed proves the decrypter fails closed on a wrong password (HMAC), a tampered ciphertext
// (HMAC), and a non-vault input — never returning garbage plaintext.
func TestDecryptFailClosed(t *testing.T) {
	t.Run("wrong password -> HMAC mismatch", func(t *testing.T) {
		_, err := Decrypt([]byte(vaultLibrespeed), []byte("not-the-password"))
		if err == nil || !strings.Contains(err.Error(), "HMAC") {
			t.Fatalf("want HMAC failure, got %v", err)
		}
	})

	t.Run("tampered ciphertext -> HMAC mismatch", func(t *testing.T) {
		// Flip one hex digit in the last (ciphertext) body line.
		lines := strings.Split(vaultLibrespeed, "\n")
		last := lines[len(lines)-1]
		if last[0] == '3' {
			lines[len(lines)-1] = "4" + last[1:]
		} else {
			lines[len(lines)-1] = "3" + last[1:]
		}
		_, err := Decrypt([]byte(strings.Join(lines, "\n")), []byte(demoVaultPassword))
		if err == nil {
			t.Fatal("tampered payload decrypted without error (fail-open!)")
		}
	})

	t.Run("not a vault payload -> ErrNotVault", func(t *testing.T) {
		if _, err := Decrypt([]byte("just some plaintext"), []byte(demoVaultPassword)); err == nil {
			t.Fatal("non-vault input decrypted without error")
		} else if !strings.Contains(err.Error(), "not an $ANSIBLE_VAULT") {
			t.Fatalf("want ErrNotVault, got %v", err)
		}
	})

	t.Run("empty/garbage body -> error", func(t *testing.T) {
		if _, err := Decrypt([]byte("$ANSIBLE_VAULT;1.1;AES256\nZZZZ"), []byte(demoVaultPassword)); err == nil {
			t.Fatal("garbage body decrypted without error")
		}
	})
}

// TestPKCS7Unpad exercises the padding validator's fail-closed branches directly.
func TestPKCS7Unpad(t *testing.T) {
	// valid: 16-byte block, pad of 4
	good := append([]byte("abcdefghijkl"), 4, 4, 4, 4)
	out, err := pkcs7Unpad(good)
	if err != nil || string(out) != "abcdefghijkl" {
		t.Fatalf("valid unpad: out=%q err=%v", out, err)
	}
	for _, bad := range [][]byte{
		{},                                     // empty
		append([]byte("abc"), 1),               // not a block multiple
		append(make([]byte, 15), 17),           // pad byte > block size
		append([]byte("abcdefghijklmn"), 2, 3), // inconsistent tail (16 bytes, pad=3 but prev != 3)
	} {
		if _, err := pkcs7Unpad(bad); err == nil {
			t.Fatalf("expected pkcs7 error for %v", bad)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------
// Tree + Source + Resolver — end-to-end against a REAL vaulted demo tree written to a tempdir.
// ---------------------------------------------------------------------------------------------------------

// writeDemoTree writes an un-controlled Ansible layout mirroring the on-box demo: an INI inventory with two
// hosts + group nesting, group_vars/all connection defaults (with a real key file), and host_vars carrying
// the REAL ansible-vault-encrypted ansible_become_pass. It returns the root and the key-file path.
func writeDemoTree(t *testing.T) (root, keyFile string) {
	t.Helper()
	root = t.TempDir()
	keyFile = filepath.Join(root, "keys", "demo.key")
	mustMkdir(t, filepath.Dir(keyFile))
	// PEM markers assembled from split literals ON PURPOSE (same convention as core/screen/screen_test.go):
	// runtime value is byte-identical to a real key file, but the contiguous begin-private-key string never
	// appears in source, so the public-mirror's private-key guard (github-sync/denylist.txt) stays strict.
	mustWrite(t, keyFile, "-----BEGIN OPENSSH "+"PRIVATE KEY-----\nDEMO-KEY-MATERIAL\n-----END OPENSSH "+"PRIVATE KEY-----\n")

	mustWrite(t, filepath.Join(root, "inventory.ini"), `# un-controlled demo (TEST)
[speedtest]
librespeed01
myspeed01

[nl_site]
librespeed01

[gr_site]
myspeed01

[region:children]
nl_site
gr_site
`)
	mustMkdir(t, filepath.Join(root, "group_vars"))
	mustWrite(t, filepath.Join(root, "group_vars", "all.yml"),
		"ansible_user: root\nansible_port: 22\nansible_ssh_private_key_file: "+keyFile+"\n")

	mustMkdir(t, filepath.Join(root, "host_vars"))
	mustWrite(t, filepath.Join(root, "host_vars", "librespeed01.yml"),
		"ansible_become: true\n"+vaultYAMLFragment(vaultLibrespeed))
	mustWrite(t, filepath.Join(root, "host_vars", "myspeed01.yml"),
		"ansible_become: true\n"+vaultYAMLFragment(vaultMyspeed))
	return root, keyFile
}

// vaultYAMLFragment renders a raw $ANSIBLE_VAULT block as an inline `!vault |` block scalar under
// ansible_become_pass, 2-space indented — exactly how `ansible-vault encrypt_string` embeds it.
func vaultYAMLFragment(block string) string {
	var b strings.Builder
	b.WriteString("ansible_become_pass: !vault |\n")
	for _, ln := range strings.Split(block, "\n") {
		b.WriteString("  ")
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return b.String()
}

func TestSourceSyncMembershipAndResolve(t *testing.T) {
	root, keyFile := writeDemoTree(t)
	passFile := filepath.Join(t.TempDir(), "vault-pass")
	mustWrite(t, passFile, demoVaultPassword+"\n") // a trailing newline is trimmed by file: resolution

	tree, err := NewTree(root, "")
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	src, err := NewSource(SourceConfig{ID: "ansible-demo", Tree: tree})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if src.Plane() != credential.PlaneMachine {
		t.Fatalf("plane = %s, want machine", src.Plane())
	}

	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (skipped=%v)", len(entries), src.Skipped())
	}

	// Find librespeed01's entry and assert the Bundle carries LOCATORS only (INV-13) — never a plaintext.
	byHost := map[string]credential.SourceEntry{}
	for _, e := range entries {
		byHost[e.Selector.Pattern] = e
	}
	lib, ok := byHost["librespeed01"]
	if !ok {
		t.Fatalf("no entry for librespeed01; have %v", byHost)
	}
	if lib.Selector.Kind != credential.KindHost {
		t.Fatalf("selector kind = %s, want host", lib.Selector.Kind)
	}
	wantBecome := config.SecretRef("ansible-vault:librespeed01#ansible_become_pass")
	if lib.Bundle.BecomeRef() != wantBecome {
		t.Fatalf("become ref = %q, want %q", lib.Bundle.BecomeRef(), wantBecome)
	}
	if lib.Bundle.SSHKeyRef() != config.SecretRef("file:"+keyFile) {
		t.Fatalf("ssh key ref = %q, want file:%s", lib.Bundle.SSHKeyRef(), keyFile)
	}
	// INV-13: the plaintext sudo password must appear in NO field of the synced entry.
	if strings.Contains(string(lib.Bundle.BecomeRef())+string(lib.Bundle.SSHKeyRef()), plainLibrespeed) {
		t.Fatal("plaintext become password leaked into a Bundle SecretRef")
	}

	// Membership: [region:children]{nl_site,gr_site} means librespeed01 ∈ {all,speedtest,nl_site,region}.
	mem, err := src.Membership(context.Background())
	if err != nil {
		t.Fatalf("Membership: %v", err)
	}
	assertGroups(t, mem["librespeed01"], "all", "speedtest", "nl_site", "region")
	assertGroups(t, mem["myspeed01"], "all", "speedtest", "gr_site", "region")

	// Register the ansible-vault: resolver (holds only the password ref) and resolve END-TO-END.
	resolver, err := NewResolver(ResolverConfig{Tree: tree, PasswordRef: config.SecretRef("file:" + passFile)})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	RegisterResolver(resolver)
	t.Cleanup(func() { RegisterResolver(nil) })

	// (a) direct through the registered core/config scheme (the path the engine uses at use time).
	got, err := config.SecretRef(wantBecome).Resolve()
	if err != nil {
		t.Fatalf("scheme resolve: %v", err)
	}
	if got != plainLibrespeed {
		t.Fatalf("decrypted become = %q, want %q", got, plainLibrespeed)
	}

	// (b) through Bundle.Resolve() — the full identity load (key file + decrypted become) at use time.
	res, err := lib.Bundle.Resolve()
	if err != nil {
		t.Fatalf("Bundle.Resolve: %v", err)
	}
	if res.Become != plainLibrespeed {
		t.Fatalf("resolved become = %q, want %q", res.Become, plainLibrespeed)
	}
	if !strings.Contains(res.SSHKey, "DEMO-KEY-MATERIAL") {
		t.Fatalf("resolved ssh key not loaded: %q", res.SSHKey)
	}
}

// TestResolveFailClosed proves the resolver fails closed on a wrong password and an unknown host/var — never
// a blank or default value.
func TestResolveFailClosed(t *testing.T) {
	root, _ := writeDemoTree(t)
	tree, err := NewTree(root, "")
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	badPass := filepath.Join(t.TempDir(), "bad")
	mustWrite(t, badPass, "wrong-password")
	resolver, err := NewResolver(ResolverConfig{Tree: tree, PasswordRef: config.SecretRef("file:" + badPass)})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if _, err := resolver.ResolveRef("ansible-vault:librespeed01#ansible_become_pass"); err == nil {
		t.Fatal("wrong password resolved without error (fail-open!)")
	}
	if _, err := resolver.ResolveRef("ansible-vault:nosuchhost#ansible_become_pass"); err == nil {
		t.Fatal("unknown host resolved without error")
	}
	if _, err := resolver.ResolveRef("ansible-vault:librespeed01#nosuchvar"); err == nil {
		t.Fatal("unknown var resolved without error")
	}
	// A resolver with no password ref refuses construction (fail closed).
	if _, err := NewResolver(ResolverConfig{Tree: tree}); err == nil {
		t.Fatal("resolver built with no password ref")
	}
}

// TestResolvePlaintextPassthrough proves a NON-vaulted var resolves as its plaintext scalar (a vars file may
// mix vaulted and plaintext values) — while the Source still stores only the locator, never the value.
func TestResolvePlaintextPassthrough(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "inventory.ini"), "[web]\nhostA\n")
	mustMkdir(t, filepath.Join(root, "host_vars"))
	mustWrite(t, filepath.Join(root, "host_vars", "hostA.yml"), "ansible_become_pass: plain-sudo\n")
	tree, _ := NewTree(root, "")
	resolver, _ := NewResolver(ResolverConfig{Tree: tree, PasswordRef: config.SecretRef("env:UNUSED")})
	got, err := resolver.ResolveRef("ansible-vault:hostA#ansible_become_pass")
	if err != nil {
		t.Fatalf("resolve plaintext: %v", err)
	}
	if got != "plain-sudo" {
		t.Fatalf("plaintext passthrough = %q, want plain-sudo", got)
	}
}

// TestSourceSkipsHostWithNoKey proves a host with no key reference is skipped-with-record (fail closed), not
// injected as a blank identity, while good hosts still resolve.
func TestSourceSkipsHostWithNoKey(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "inventory.ini"), "[web]\nnokey\ngood ansible_ssh_private_key_file=/secrets/k\n")
	mustMkdir(t, filepath.Join(root, "group_vars"))
	mustWrite(t, filepath.Join(root, "group_vars", "all.yml"), "ansible_user: root\n")
	tree, _ := NewTree(root, "")
	src, _ := NewSource(SourceConfig{ID: "t", Tree: tree})
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(entries) != 1 || entries[0].Selector.Pattern != "good" {
		t.Fatalf("want 1 entry (good), got %v", entries)
	}
	if len(src.Skipped()) != 1 || src.Skipped()[0].Host != "nokey" {
		t.Fatalf("want nokey skipped-with-record, got %v", src.Skipped())
	}
}

// ---- helpers ----

func assertGroups(t *testing.T, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Fatalf("membership %v missing group %q", got, w)
		}
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
