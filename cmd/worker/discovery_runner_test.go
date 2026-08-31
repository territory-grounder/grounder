package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

// An empty allowlist must yield NIL, not a runner that refuses everything.
//
// The distinction is the whole reason this returns a pointer: a discovery source built over a
// refuse-everything runner contributes zero edges, and downstream that is indistinguishable from a
// genuinely serviceless estate. worlddiscovery would then diff an empty observation against the manifest
// and mark approved entries stale — the exact failure the lane's own no-sources guard exists to prevent.
func TestDiscoveryRunnerIsNilWithoutAnAllowlist(t *testing.T) {
	if r := newDiscoveryRunner(nil, nil, "", 0); r != nil {
		t.Fatal("an empty host list produced a runner; a source built over it would report 'these hosts " +
			"run nothing' rather than 'nothing was observed'")
	}
	if r := newDiscoveryRunner([]string{"  ", ""}, nil, "", 0); r != nil {
		t.Fatal("a host list of blanks produced a runner")
	}
	// A resolver is equally mandatory: without one every read fails closed, which is the same lie.
	if r := newDiscoveryRunner([]string{"web01"}, nil, "/dev/null", 0); r != nil {
		t.Fatal("a runner was built with no credential resolver — every read would fail closed while the " +
			"source reported itself armed")
	}
}

// A host outside the operator allowlist must be REFUSED, and refused before any credential is resolved or
// any connection is attempted. Discovery reads a real machine; the allowlist is the boundary.
func TestDiscoveryRunnerRefusesUnallowlistedAndMalformedHosts(t *testing.T) {
	d := &discoveryRunner{allow: map[string]bool{"web01": true}}

	// ASSERT WHICH REFUSAL, not merely that something failed. A first draft asserted only `err != nil`,
	// and removing the allowlist check entirely LEFT IT GREEN: an un-allowlisted host then failed one step
	// later on the nil resolver, which satisfies "an error occurred" while the boundary is gone. That is
	// the same vacuity this repo keeps paying for — a control that cannot fail when the thing it guards
	// breaks is not a control.
	for _, tc := range []struct{ host, wantErr string }{
		// Well-formed but NOT GRANTED — must be stopped by the allowlist itself.
		{"web02", "not in the operator discovery allowlist"},
		{"WEB02", "not in the operator discovery allowlist"},
		// Malformed — must be stopped earlier still, by the label shape.
		{"../etc/passwd", "not a valid host label"},
		{"-oProxyCommand=x", "not a valid host label"},
		{"web01;rm -rf /", "not a valid host label"},
		{"web01 web02", "not a valid host label"},
		{"", "not a valid host label"},
	} {
		_, err := d.Run(context.Background(), tc.host, []string{"systemctl", "list-units"})
		if err == nil {
			t.Errorf("host %q was accepted by the discovery runner", tc.host)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("host %q was refused for the WRONG reason: got %q, want it to contain %q — a refusal "+
				"that happens by accident downstream is not a boundary", tc.host, err, tc.wantErr)
		}
	}
	// The allowlisted host gets past the gate and fails later (no resolver), which proves the gate is not
	// simply refusing everything — a control that refuses all inputs would pass the loop above vacuously.
	_, err := d.Run(context.Background(), "web01", []string{"systemctl", "list-units"})
	if err == nil {
		t.Fatal("expected a later failure for the allowlisted host (no resolver wired)")
	}
	if strings.Contains(err.Error(), "not in the operator discovery allowlist") {
		t.Fatal("the allowlisted host was refused BY THE ALLOWLIST — the gate refuses everything, so the " +
			"refusal assertions above prove nothing")
	}
	// An empty argv is refused: this adapter never constructs a command, and an empty one could only mean
	// a caller lost the package constant it was supposed to pass through.
	if _, err := d.Run(context.Background(), "web01", nil); err == nil {
		t.Fatal("an empty argv was accepted")
	}
}

