package faultinjector

import (
	"context"
	"strings"
	"testing"
)

const gib = int64(1) << 30

// A normal guest: fill from 45% to the 91% target, leaving real headroom.
func TestPlanDiskFill_HitsTheAlertingBandWithHeadroom(t *testing.T) {
	size, used := 10*gib, 45*gib/10 // 10 GiB, 4.5 GiB used (45%)
	p, err := planDiskFill(size, used)
	if err != nil {
		t.Fatalf("want a plan, got %v", err)
	}
	// Integer division floors, so assert the BAND rather than an exact percentage: the requirement is "at or
	// above LibreNMS rule 22's 90% threshold, without overshooting the 91% target into the range where a guest
	// starts failing for reasons unrelated to the fault under test".
	after := (p.UsedBytes + p.AllocBytes) * 100 / p.SizeBytes
	if after < 90 || after > fillTargetPercent {
		t.Fatalf("post-fill usage = %d%%, want within [90, %d] so rule 22 fires without wedging the guest", after, fillTargetPercent)
	}
	if p.FreeAfter <= 0 {
		t.Fatal("no headroom left")
	}
}

// A guest already above target needs no fault. Injecting anyway would record an obligation for a fault we did
// not cause and would not clean up correctly.
func TestPlanDiskFill_RefusesAGuestAlreadyFull(t *testing.T) {
	if _, err := planDiskFill(10*gib, 95*gib/10); err == nil {
		t.Fatal("planned a fill on a guest already above target — no fault is needed there")
	}
}

// A guest so small that 91% leaves under 128 MiB free must be refused: with no headroom it cannot run the
// diagnostics TG is meant to perform on it, so the drill would measure our ability to wedge a machine rather
// than TG's ability to notice a disk filling.
func TestPlanDiskFill_RefusesToWedgeATinyGuest(t *testing.T) {
	small := int64(512) << 20 // 512 MiB; 9% of it is ~46 MiB
	if _, err := planDiskFill(small, small/2); err == nil {
		t.Fatal("planned a fill leaving <128 MiB free — that wedges the guest instead of testing detection")
	}
}

func TestPlanDiskFill_RejectsNonsenseGeometry(t *testing.T) {
	if _, err := planDiskFill(0, 0); err == nil {
		t.Fatal("accepted a zero-size filesystem")
	}
}

// The fill must never overshoot into the range where a guest starts failing for unrelated reasons.
func TestPlanDiskFill_NeverExceedsTheTargetAcrossManyGeometries(t *testing.T) {
	for _, size := range []int64{2 * gib, 8 * gib, 24 * gib, 64 * gib, 512 * gib} {
		for _, pct := range []int64{5, 20, 50, 80, 89} {
			p, err := planDiskFill(size, size*pct/100)
			if err != nil {
				continue // legitimately refused
			}
			after := (p.UsedBytes + p.AllocBytes) * 100 / p.SizeBytes
			if after > fillTargetPercent {
				t.Fatalf("size=%d used=%d%%: post-fill %d%% overshoots the %d%% target", size, pct, after, fillTargetPercent)
			}
		}
	}
}

func TestParseDF(t *testing.T) {
	size, used, err := parseDF("     1K-blocks      Used\n  10737418240 4831838208\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if size != 10737418240 || used != 4831838208 {
		t.Fatalf("size=%d used=%d", size, used)
	}
	if _, _, err := parseDF("nonsense"); err == nil {
		t.Fatal("accepted unparseable df output — better to refuse than to size a fill from a guess")
	}
}

// `test -e` exits non-zero when the path is absent, which is what "removed" means. Getting this backwards
// would mark a fill restored while it is still on disk — the exact false-clean that hid the strandings.
func TestVerifyFillRemoved_ReadsExitCodeCorrectly(t *testing.T) {
	f := newFake()
	f.code["test"] = 1 // absent
	gone, err := VerifyFillRemoved(context.Background(), f, "alpha", FillPath)
	if err != nil || !gone {
		t.Fatalf("absent file must verify as removed (gone=%v err=%v)", gone, err)
	}

	f2 := newFake()
	f2.code["test"] = 0 // still present
	gone, err = VerifyFillRemoved(context.Background(), f2, "alpha", FillPath)
	if err != nil || gone {
		t.Fatalf("present file must NOT verify as removed (gone=%v err=%v)", gone, err)
	}
}

// A device-down repair is verified by READING status, never by trusting `pct start`'s exit code — it exits
// non-zero on an already-running guest, which is a success for our purposes.
func TestVerifyGuestRunning_ReadsStatusNotExitCode(t *testing.T) {
	f := newFake()
	f.stdout["pct"] = "status: running\n"
	ok, err := VerifyGuestRunning(context.Background(), f, "pve01", "101")
	if err != nil || !ok {
		t.Fatalf("want running (ok=%v err=%v)", ok, err)
	}

	f2 := newFake()
	f2.stdout["pct"] = "status: stopped\n"
	ok, _ = VerifyGuestRunning(context.Background(), f2, "pve01", "101")
	if ok {
		t.Fatal("a stopped guest must not verify as running — that would close an obligation while the guest is down")
	}
}

func TestInjectDiskFill_UsesFallocateWithTheComputedSize(t *testing.T) {
	f := newFake()
	f.stdout["df"] = "size used\n10737418240 4831838208\n"
	plan, err := InjectDiskFill(context.Background(), f, "alpha")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var alloc []string
	for _, c := range f.calls {
		if c[0] == "fallocate" {
			alloc = c
		}
	}
	if alloc == nil {
		t.Fatal("no fallocate call")
	}
	if !strings.HasSuffix(strings.Join(alloc, " "), FillPath) {
		t.Fatalf("fill path not targeted: %v", alloc)
	}
	if plan.AllocBytes <= 0 {
		t.Fatal("non-positive allocation")
	}
}
