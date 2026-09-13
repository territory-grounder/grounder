package skills

// TG-471 (epic TG-114, C-2) — THE BYTE-IDENTITY GOLDEN. Written and committed BEFORE the go:embed swap,
// against the compiled Go-string bodies; the swap must reproduce every body and every composed seed
// byte-for-byte, which is what makes the refactor provably non-behavioral (the eval-gate waiver's
// evidence, TG-394 precedent). Killing mutations: (a) edit one byte of one seeds/*.md — its body hash
// reddens; (b) drop a skill from the registry — the roster reddens; (c) reorder — the compose hash reddens.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/territory-grounder/grounder/core/execclass"
)

// bodyGoldens: sha256 of each skill's EXACT body bytes at extraction time (2026-08-14, pre-embed).
var bodyGoldens = map[string]string{
	"proving-your-work":        "fba57ceeb556ad786deb085687b7622749a50f3089ea88af4d33d0545f006c4c",
	"loop-red-flags":           "be169a98e2d608aec7f45f7a87ef2990b15c3184437785f91b644cfbefc6cca7",
	"debugging-protocol":       "ef9b8c1b463e0319beea6c1829a60cc6fad3992324ec1de36d81a575730cea84",
	"shortcuts-to-resist":      "183419cd226be0160e7f08fbee9bd1ae4ab79af506f7cc264d21042d3ad21902",
	"k8s-triage":               "a04abdb90075e63161bc8b7481112b85569a98a4c7e2a2c9179a3560e9d81f56",
	"proxmox-triage":           "b5758d00d6775b768da678e13283a973ad895dd3e70dd3063347a8a0fcd53cc4",
	"cisco-triage":             "94167dc5ded3cd0655aa1ee0a7ac058d40ef33a71b44410a3363de1906225bde",
	"linux-triage":             "f2da144b823521d4e8c7e14a6a425f0b8bed8f6437c9458b3d04dd56e433a4ae",
	"storage-triage":           "5f0a4182a1ce3aae418c9e6ef9bdd97bca3a404d649f6f1f9caa6fe4cd74791c",
	"conservative-remediation": "3de4d83202e6efc2e01e32708e247630824bfea00beecc0d40c0ef7816d2c5ab",
	"triage-protocol":          "6107856cf781aad8071b2de42da27ceba3c7d7cf98dd01dac22d6e8a4a181fc2",
	"alert-class-playbooks":    "f5c48652e80ea67c7a83f846dcf7ad465579cab7783ca7441eb2512e0d8a2f33",
	"correlated-triage":        "4978c364c25823231247131b2751ffaef85e7f208f0ddd414c8c186f136cbc8a",
	"exec-safety":              "3d0ad3ab2961ecf241ee7ace0ec8312eafd47b35c8aaaed13da797392735af2a",
}

