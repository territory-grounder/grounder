package deploy

// THE SEAL-KEY IDENTITY GUARD (TG-275).
//
// TestComposeEnvParity next door proves each service RECEIVES the variables its binary reads. That is not
// enough for the seal key, because presence is not agreement: a worker configured with a different master
// key than the grounder passes parity perfectly and then cannot open one single credential the grounder
// wrote. Nothing surfaces at boot — both processes log a healthy sealer — and the damage is discovered
// later as "the AWX sync says the secret is corrupt", by which time secrets have been written under two
// keys and neither process can read all of them.
//
// So this asserts the stronger property: the two services resolve the seal configuration from the SAME
// expressions, character for character.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sealKeys are the variables that decide WHICH key seals and unseals. A divergence in any one of them
// splits the two processes' view of the secret store.
var sealKeys = []string{
	"TG_SEAL_KEY",
	"TG_SEAL_KEY_REF",
	"TG_SEAL_TRANSIT_KEY",
	"TG_SEAL_TRANSIT_ADDR",
	"TG_SEAL_TRANSIT_TOKEN_REF",
	"TG_SEAL_TRANSIT_MOUNT",
}

// composeEnv reads one service's `environment:` block. It walks a generic tree rather than unmarshalling
// into a typed struct, because compose lets OTHER services declare `environment:` as a LIST — a typed
// map field makes the whole document fail to parse on a service this guard does not even look at.
func composeEnv(t *testing.T, service string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	services, _ := doc["services"].(map[string]any)
	svc, ok := services[service].(map[string]any)
	if !ok {
		t.Fatalf("service %q absent from compose — this guard is scanning a file that no longer describes "+
			"the deployment", service)
	}
	out := map[string]string{}
	switch env := svc["environment"].(type) {
	case map[string]any:
		for k, v := range env {
			out[k] = fmt.Sprint(v)
		}
	case []any: // list form: "KEY=value"
		for _, item := range env {
			kv := fmt.Sprint(item)
			if i := strings.Index(kv, "="); i > 0 {
				out[kv[:i]] = kv[i+1:]
			}
		}
	default:
		t.Fatalf("service %q has no readable environment block (%T) — the guard cannot see the seal "+
			"configuration and must not pretend it checked", service, svc["environment"])
	}
	return out
}

// KILLING MUTATION: drop the TG_SEAL_* block from the worker service (the shipped state), or change any
// one of its values. RED either way.
func TestWorkerAndGrounderSealFromTheSameKey(t *testing.T) {
	worker, grounder := composeEnv(t, "worker"), composeEnv(t, "grounder")
	for _, k := range sealKeys {
		g, inG := grounder[k]
		w, inW := worker[k]
		if !inG {
			t.Errorf("%s is absent from the GROUNDER — it is the process that writes sealed secrets", k)
			continue
		}
		if !inW {
			t.Errorf("%s is absent from the WORKER. The worker is what OPENS sealed credentials "+
				"(hostdiag, syslog-ng, actuation, AWX sync); without this it resolves no `store:` ref at "+
				"all, which is why sealed_secret held zero rows.", k)
			continue
		}
		if g != w {
			t.Errorf("%s DIVERGES: grounder=%v worker=%v. The two processes would seal and unseal under "+
				"different keys — every credential written by one becomes unreadable by the other, and "+
				"neither logs anything wrong.", k, g, w)
		}
	}
}

// VACUITY FLOOR. If the compose is reshaped and the env blocks stop being maps, the loop above compares
// nothing and reports green.
func TestTheSealParityScanActuallyComparedSomething(t *testing.T) {
	worker := composeEnv(t, "worker")
	found := 0
	for _, k := range sealKeys {
		if _, ok := worker[k]; ok {
			found++
		}
	}
	if found != len(sealKeys) {
		t.Fatalf("matched %d of %d seal variables in the worker service — the guard above is comparing an "+
			"empty set and would pass on any configuration at all", found, len(sealKeys))
	}
}
