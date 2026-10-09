package ansible

// ORACLE tests for the un-controlled Ansible module's console TEST probe (core/selftest.Tester). There is no
// fake here and there does not need to be: this module's "network" is the FILESYSTEM, so the tests drive the
// REAL Resolver over the REAL demo tree (the same ansible-vault fixtures the sync oracles use, produced by
// the actual ansible-vault CLI) with the password supplied through a REAL file: SecretRef. They prove: the
// Summary reports what the TREE contained and names the value it decrypted; the decrypted plaintext never
// appears anywhere; a wrong password FAILS — which is the killing oracle, because every configured value is
// present and only the secret is wrong; an unreadable root and an unresolvable password reference are
// classified as different faults; and a tree with nothing vaulted passes while saying plainly that the
// password was not exercised.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
)

// probeResolver builds the REAL Resolver over root with the vault password read from a REAL file: reference
// — every configured value present and non-empty, which is the precondition the killing oracle depends on.
func probeResolver(t *testing.T, root, password string) *Resolver {
	t.Helper()
	passFile := filepath.Join(t.TempDir(), "vault-pass")
	mustWrite(t, passFile, password+"\n") // the trailing newline is trimmed by file: resolution
	tree, err := NewTree(root, "")
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	r, err := NewResolver(ResolverConfig{Tree: tree, PasswordRef: config.SecretRef("file:" + passFile)})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestSelfTestDecryptsAndReportsWhatItRead(t *testing.T) {
	root, _ := writeDemoTree(t)
	r := probeResolver(t, root, demoVaultPassword)

	res, err := r.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	// Every fact comes from the TREE: the root, the inventory file that was actually chosen (the conventional
	// search order picks one of four names — naming it is how an operator sees the wrong file was picked),
	// the host count, and the vaulted value that was decrypted.
	for _, want := range []string{root, "inventory.ini", "2 hosts", "librespeed01#ansible_become_pass", "decrypted"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
	if res.Detail != "" {
		t.Fatalf("a healthy tree must not warn: %q", res.Detail)
	}
	// Rule 5 with the strongest possible teeth: the probe just decrypted a real sudo password.
	if strings.Contains(res.Summary+res.Detail, "librespeed01-sudo-pw") ||
		strings.Contains(res.Summary+res.Detail, demoVaultPassword) {
		t.Fatalf("the probe leaked secret material: %q / %q", res.Summary, res.Detail)
	}
}

func TestSelfTestPicksTheSameTargetEveryTime(t *testing.T) {
	// Map iteration in Go is randomised. Two presses that decrypt different values would leave two operators
	// comparing incomparable results, and a failure would name a value that is not the one that failed last
	// time.
	root, _ := writeDemoTree(t)
	r := probeResolver(t, root, demoVaultPassword)

	first, err := r.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := r.SelfTest(context.Background(), "alice")
		if err != nil {
			t.Fatalf("SelfTest: %v", err)
		}
		if again.Summary != first.Summary {
			t.Fatalf("probe target is not deterministic:\n %q\n %q", first.Summary, again.Summary)
		}
	}
}

func TestSelfTestFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		password   string
		mutate     func(t *testing.T, root string) // break the tree after it is written
		noPassFile bool
		wantDetail []string
	}{
		{
			// THE KILLING ORACLE, and for this module it is the whole reason the probe exists. Root set,
			// inventory present, password reference present and RESOLVING to a real value — only the value
			// itself is wrong. A "the configured values are non-empty" check passes; the real probe must not.
			name:       "a wrong vault password fails",
			password:   "not-the-demo-password",
			wantDetail: []string{"DOES NOT DECRYPT", "message authentication"},
		},
		{
			// The reference resolves to nothing: a TG-side fault, and a different fix from a wrong password.
			name:       "an unresolvable password reference is named as TG-side",
			noPassFile: true,
			wantDetail: []string{"could not be READ from its reference"},
		},
		{
			// The tree disappears after boot — a detached bind mount, which presents as a sync that quietly
			// stops learning hosts.
			name:     "an unreadable root is named as a mount problem",
			password: demoVaultPassword,
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			},
			wantDetail: []string{"bind mount"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := writeDemoTree(t)
			var r *Resolver
			if tc.noPassFile {
				tree, err := NewTree(root, "")
				if err != nil {
					t.Fatalf("NewTree: %v", err)
				}
				// A reference that is CONFIGURED but points at a file that is not there.
				r, err = NewResolver(ResolverConfig{
					Tree:        tree,
					PasswordRef: config.SecretRef("file:" + filepath.Join(t.TempDir(), "absent-vault-pass")),
				})
				if err != nil {
					t.Fatalf("NewResolver: %v", err)
				}
			} else {
				r = probeResolver(t, root, tc.password)
			}
			if tc.mutate != nil {
				tc.mutate(t, root)
			}

			res, err := r.SelfTest(context.Background(), "alice")
			if err == nil {
				t.Fatalf("expected an error, got summary=%q detail=%q", res.Summary, res.Detail)
			}
			if res.Detail == "" {
				t.Fatal("a failed probe must carry an actionable Detail, never a bare error")
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Fatalf("detail %q does not carry %q", res.Detail, want)
				}
			}
			if strings.Contains(res.Summary+res.Detail+err.Error(), demoVaultPassword) {
				t.Fatal("the probe leaked the vault password into its result or error")
			}
		})
	}
}

func TestSelfTestSaysWhenNothingWasVaulted(t *testing.T) {
	// A tree with no !vault value anywhere: the probe cannot exercise the password, and the one thing it must
	// not do is imply that it did.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "inventory.ini"), "[web]\nweb01\n")
	mustMkdir(t, filepath.Join(root, "group_vars"))
	mustWrite(t, filepath.Join(root, "group_vars", "all.yml"), "ansible_user: root\n")
	r := probeResolver(t, root, demoVaultPassword)

	res, err := r.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("a readable tree with nothing vaulted is a pass: %v", err)
	}
	if !strings.Contains(res.Detail, "NOT exercised") {
		t.Fatalf("the probe must say the password was not exercised: %q", res.Detail)
	}
	if strings.Contains(res.Summary, "decrypted ") {
		t.Fatalf("the probe must not claim a decrypt it did not perform: %q", res.Summary)
	}
}

// TestResolverImplementsTester pins the capability the console detects by assertion — and pins WHICH object
// carries it. The composition root must offer the RESOLVER: the Source holds the same tree but not the vault
// password, so a probe built from it would pass with a wrong secret.
func TestResolverImplementsTester(t *testing.T) {
	root, _ := writeDemoTree(t)
	if _, ok := selftest.Of(probeResolver(t, root, demoVaultPassword)); !ok {
		t.Fatal("the ansible resolver must be detected as a selftest.Tester")
	}
	src, err := NewSource(SourceConfig{ID: "ansible", Tree: probeResolver(t, root, demoVaultPassword).tree})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if _, ok := selftest.Of(src); ok {
		t.Fatal("the Source must NOT advertise a self-test: it cannot reach the vault password, so its " +
			"probe could not tell a right password from a wrong one")
	}
}