// composeGoldens: sha256 of the full Compose output per (phase, execclass, domain) triple, pinning
// selection + order + separator, not only the bodies.
var composeGoldens = map[string]string{
	"investigate||":                             "997678987acffb1c0b20b31ac003ce0b6e52e6edf5dea388e7cbc378953959ec",
	"investigate||kubernetes":                   "2b51d05012f06fa1f07337fd09f27cea235628d7edfd8bde944dabcf7fe450c2",
	"investigate||proxmox":                      "d83da16f89f5999b11cca739912c314059a5f21070774204392541661b96878c",
	"investigate||cisco":                        "c07f23e3926746f8591c78d60748933f28a0424e59c8d3748cedb11cac3b7004",
	"investigate||linux":                        "673aee08ab27c78556642e82120a1a8f3666bd06463e5d29cda79e33972231f1",
	"investigate||storage":                      "5a462bc123fd7fe546c2255db4e140b801dee464e1fe3790aad216e13439b6da",
	"investigate|FAST_AGENT|":                   "46d8778261f47a3e8b42cac8297df6a57232e1236ea511fdf7e6d590539b8b1d",
	"investigate|FAST_AGENT|kubernetes":         "46d8778261f47a3e8b42cac8297df6a57232e1236ea511fdf7e6d590539b8b1d",
	"investigate|FAST_AGENT|proxmox":            "46d8778261f47a3e8b42cac8297df6a57232e1236ea511fdf7e6d590539b8b1d",
	"investigate|FAST_AGENT|cisco":              "46d8778261f47a3e8b42cac8297df6a57232e1236ea511fdf7e6d590539b8b1d",
	"investigate|FAST_AGENT|linux":              "46d8778261f47a3e8b42cac8297df6a57232e1236ea511fdf7e6d590539b8b1d",
	"investigate|FAST_AGENT|storage":            "46d8778261f47a3e8b42cac8297df6a57232e1236ea511fdf7e6d590539b8b1d",
	"investigate|STANDARD_AGENT|":               "997678987acffb1c0b20b31ac003ce0b6e52e6edf5dea388e7cbc378953959ec",
	"investigate|STANDARD_AGENT|kubernetes":     "2b51d05012f06fa1f07337fd09f27cea235628d7edfd8bde944dabcf7fe450c2",
	"investigate|STANDARD_AGENT|proxmox":        "d83da16f89f5999b11cca739912c314059a5f21070774204392541661b96878c",
	"investigate|STANDARD_AGENT|cisco":          "c07f23e3926746f8591c78d60748933f28a0424e59c8d3748cedb11cac3b7004",
	"investigate|STANDARD_AGENT|linux":          "673aee08ab27c78556642e82120a1a8f3666bd06463e5d29cda79e33972231f1",
	"investigate|STANDARD_AGENT|storage":        "5a462bc123fd7fe546c2255db4e140b801dee464e1fe3790aad216e13439b6da",
	"investigate|DEEP_INVESTIGATION|":           "6751ca1cbd314b36b4171b89fc004f0c2dae731ac929c39db83264ead0679c7f",
	"investigate|DEEP_INVESTIGATION|kubernetes": "d757625b9f4271c5ed849620752823a81fbecd3e072090411456ce1c40210194",
	"investigate|DEEP_INVESTIGATION|proxmox":    "fd9284f64b5d41ae91286100ddbc4c088cbe5a64242624b6384778bc49b14838",
	"investigate|DEEP_INVESTIGATION|cisco":      "f6ee0032a0f51e9969e68d54a6c00fb239e0f1da31331769a29e380c91555dc8",
	"investigate|DEEP_INVESTIGATION|linux":      "b36091fc8f95184aecd90f01331ffb5f03ba98d6dde0208807d78dd546ab211f",
	"investigate|DEEP_INVESTIGATION|storage":    "65ae90159550118295523b0f628389fff734c416a2f15aef217cebb4e4ec3d60",
	"execute||":                                 "fe767169cf6ee1b6a767d92b2b1f90012e5f59ddad5d099ca64509cf520d0078",
	"execute||kubernetes":                       "f803936c9c1bc3d5d4755582c746a02416262d0a94b753b7a7381821aec2b77b",
	"execute||proxmox":                          "fd123eb02a1e5251501c727faeadd8db51e8abe53614a7561b6c9218bb5ec660",
	"execute||cisco":                            "7f615ffc7a11e7b58df466a53163231f97be87e8d1778112fdfafb5ae919e2b2",
	"execute||linux":                            "d0efc681bf96176f994fb488b91cc0b644311b522e8de0417ede9d308be2bf49",
	"execute||storage":                          "5dbf763ba66ec4ecd0f3c354a0e718e763fc4b3563570b42e3e2ef258974cc49",
	"execute|FAST_AGENT|":                       "07d86f59f0d27e342648a227358aa221513c25c20e0473c24dcf51d47529b31b",
	"execute|FAST_AGENT|kubernetes":             "07d86f59f0d27e342648a227358aa221513c25c20e0473c24dcf51d47529b31b",
	"execute|FAST_AGENT|proxmox":                "07d86f59f0d27e342648a227358aa221513c25c20e0473c24dcf51d47529b31b",
	"execute|FAST_AGENT|cisco":                  "07d86f59f0d27e342648a227358aa221513c25c20e0473c24dcf51d47529b31b",
	"execute|FAST_AGENT|linux":                  "07d86f59f0d27e342648a227358aa221513c25c20e0473c24dcf51d47529b31b",
	"execute|FAST_AGENT|storage":                "07d86f59f0d27e342648a227358aa221513c25c20e0473c24dcf51d47529b31b",
	"execute|STANDARD_AGENT|":                   "fe767169cf6ee1b6a767d92b2b1f90012e5f59ddad5d099ca64509cf520d0078",
	"execute|STANDARD_AGENT|kubernetes":         "f803936c9c1bc3d5d4755582c746a02416262d0a94b753b7a7381821aec2b77b",
	"execute|STANDARD_AGENT|proxmox":            "fd123eb02a1e5251501c727faeadd8db51e8abe53614a7561b6c9218bb5ec660",
	"execute|STANDARD_AGENT|cisco":              "7f615ffc7a11e7b58df466a53163231f97be87e8d1778112fdfafb5ae919e2b2",
	"execute|STANDARD_AGENT|linux":              "d0efc681bf96176f994fb488b91cc0b644311b522e8de0417ede9d308be2bf49",
	"execute|STANDARD_AGENT|storage":            "5dbf763ba66ec4ecd0f3c354a0e718e763fc4b3563570b42e3e2ef258974cc49",
	"execute|DEEP_INVESTIGATION|":               "fe767169cf6ee1b6a767d92b2b1f90012e5f59ddad5d099ca64509cf520d0078",
	"execute|DEEP_INVESTIGATION|kubernetes":     "f803936c9c1bc3d5d4755582c746a02416262d0a94b753b7a7381821aec2b77b",
	"execute|DEEP_INVESTIGATION|proxmox":        "fd123eb02a1e5251501c727faeadd8db51e8abe53614a7561b6c9218bb5ec660",
	"execute|DEEP_INVESTIGATION|cisco":          "7f615ffc7a11e7b58df466a53163231f97be87e8d1778112fdfafb5ae919e2b2",
	"execute|DEEP_INVESTIGATION|linux":          "d0efc681bf96176f994fb488b91cc0b644311b522e8de0417ede9d308be2bf49",
	"execute|DEEP_INVESTIGATION|storage":        "5dbf763ba66ec4ecd0f3c354a0e718e763fc4b3563570b42e3e2ef258974cc49",
}

