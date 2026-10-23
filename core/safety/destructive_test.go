package safety

import "testing"

func TestIsDestructiveOp(t *testing.T) {
	destructive := []string{
		"dropdb production", "DROP TABLE users", "truncate table sessions", "mkfs.ext4 /dev/sda1",
		"wipefs -a /dev/sdb", "rm -rf /var/data", "terraform destroy -auto-approve",
		"kubectl delete namespace prod", "docker system prune -af", "zpool destroy tank",
		"lvremove /dev/vg/lv", "shutdown -h now", "certbot revoke --cert-name x",
		// full spellings (not just the pv/pvc aliases) — a `delete persistentvolumeclaim` IS data loss and
		// must floor to POLL_PAUSE, matching the predecessor's k8s-delete-stateful pattern.
		"kubectl delete persistentvolumeclaim data-claim-01", "kubectl delete persistentvolume pv-data-01",
		"kubectl delete pvc web-0", "kubectl delete pv nfs-01",
		// Proxmox guest destroy (irreversible) and hard reset — the predecessor floors both.
		"qm destroy 102", "qm reset 101", "pct destroy 200",
		// helm teardown + kubectl apply --prune — destruction-equivalent k8s ops.
		"helm uninstall postgres", "helm rollback app 3", "kubectl apply --prune -f partial.yaml",
	}
	for _, s := range destructive {
		if !IsDestructiveOp(s) {
			t.Errorf("%q must be recognized as destructive", s)
		}
	}
	benign := []string{"systemctl restart nginx", "kubectl get pods", "docker image prune -f", "df -h", "certbot renew"}
	for _, s := range benign {
		if IsDestructiveOp(s) {
			t.Errorf("%q must NOT be flagged destructive", s)
		}
	}
	// the model under-declares: op_class "restart-service" but the op is dropdb → caught across parts
	if !IsDestructiveOp("restart-service", "dropdb prod") {
		t.Error("a destructive op hidden behind a benign op_class must be caught")
	}
}

// TG-224 (port-fidelity finding #17) — the two categories the server-side deriver did not know.
//
// EVERY case here is one alternation branch, so a dropped branch reds a named case rather than thinning a
// pattern silently. These verbs cannot execute today by construction (registry-only argv: an unregistered
// op-class has no builder and no template, so it never reaches an argv vector) — this is the deriver KNOWING
// them BEFORE the first network or deploy op-class is registered, which is the only order in which knowing
// them helps.
func TestIsDestructiveOpNetworkCatastrophic(t *testing.T) {
	// Ported from the predecessor's `irreversible:network-catastrophic` floor entry and the never-do list its
	// network estate maintains. Each one erases the config, kills the routing, or tears down the link that TG's
	// own management path runs over — there is no far side of the partition from which to undo it.
	for _, op := range []string{
		"write erase",                             // wipes the running config on a Cisco device
		"erase startup-config",                    // same, at next boot
		"erase nvram:",                            //
		"erase flash:",                            //
		"no ip routing",                           // kills L3 forwarding — total outage, remote lockout
		"default interface GigabitEthernet1/0/1",  // resets an interface to defaults, dropping its addressing
		"clear configure all",                     // ASA: full config wipe
		"clear config all",                        //
		"no ip route 10.0.0.0 255.0.0.0 192.0.2.1", // removes a route
		"no access-list OUTSIDE_IN",               // removes an ACL
		"no spanning-tree vlan 10",                // invites a broadcast storm
		"no switchport trunk allowed vlan 20",     // strips a trunk
		"ip link delete br0",                      // Linux bridge teardown
		"brctl delbr br0",                         //
		"brctl delif br0 eth1",                    //
		"ovs-vsctl del-br br-int",                 //
		"ovs-vsctl del-port br-int eth2",          //
		"nft flush ruleset",                       // drops the whole firewall ruleset at once
		"iptables -F",                             //
		"iptables -X",                             //
	} {
		if !IsDestructiveOp(op) {
			t.Errorf("IsDestructiveOp(%q) = false — a network-catastrophic verb must be caught even when the "+
				"model declares a benign op_class (predecessor: irreversible:network-catastrophic)", op)
		}
	}
	// The two DELIBERATE non-matches, asserted so a later widening is a conscious decision rather than a
	// silent one. `reload` collides with `systemctl reload` (a conservative-carve remediation verb here) —
	// the predecessor recorded the same decision. `no interface` / `no vlan` / `no router` are plausible
	// ENGLISH and this pattern is fed the proposal's RATIONALE, so they live on the floor-slug list instead.
	for _, op := range []string{
		"systemctl reload nginx",
		"reload",
		"there is no interface configured on that host",
		"no vlan tagging is in use here",
		"no router advertisement was observed",
	} {
		if IsDestructiveOp(op) {
			t.Errorf("IsDestructiveOp(%q) = true — this is a DELIBERATE non-match; widening it here is a "+
				"decision to make, not a default to drift into (see the exclusion note in safety.go)", op)
		}
	}
}

