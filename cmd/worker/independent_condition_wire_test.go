package main

import (
	"strings"
	"testing"
)

func TestParseResourceBindingsNormalizesAUnitAndAcceptsHTTPS(t *testing.T) {
	b := parseResourceBindings("host01|unit|nginx.service|https://host01:443/|restart-service")
	if len(b["host01"]) != 1 || b["host01"][0].Name != "nginx" || b["host01"][0].Probe != "https://host01:443/" {
		t.Fatalf("parsed = %+v", b)
	}
}

func TestParseResourceBindings(t *testing.T) {
	if b := parseResourceBindings(""); b == nil || len(b) != 0 {
		t.Fatalf("an empty list must authorize nothing, got %+v", b)
	}
	b := parseResourceBindings("host01|unit|nginx|tcp://192.0.2.1:80|start-service; host02.example.net|container|app|http://192.0.2.2:3000/|restart-container, start-container")
	if len(b["host01"]) != 1 || b["host01"][0].Name != "nginx" || b["host01"][0].Probe != "tcp://192.0.2.1:80" || len(b["host01"][0].Ops) != 1 || len(b["host02"][0].Ops) != 2 || b["host02"][0].Kind != "container" || b["host02"][0].Ops[0] != "restart-container" {
		t.Fatalf("parsed = %+v", b)
	}
	for _, bad := range []string{"host01|unit|x|tcp://192.0.2.1:80|restart-service", "host01|unit|x|tcp://127.0.0.1:80|start-service", "host01|unit|x|http://otherhost:80/|start-service", "host01|unit|nginx", "host01|unit|nginx|tcp://host01:1", "host01|mode|x|tcp://host01:1|o", "host01|unit||tcp://host01:1|o", "host01|unit|nginx|ftp://host01:1|o", "host01|unit|nginx|tcp://host01:1|o;oops"} {
		if m := parseResourceBindings(bad); m == nil || len(m) != 0 {
			t.Fatalf("%q must deny everything, got %+v", bad, m)
		}
	}
}

func TestABindingOnAProtectedObjectRefusesEveryBinding(t *testing.T) {
	good := "host01|unit|nginx|http://host01:80/|start-service"
	for name, bad := range map[string]string{
		"never-touch unit":      "host01|unit|docker|http://host01:80/|start-service",
		"never-touch, suffixed": "host01|unit|sshd.service|http://host01:22/|start-service",
		"never-touch prefix":    "host01|unit|pvedaemon|http://host01:80/|start-service",
		"unit type":             "host01|unit|docker.socket|http://host01:80/|start-service",
		"stateful container":    "host01|container|postgres|http://host01:80/|start-container",
		"stateful host":         "app-db|unit|nginx|http://app-db:80/|start-service",
	} {
		if m := parseResourceBindings(good + ";" + bad); len(m) != 0 {
			t.Errorf("%s: bindings loaded: %v", name, m)
		}
	}
	if m := parseResourceBindings(good); len(m) != 1 {
		t.Fatalf("an ordinary binding must load: %v", m)
	}
}

func TestAMistypedConditionSettingStillEnforces(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "  ": false, "off": false, "OFF": false, "enforce": true, "Enforce": true, " ENFORCE ": true, "enforced": true, "yes": true} {
		if got := conditionEnforced(raw); got != want {
			t.Errorf("conditionEnforced(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestTheBootLineNamesEveryBindingAndAnEmptyList(t *testing.T) {
	if got := bindingSummary(parseResourceBindings("")); !strings.HasPrefix(got, "0 service or container bindings") {
		t.Fatalf("empty: %q", got)
	}
	got := bindingSummary(parseResourceBindings("host01|unit|nginx|http://host01:80/|start-service,restart-service;host02|container|app|http://host02:80/|start-container"))
	for _, want := range []string{"2 service or container binding(s)", "host01/unit/nginx[start-service,restart-service]", "host02/container/app[start-container]"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not name %q", got, want)
		}
	}
}
