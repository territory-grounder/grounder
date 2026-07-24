package safety

import "testing"

func TestIsStatefulWorkload(t *testing.T) {
	stateful := []string{"etcd-0", "dc1-postgres01", "redis-cache", "prod-db", "user-database", "sts/kafka", "a statefulset", "victoriametrics", "seaweedfs-volume"}
	for _, s := range stateful {
		if !IsStatefulWorkload(s) {
			t.Errorf("%q must be recognized as a stateful workload", s)
		}
	}
	benign := []string{"web01", "nginx", "frr-router", "sw-core-01", "grafana-dashboard"}
	for _, s := range benign {
		if IsStatefulWorkload(s) {
			t.Errorf("%q must NOT be flagged stateful", s)
		}
	}
	// matches across any of the parts (target, op, params)
	if !IsStatefulWorkload("web01", "kubectl rollout restart statefulset/etcd") {
		t.Error("a stateful workload named in the op must be caught")
	}
}