// Host order must not depend on Go's map iteration, or a source's edges reshuffle between boots.
func TestDiscoveryRunnerHostListIsDeterministic(t *testing.T) {
	d := &discoveryRunner{allow: map[string]bool{"zeta": true, "alpha": true, "mike": true}}
	want := "alpha,mike,zeta"
	for i := 0; i < 8; i++ {
		if got := strings.Join(d.hostList(), ","); got != want {
			t.Fatalf("hostList() = %q, want %q", got, want)
		}
	}
	// Hosts are lower-cased on entry, so an operator writing WEB01 and web01 does not get two probes.
	r := newDiscoveryRunner([]string{"WEB01", "web01", "Web01"}, &dummyResolverHolder, "/dev/null", 0)
	if r == nil {
		t.Fatal("a valid host list produced no runner")
	}
	if got := r.hostList(); len(got) != 1 || got[0] != "web01" {
		t.Fatalf("case variants were not folded to one host: %v", got)
	}
}

// THE REACHABILITY GUARD. modules/discovery/{systemd,docker} were linked into NO BINARY: they are the only
// producers of estate.TypeService, and core/worldmodel routes exclusively TypeService to KindUnit and
// KindContainer, so two of the three adoption kinds were unreachable by construction — while the
// world.discovery seam reported LIVE and the boot log announced the lane as armed.
//
// A unit test on the probe packages cannot catch that; they were fully tested the entire time. What has to
// be pinned is that the COMPOSITION ROOT still references them.
//
// KILLING MUTATION: delete the systemddisc/dockerdisc imports and their construction from main.go. RED.
func TestDiscoveryProbesAreReferencedByTheCompositionRoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	imported := map[string]bool{}
	for _, im := range file.Imports {
		path := strings.Trim(im.Path.Value, `"`)
		if strings.Contains(path, "modules/discovery/") {
			imported[path] = true
		}
	}
	for _, want := range []string{
		"github.com/territory-grounder/grounder/modules/discovery/systemd",
		"github.com/territory-grounder/grounder/modules/discovery/docker",
	} {
		if !imported[want] {
			t.Errorf("cmd/worker/main.go does not import %s — it is the ONLY producer of estate.TypeService "+
				"for its kind, so the world model can never draft that adoption kind, and nothing says so "+
				"because the world.discovery seam reports on the LANE rather than on this half of it", want)
		}
	}

	// Imported is not the same as CALLED. Both constructors must actually be invoked, and the result must
	// reach the estate source set the discovery pass reads.
	calls := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// BOTH shapes, deliberately. A first draft matched only *ast.SelectorExpr, so the local
		// newDiscoveryRunner — a bare *ast.Ident — was invisible and the guard reported it uncalled while
		// it was called. That is the same control-mutation this repo has paid for before
		// (cmd/worker/axis_wiring_test.go:28): a walk that matches one node kind certifies nothing about
		// the others.
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok {
				calls[x.Name+"."+fn.Sel.Name] = true
			}
		case *ast.Ident:
			calls[fn.Name] = true
		}
		return true
	})
	for _, want := range []string{"systemddisc.New", "dockerdisc.New"} {
		if !calls[want] {
			t.Errorf("%s is imported but never called — the package is linked and still produces nothing", want)
		}
	}
	if !calls["newDiscoveryRunner"] {
		t.Error("newDiscoveryRunner is never called, so neither probe can have a transport")
	}
	// Vacuity floor: if the walk found no calls at all the assertions above are meaningless.
	if len(calls) < 10 {
		t.Fatalf("vacuity floor: the AST walk found only %d call sites in main.go — the matcher is broken "+
			"and a passing run would certify nothing", len(calls))
	}
}

// dummyResolverHolder is a non-nil *credential.AuditedResolver used only to get past the nil check in
// newDiscoveryRunner. It is never invoked: these tests assert construction and gating, never a connection.
var dummyResolverHolder = *credential.NewAuditedResolver(nil)
