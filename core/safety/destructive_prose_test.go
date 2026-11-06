package safety

import (
	"testing"
)

// TG-641: a rationale is prose. A sentence ABOUT a shutdown is not a shutdown command; a sentence that spells
// out a destructive command still is.
func TestIsDestructiveProse(t *testing.T) {
	notCommands := []string{
		"The eventlog contains no evidence of a planned shutdown or disabling.",
		"The guest halted unexpectedly and has not powered back on.",
		"No power-off was scheduled; the certificate was not revoked.",
		"An unexpected shutdown of the container stopped the app.",
	}
	for _, s := range notCommands {
		if IsDestructiveProse(s) {
			t.Errorf("prose about an event read as a destructive command: %q", s)
		}
	}
	commands := []string{
		"I will run shutdown -h now on the host.",
		"then shutdown +5 to drain",
		"systemctl poweroff",
		"qm shutdown 101 first",
		"vault token revoke the lease",
		"run dropdb prod and recreate",
		"rm -rf /var/lib/app",
		"certbot revoke --cert-name x",
		"qm destroy 101",
	}
	for _, s := range commands {
		if !IsDestructiveProse(s) {
			t.Errorf("a destructive command in prose was missed: %q", s)
		}
	}
}

// The ACTION keeps the full pattern: a bare host-power or revoke verb in the command is still destructive.
func TestIsDestructiveOpStillMatchesBareVerbsInTheAction(t *testing.T) {
	for _, op := range []string{"shutdown", "halt", "poweroff", "revoke"} {
		if !IsDestructiveOp(op) {
			t.Errorf("the action %q must stay destructive", op)
		}
	}
}

// SEMANTIC parity: every destructive command the ACTION pattern catches (other than the four bare words) is
// caught in prose too. The init-time panic in proseVariant guards the shape; this guards the meaning.
func TestProseCatchesEveryActionCommandExceptTheBareWords(t *testing.T) {
	for _, c := range []string{
		"mkfs.ext4 /dev/sdb1", "wipefs -a /dev/sdb", "dd if=/dev/zero of=/dev/sda", "lvremove vg/lv",
		"zpool destroy tank", "zfs rollback tank@x", "drop table users", "dropdb prod", "rm -rf /data",
		"rm /var/log/x", "terraform destroy", "qm destroy 101", "pct reset 101", "certbot revoke --cert-name x",
		"kubectl delete pvc data", "helm uninstall app", "docker volume prune", "write erase",
		"nft flush ruleset", "git push --force origin main", "git reset --hard", "glab mr merge 12",
		"truncate -s 0 /var/log/app.log",
	} {
		if !IsDestructiveOp(c) {
			t.Fatalf("fixture %q is not destructive even as an action; fix the fixture", c)
		}
		if !IsDestructiveProse("I will run " + c + " now") {
			t.Errorf("the prose pattern lost a destructive command the action pattern catches: %q", c)
		}
	}
}

// The reviewer's escape cases (TG-641 review): command shapes that must stay caught in prose, and English
// that must not.
func TestDestructiveProseCommandShapes(t *testing.T) {
	for _, s := range []string{
		"run poweroff on the host", "shutdown --poweroff", "systemctl --now poweroff", "systemctl -f poweroff",
		"sudo halt", "pct shutdown ct101", "vault revoke secret/x", "REVOKE ALL ON db FROM bob",
		"shutdown -P now", "sudo shutdown now", "/sbin/shutdown now",
	} {
		if !IsDestructiveProse(s) {
			t.Errorf("command-shaped %q must stay destructive in prose", s)
		}
	}
	for _, s := range []string{
		"the init 0.5 value", "the rollout was halted", "the token was revoked yesterday",
		"no shutdown was planned", "power off was not requested",
	} {
		if IsDestructiveProse(s) {
			t.Errorf("English %q must not read as a destructive command", s)
		}
	}
}
