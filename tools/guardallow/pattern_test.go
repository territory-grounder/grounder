package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/actuation/ssh"
)

// TG-626: the REAL host guard script, driven with the REAL rendered pattern file. Fake systemctl/docker echo
// what they were asked so a test can prove what actually ran; the guard's refusal is exit 42.
// guardShells are the /bin/sh implementations the guard meets on this estate's hosts (Debian/Ubuntu dash, plus
// whatever sh this box has). Each available one runs every case.
func guardShells() []string {
	var out []string
	for _, s := range []string{"sh", "dash", "bash"} {
		if _, err := exec.LookPath(s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

type guardRun struct {
	shell, hostname string
	patternLines    []string // nil ⇒ RenderPattern()
}

func runGuard(t *testing.T, cmd string, allowLines []string, withPattern bool) (string, int) {
	t.Helper()
	return runGuardWith(t, guardRun{shell: "sh", hostname: "dc1gpu01"}, cmd, allowLines, withPattern)
}

func runGuardWith(t *testing.T, g guardRun, cmd string, allowLines []string, withPattern bool) (string, int) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"systemctl", "docker"} {
		if err := os.WriteFile(filepath.Join(bin, b), []byte("#!/bin/sh\necho \"RAN "+b+" $*\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "hostname"), []byte("#!/bin/sh\necho "+g.hostname+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	allow := filepath.Join(dir, "allow")
	if err := os.WriteFile(allow, []byte(strings.Join(allowLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pattern := filepath.Join(dir, "pattern")
	if withPattern {
		lines := g.patternLines
		if lines == nil {
			lines = RenderPattern()
		}
		if err := os.WriteFile(pattern, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := exec.Command(g.shell, "../../deploy/actuation-guard/tg-actuator-guard")
	c.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"SSH_ORIGINAL_COMMAND=" + cmd,
		"TG_ACTUATOR_ALLOWFILE=" + allow,
		"TG_ACTUATOR_PATTERNFILE=" + pattern,
	}
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run guard: %v", err)
	}
	return string(out), code
}

func q(argv ...string) string { return ssh.RemoteCommand(argv) }

func TestPatternGuardAdmitsTheFollowEstateScope(t *testing.T) {
	for _, argv := range [][]string{
		{"systemctl", "restart", "uvicorn"},
		{"systemctl", "start", "ollama.service"},
		{"systemctl", "stop", "uvicorn"}, // start-service's rollback
		{"systemctl", "is-active", "uvicorn"},
		{"docker", "restart", "openwebui"},
	} {
		out, code := runGuard(t, q(argv...), nil, true)
		if code != 0 || !strings.Contains(out, "RAN "+strings.Join(argv, " ")) {
			t.Errorf("%v must run under pattern mode with argv intact; exit=%d out=%q", argv, code, out)
		}
	}
}

func TestPatternGuardRefusals(t *testing.T) {
	cases := map[string]string{
		"never-touch sshd":       q("systemctl", "restart", "sshd"),
		"never-touch systemd-*":  q("systemctl", "restart", "systemd-networkd"),
		"never-touch docker":     q("systemctl", "restart", "docker.service"),
		"never-touch pve":        q("systemctl", "restart", "pveproxy"),
		"stateful postgres":      q("systemctl", "restart", "postgresql"),
		"stateful container":     q("docker", "restart", "app-db"),
		"case-folded deny":       q("systemctl", "restart", "SSHD"),
		"unknown verb":           q("systemctl", "disable", "uvicorn"),
		"unknown binary":         q("rm", "-rf", "x"),
		"extra token":            q("systemctl", "restart", "uvicorn", "--force"),
		"interactive shell":      "",
		"unquoted":               "systemctl restart uvicorn",
		"command substitution":   "'systemctl' 'restart' '$(id)'",
		"quote break-out":        "'systemctl' 'restart' 'a' ; 'id'",
		"newline smuggle":        q("systemctl", "restart", "uvicorn") + "\nid",
		"space in subject":       "'systemctl' 'restart' 'a b'",
		"leading dash container": "'docker' 'restart' '-rm'",
	}
	for name, cmd := range cases {
		out, code := runGuard(t, cmd, nil, true)
		if code != 42 || strings.Contains(out, "RAN") {
			t.Errorf("%s: must be refused with exit 42 and nothing run; exit=%d out=%q", name, code, out)
		}
	}
}

func TestNoPatternFileIsByteIdenticalListMode(t *testing.T) {
	cmd := q("systemctl", "restart", "uvicorn")
	if _, code := runGuard(t, cmd, nil, false); code != 42 {
		t.Fatalf("without a pattern file an unlisted command must still be refused, exit=%d", code)
	}
	listed := q("systemctl", "restart", "nginx")
	if out, code := runGuard(t, listed, []string{listed}, false); code != 0 || !strings.Contains(out, "RAN systemctl restart nginx") {
		t.Fatalf("an exactly listed command must still run in list mode, exit=%d out=%q", code, out)
	}
}

// Review findings (TG-626 security pass), each pinned under every available shell.
func TestPatternGuardHardening(t *testing.T) {
	allowLine := RenderPattern()[0]
	for _, sh := range guardShells() {
		std := guardRun{shell: sh, hostname: "dc1gpu01"}
		// 1. A truncated/corrupt DENY regex must DENY (grep exit 2 used to read as "not denied").
		broken := guardRun{shell: sh, hostname: "dc1gpu01", patternLines: []string{allowLine, "DENY ^(ssh|sshd"}}
		if out, code := runGuardWith(t, broken, q("systemctl", "restart", "sshd"), nil, true); code != 42 || strings.Contains(out, "RAN") {
			t.Errorf("[%s] a corrupt DENY must fail CLOSED; exit=%d out=%q", sh, code, out)
		}
		if out, code := runGuardWith(t, broken, q("systemctl", "restart", "uvicorn"), nil, true); code != 42 || strings.Contains(out, "RAN") {
			t.Errorf("[%s] a corrupt DENY must refuse even a benign subject; exit=%d out=%q", sh, code, out)
		}
		// 2. Option injection and root units via a leading '-'.
		for _, subj := range []string{"--all", "-Hroot@evil", "-.mount", "-.slice"} {
			if out, code := runGuardWith(t, std, q("systemctl", "stop", subj), nil, true); code != 42 || strings.Contains(out, "RAN") {
				t.Errorf("[%s] leading-dash subject %q must be refused; exit=%d out=%q", sh, subj, code, out)
			}
		}
		// 3. The stateful-HOST rule: on a stateful host nothing passes pattern mode.
		statefulHost := guardRun{shell: sh, hostname: "dc1cl01mariadb01"}
		if out, code := runGuardWith(t, statefulHost, q("systemctl", "restart", "uvicorn"), nil, true); code != 42 || strings.Contains(out, "RAN") {
			t.Errorf("[%s] pattern mode must refuse everything on a stateful host; exit=%d out=%q", sh, code, out)
		}
		// 4. Deny-list gaps the review named.
		for _, argv := range [][]string{
			{"systemctl", "restart", "nftables"}, {"systemctl", "restart", "messagebus"},
			{"systemctl", "restart", "ssh.socket"}, {"systemctl", "restart", "sshd@0-1.2.3.4:22"},
			{"systemctl", "restart", "multi-user.target"}, {"systemctl", "restart", "home.mount"},
			{"systemctl", "restart", "ceph-osd@1"}, {"docker", "restart", "portainer"},
		} {
			if out, code := runGuardWith(t, std, q(argv...), nil, true); code != 42 || strings.Contains(out, "RAN") {
				t.Errorf("[%s] %v must be refused; exit=%d out=%q", sh, argv, code, out)
			}
		}
		// And the scope still works under this shell.
		if out, code := runGuardWith(t, std, q("systemctl", "restart", "uvicorn"), nil, true); code != 0 || !strings.Contains(out, "RAN systemctl restart uvicorn") {
			t.Errorf("[%s] the follow-estate scope must still run; exit=%d out=%q", sh, code, out)
		}
	}
}