func TestIsDestructiveOpCodeDeployAndRepoWrite(t *testing.T) {
	// The gh/glab cases are the predecessor's own three branches (its HELD `code-deploy-or-repo-write` class,
	// which it kept OUT of its gate-governable set — permanently a human decision). The FLAG-LEVEL git cases
	// close the gap its coarse `git-write` verb match could not express: to it, `git push` and
	// `git push --force` were the same string, and only one of them destroys history.
	for _, op := range []string{
		"git push --force origin main",
		"git push --force-with-lease origin main",
		"git push -f origin main",
		"git push origin --delete release-1.2",
		"git branch -D release-1.2",
		"git branch --delete --remotes origin/hotfix",
		"git tag -d v1.4.0",
		"git tag --delete v1.4.0",
		"git reset --hard origin/main",
		"git clean -fdx",
		"git update-ref -d refs/heads/main",
		"git filter-repo --path secrets --invert-paths",
		"gh pr merge 42 --squash",
		"glab mr merge 42",
		"gh release create v2.0.0",
		"glab release delete v2.0.0",
		"gh repo delete acme/thing",
		"gh api /repos/acme/thing/git/refs/heads/main -X DELETE",
		"glab api projects/7/pipelines/91 --method DELETE",
		"glab pipeline delete 91",
		"glab environment stop staging",
		"glab deploy-key revoke 12",
		"deploy_key delete 12",
		"gh run delete 5512",
		"gh secret delete DEPLOY_TOKEN",
		"gitlab-runner unregister --all-runners",
	} {
		if !IsDestructiveOp(op) {
			t.Errorf("IsDestructiveOp(%q) = false — a code-deploy / repo-write verb must be caught: the review "+
				"flow that makes a change safe is exactly what it bypasses, and a deleted ref takes its own "+
				"audit trail with it (predecessor: code-deploy-or-repo-write, a HELD class)", op)
		}
	}
	// The READ and NON-DESTRUCTIVE git/gh shapes must stay clear, or the deriver polls every session that
	// merely looks at a repository and the signal becomes noise.
	for _, op := range []string{
		"git status", "git log --oneline -12", "git diff origin/main", "git fetch --all",
		"git push origin main", // a plain push is not history destruction
		"git branch --list", "git tag --list", "git clean --dry-run",
		"gh pr view 42", "gh run list", "glab ci status",
		"the pipeline was deleted by the release engineer last week", // rationale prose, not a command
	} {
		if IsDestructiveOp(op) {
			t.Errorf("IsDestructiveOp(%q) = true — a read-only or non-destructive repo op must not be floored", op)
		}
	}
}

// The floor SLUGS are the other half of TG-224: destructiveOpRE reads the actual op string, the slug list
// reads the DECLARED op_class. A verb that arrives as a registered class (rather than as a raw command) meets
// the floor here, which is why the slugs must exist before the class does.
func TestNeverAutoFloorCoversNetworkAndRepoWriteSlugs(t *testing.T) {
	for _, op := range []string{
		// network-catastrophic
		"write-erase", "erase-startup-config", "erase-nvram", "erase-flash", "no-ip-routing",
		"default-interface", "clear-configure-all", "no-interface", "interface-shutdown",
		"vlan-delete", "route-delete", "acl-delete", "trunk-remove", "no-spanning-tree",
		// code-deploy / repo-write
		"force-push", "branch-delete", "tag-delete", "ref-delete", "deploy-key-revoke",
		"pipeline-delete", "environment-destroy", "repo-delete", "release-delete", "runner-unregister",
		// case/space variants must still be floored (normalization is fail-closed)
		" Write-Erase ", "FORCE-PUSH",
	} {
		if !IsNeverAuto(op) {
			t.Errorf("op-class %q must be on the never-auto floor", op)
		}
	}
	// …and the floor must still discriminate: the shipped remediation verbs are not floored.
	for _, op := range []string{"restart-service", "start-guest", "restart-container", "disk-grow"} {
		if IsNeverAuto(op) {
			t.Errorf("op-class %q must NOT be floored — a blanket floor makes the list meaningless", op)
		}
	}
}

func TestNeverAutoFloorCoversDestructiveSlugs(t *testing.T) {
	for _, op := range []string{
		"wipefs", "shred", "blkdiscard", "dd", "vgremove", "lvremove", "pvremove",
		"zfs-rollback", "zpool-offline", "drop-table", "truncate-table", "drop-database",
		"docker-system-prune", "docker-volume-prune", "docker-network-prune",
		"shutdown", "halt", "poweroff",
		"MKFS", " Reboot ", // case/space variants must still be floored
	} {
		if !IsNeverAuto(op) {
			t.Errorf("op-class %q must be on the never-auto floor", op)
		}
	}
	if IsNeverAuto("restart-service") || IsNeverAuto("kubectl-get") {
		t.Error("a benign op-class must not be floored")
	}
}
