package main

// ORACLES FOR THE PLANE-SCOPED DATABASE IDENTITY (TG-164).
//
// The GRANTS are proven against a real Postgres in core/db/plane_roles_test.go. What is proven HERE is the
// half Postgres cannot enforce: which DSN each process picks up. That is the weak link by construction — an
// operator editing a .env — and its failure mode is the one this ticket names as the worst available: not a
// boot failure, a permission error deep inside an activity hours later.
//
// Deliberately NOT DSN-gated: these are pure resolution rules and must run in `make all` on a box with no
// database, alongside the TG-153 plane oracles they extend.

import (
	"sort"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
)

// envMap turns a fixed map into the getter planeDBDSN takes, so the rules are testable without mutating the
// process environment (which is also why the one real os.Getenv call lives in a separate one-line wrapper).
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const (
	sharedDSN  = "postgres://tg_runtime:x@postgres:5432/grounder"
	triageDSN  = "postgres://tg_triage:x@postgres:5432/grounder"
	actuateDSN = "postgres://tg_actuate:x@postgres:5432/grounder"
)

// THE UPGRADE-PATH ORACLE, and the one a reviewer should read first. An existing deployment defines exactly
// one DSN and no plane roles; every plane must keep using it, unchanged.
//
// KILLING MUTATION: make planeDBDSN return "" (or the plane key) when the plane key is unset. RED — "a
// deployment that has not opted in loses its durable stores entirely", which is a silent downgrade to
// in-memory predictions and ledger.
func TestWithoutAPlaneDSNEveryPlaneKeepsTheSharedOne(t *testing.T) {
	env := envMap(map[string]string{"TG_DB_DSN": sharedDSN})
	for _, plane := range []credential.ProcessPlane{
		credential.ProcessPlaneBoth, credential.ProcessPlaneTriage, credential.ProcessPlaneActuation,
	} {
		got, why := planeDBDSN(plane, env)
		if got != sharedDSN {
			t.Errorf("plane=%s resolved %q, want the shared TG_DB_DSN %q — an un-opted-in deployment must be "+
				"byte-identical after this change", plane, got, sharedDSN)
		}
		if why == "" {
			t.Errorf("plane=%s produced an empty provenance string — the boot log would not say which key it "+
				"read, which is how a split that is believed but not in effect survives", plane)
		}
	}
}

// Each split plane picks up ITS OWN role, and never the other's. The cross-check is the point: docker-compose
// hands every service the same .env, so both keys are present in both processes and the resolver — not the
// deployment — is what keeps them apart.
//
// KILLING MUTATION: swap the two cases in planeDBDSN's switch. RED — "the actuation worker authenticated as
// tg_triage", i.e. the process holding the estate-mutating key loses the right to record what it did while
// gaining the right to author the evidence it acts on.
func TestASplitPlanePicksUpItsOwnRoleAndNeverTheOthers(t *testing.T) {
	env := envMap(map[string]string{
		"TG_DB_DSN":         sharedDSN,
		"TG_DB_DSN_TRIAGE":  triageDSN,
		"TG_DB_DSN_ACTUATE": actuateDSN,
	})
	for _, tc := range []struct {
		plane credential.ProcessPlane
		want  string
	}{
		{credential.ProcessPlaneTriage, triageDSN},
		{credential.ProcessPlaneActuation, actuateDSN},
	} {
		got, why := planeDBDSN(tc.plane, env)
		if got != tc.want {
			t.Errorf("plane=%s resolved %q, want %q (why=%q)", tc.plane, got, tc.want, why)
		}
	}
}

