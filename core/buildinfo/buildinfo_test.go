package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromSettingsReadsRevisionAndDirtyFlag(t *testing.T) {
	rev, dirty := fromSettings([]debug.BuildSetting{
		{Key: "-trimpath", Value: "true"},
		{Key: "vcs.revision", Value: "96dff7e86641b1c92da4d20c1f155521cdb272e1"},
		{Key: "vcs.modified", Value: "true"},
	})
	if rev != "96dff7e86641b1c92da4d20c1f155521cdb272e1" || !dirty {
		t.Fatalf("got rev=%q dirty=%v", rev, dirty)
	}
	if rev, dirty := fromSettings(nil); rev != "" || dirty {
		t.Fatal("no settings must read as unrecorded, never as a fabricated revision")
	}
}

func TestDescribeIsThePrefixPlusDirtyOrUnknown(t *testing.T) {
	if got := describe("", false); got != "unknown" {
		t.Fatalf("empty revision must describe as unknown, got %q", got)
	}
	if got := describe("96dff7e86641b1c92da4d20c1f155521cdb272e1", false); got != "96dff7e86641" {
		t.Fatalf("got %q", got)
	}
	if got := describe("96dff7e86641b1c92da4d20c1f155521cdb272e1", true); got != "96dff7e86641+dirty" {
		t.Fatalf("got %q", got)
	}
}

// The real read must not panic and must agree with itself (Describe derives from Revision/Modified).
func TestLiveReadIsConsistent(t *testing.T) {
	if Revision() == "" && Describe() != "unknown" {
		t.Fatalf("no revision but Describe=%q", Describe())
	}
	if Revision() != "" && Describe() == "unknown" {
		t.Fatal("a stamped revision must not describe as unknown")
	}
}
