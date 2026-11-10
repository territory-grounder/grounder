package faultinjector

import (
	"fmt"
	"testing"
	"time"
)

// TG-624 G3: every class must sweep the WHOLE pool, whatever the pool size and the rotation length. With the
// start offset taken as Cycle % n, a 10-guest pool and a 5-class rotation only ever started device-down at
// guests 0 and 5, so two soak VMs were never chosen for it in 39 live faults.
func TestEveryClassSweepsTheWholePool(t *testing.T) {
	rotation := []Class{ClassDeviceDown, ClassContainerDown, ClassServiceDown, ClassContainerHang, ClassServiceHang}
	var pool []PoolGuest
	status := map[string]string{}
	allow := map[string]bool{}
	for i := 0; i < 10; i++ {
		g := PoolGuest{VMID: fmt.Sprintf("%d", 100+i), Name: fmt.Sprintf("guest-%02d", i), Node: "n", Container: "app", Unit: "web"}
		pool = append(pool, g)
		status[g.VMID] = "running"
		allow[g.Name] = true
	}
	chosen := map[Class]map[string]bool{}
	for cycle := 0; cycle < len(rotation)*len(pool)*2; cycle++ {
		st := State{Now: time.Now(), Pool: pool, Allowlist: allow, Status: status, Cycle: cycle,
			Limits: Limits{MaxDown: 2, MaxBusy: 5}}
		d := PlanNext(st, rotation)
		if !d.Act {
			continue
		}
		if chosen[d.Class] == nil {
			chosen[d.Class] = map[string]bool{}
		}
		chosen[d.Class][d.Guest.Name] = true
	}
	for _, c := range rotation {
		if len(chosen[c]) != len(pool) {
			t.Errorf("class %s reached %d of %d guests: %v", c, len(chosen[c]), len(pool), chosen[c])
		}
	}
}