// A `both` process serves BOTH queues in one address space, so it needs the union of both authorities —
// which is exactly tg_runtime. Handing it a plane role would make it fail at its first off-plane write.
//
// The IGNORED keys must be named in the provenance string. A compose .env that defines them reaches every
// service, so this is the normal case on a split deployment's un-split sibling, and silence here is how an
// operator concludes the database is split when it is not.
//
// KILLING MUTATION: return the triage DSN under `both` when TG_DB_DSN_TRIAGE is set. RED — the both-plane
// worker would then run the actuation queue on a role denied every actuation write.
func TestBothPlaneUsesTheSharedRoleAndSaysSoWhenPlaneKeysAreSet(t *testing.T) {
	env := envMap(map[string]string{
		"TG_DB_DSN":         sharedDSN,
		"TG_DB_DSN_TRIAGE":  triageDSN,
		"TG_DB_DSN_ACTUATE": actuateDSN,
	})
	got, why := planeDBDSN(credential.ProcessPlaneBoth, env)
	if got != sharedDSN {
		t.Fatalf("plane=both resolved %q, want the shared %q — a both-plane process holds both queues and "+
			"needs the union of both authorities", got, sharedDSN)
	}
	for _, key := range []string{TriageDBDSNEnv, ActuateDBDSNEnv} {
		if !strings.Contains(why, key) {
			t.Errorf("the boot provenance %q does not name the IGNORED key %s — an operator who set it would "+
				"read a clean boot and believe the database planes were split", why, key)
		}
	}
	if !strings.Contains(strings.ToUpper(why), "IGNORED") {
		t.Errorf("the boot provenance %q does not say the plane keys were IGNORED", why)
	}
}

// The withheld-table set must follow the plane, and `both` must withhold NOTHING — because a both-plane
// process has no off-plane half. The last clause is why the worker branches on len()==0 rather than printing
// an empty list: "checked nothing, found nothing" must never read as a clean bill of health.
//
// KILLING MUTATION: return db.ActuationAuthorityTables for every plane. RED on the `both` case — the boot
// self-check would report a LIVE EXPOSURE on every existing deployment, which trains operators to ignore it.
func TestWithheldTablesFollowThePlane(t *testing.T) {
	for _, tc := range []struct {
		plane credential.ProcessPlane
		want  []string
	}{
		{credential.ProcessPlaneTriage, db.ActuationAuthorityTables},
		{credential.ProcessPlaneActuation, db.TriageContentTables},
		{credential.ProcessPlaneBoth, nil},
	} {
		got := planeWithheldTables(tc.plane)
		if !sameSet(got, tc.want) {
			t.Errorf("plane=%s withholds %v, want %v", tc.plane, got, tc.want)
		}
	}
	// Vacuity floor for the two non-empty cases: an empty list would satisfy "follows the plane" while
	// withholding nothing at all.
	if len(planeWithheldTables(credential.ProcessPlaneTriage)) == 0 ||
		len(planeWithheldTables(credential.ProcessPlaneActuation)) == 0 {
		t.Fatal("a split plane withholds ZERO tables — the boot self-check would examine nothing and report " +
			"a split that does not exist")
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// The plane DSN keys must be structurally unreachable from the console's config store. The console writes
// through tg_runtime — the very identity the split exists to stop being universal — so a stored override on
// these keys would let it choose which identity the split worker authenticates as.
//
// KILLING MUTATION: remove either key from bootConfigForbiddenEnvKeys. RED.
func TestThePlaneDSNKeysCannotBeServedFromTheDatabase(t *testing.T) {
	for _, key := range []string{TriageDBDSNEnv, ActuateDBDSNEnv} {
		if !bootConfigForbiddenEnvKeys[key] {
			t.Errorf("%s is not in bootConfigForbiddenEnvKeys — a module descriptor could bind it and the "+
				"config plane (which writes as tg_runtime) would choose the split worker's database identity", key)
		}
	}
	// Vacuity floor: the map must actually be the guard, not an empty map that passes everything.
	if !bootConfigForbiddenEnvKeys["TG_DB_DSN"] {
		t.Fatal("bootConfigForbiddenEnvKeys does not even contain TG_DB_DSN — the assertions above are vacuous")
	}
}
