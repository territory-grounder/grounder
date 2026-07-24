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