func sum(b string) string { h := sha256.Sum256([]byte(b)); return hex.EncodeToString(h[:]) }

// TestGoldenGenerate prints the golden maps (GEN_GOLDENS=1) — used once at extraction; kept so a
// DELIBERATE body change regenerates them in the same reviewed diff that changes a seed.
func TestGoldenGenerate(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	if len(bodyGoldens) != 0 {
		return // goldens are filled in; generation is only for the initial capture / a deliberate refresh
	}
	for _, s := range Default().All() {
		fmt.Printf("\t%q: %q,\n", s.Name, sum(s.Body))
	}
	fmt.Println("--- compose ---")
	for _, tr := range composeTriples() {
		body, _ := Default().Compose(tr.ctx)
		fmt.Printf("\t%q: %q,\n", tr.key, sum(body))
	}
	t.Fatal("goldens are empty — paste the printed maps into bodyGoldens/composeGoldens")
}

type triple struct {
	key string
	ctx Context
}

func composeTriples() []triple {
	var out []triple
	for _, ph := range []Phase{PhaseInvestigate, PhaseExecute} {
		for _, ec := range []execclass.Class{"", execclass.FastAgent, execclass.StandardAgent, execclass.DeepInvestigation} {
			for _, d := range []Domain{DomainUnknown, DomainKubernetes, DomainProxmox, DomainCisco, DomainLinux, DomainStorage} {
				out = append(out, triple{key: fmt.Sprintf("%s|%s|%s", ph, ec, d), ctx: Context{Phase: ph, ExecClass: ec, Domain: d}})
			}
		}
	}
	return out
}

func TestDefaultRegistryGolden(t *testing.T) {
	reg := Default()
	if len(bodyGoldens) == 0 {
		t.Fatal("bodyGoldens is empty — run TestGoldenGenerate and paste")
	}
	seen := map[string]bool{}
	for _, s := range reg.All() {
		seen[s.Name] = true
		want, ok := bodyGoldens[s.Name]
		if !ok {
			t.Errorf("skill %q is not in the golden roster — a new skill needs a reviewed golden", s.Name)
			continue
		}
		if got := sum(s.Body); got != want {
			t.Errorf("skill %q body drifted from the golden (the byte-identity the eval waiver rests on)", s.Name)
		}
	}
	for name := range bodyGoldens {
		if !seen[name] {
			t.Errorf("golden skill %q missing from the registry — a silently thinner library", name)
		}
	}
	for _, tr := range composeTriples() {
		body, _ := reg.Compose(tr.ctx)
		if got, want := sum(body), composeGoldens[tr.key]; got != want {
			t.Errorf("Compose(%s) drifted from the golden (selection/order/separator changed)", tr.key)
		}
	}
}
