package offload

import (
	"fmt"
	"testing"
)

// blockedResult builds one not-durable result refused by the listed
// targets, each with a per-file Detail so the uniformity tracking is
// exercised.
func blockedResult(path string, failures ...Failure) FileResult {
	return FileResult{Path: path, Outcome: OutcomeNotDurable, Failures: failures}
}

func noEvidence(target, detail string) Failure {
	return Failure{
		Target:  target,
		Kind:    FailureNoEvidence,
		Summary: "no durability evidence yet: " + target + " has never reported holding content that originated on laptop",
		Advice:  "sync to " + target,
		Detail:  detail,
	}
}

// TestSummariseKeepsHeterogeneousSetsApart is the aggregation guarantee
// that matters: 25 files blocked on one target and 1 on another is a
// different situation from 26 on the same one, so the two must never
// collapse into one bucket — nor may two different causes on the same
// target.
func TestSummariseKeepsHeterogeneousSetsApart(t *testing.T) {
	var rep Report
	for i := 0; i < 25; i++ {
		rep.record(blockedResult(pathN(i), noEvidence("cloudbox", "coords")))
	}
	rep.record(blockedResult("odd.jpg", noEvidence("s3archive", "coords")))
	rep.record(FileResult{Path: "fine.jpg", Outcome: OutcomeOffloaded})
	rep.summarise([]string{"nas", "cloudbox", "s3archive"})

	if len(rep.Blocked) != 2 {
		t.Fatalf("blocked groups = %+v, want one per target", rep.Blocked)
	}
	if got := rep.Blocked[0]; got.Target != "cloudbox" || got.Files != 25 {
		t.Fatalf("first group = %+v, want cloudbox with 25 files", got)
	}
	if got := rep.Blocked[1]; got.Target != "s3archive" || got.Files != 1 {
		t.Fatalf("second group = %+v, want s3archive with 1 file", got)
	}
	if !rep.Blocked[0].UniformDetail || rep.Blocked[0].ExamplePath != "f00.jpg" {
		t.Fatalf("group detail = %+v, want uniform coordinates and an example path", rep.Blocked[0])
	}
}

// TestSummariseSplitsCausesOnOneTarget: the same target refusing two
// files for different reasons stays two groups, because the explanation
// — not just the target — is part of the identity.
func TestSummariseSplitsCausesOnOneTarget(t *testing.T) {
	var rep Report
	rep.record(blockedResult("a.jpg", noEvidence("cloudbox", "coords-a")))
	rep.record(blockedResult("b.jpg", Failure{
		Target: "cloudbox", Kind: FailureNotPushed,
		Summary: "no whole-volume push to cloudbox has been reported for content from laptop",
		Detail:  "coords-b",
	}))
	rep.summarise([]string{"cloudbox"})

	if len(rep.Blocked) != 2 {
		t.Fatalf("blocked groups = %+v, want the two causes kept apart", rep.Blocked)
	}
	for _, g := range rep.Blocked {
		if g.Files != 1 {
			t.Fatalf("group %+v, want one file each", g)
		}
	}
}

// TestSummariseReportsWhatPassed: the affirmative half. A file blocked by
// one of three required targets must leave the other two visibly
// satisfied, so "one target away" reads differently from "nothing is
// durable anywhere".
func TestSummariseReportsWhatPassed(t *testing.T) {
	var rep Report
	rep.record(blockedResult("a.jpg", noEvidence("cloudbox", "coords")))
	rep.record(FileResult{Path: "b.jpg", Outcome: OutcomeOffloaded})
	rep.summarise([]string{"nas", "cloudbox", "s3archive"})

	want := []RequirementStatus{
		{Target: "nas", Satisfied: 2},
		{Target: "cloudbox", Satisfied: 1, Blocked: 1},
		{Target: "s3archive", Satisfied: 2},
	}
	if len(rep.Requirements) != len(want) {
		t.Fatalf("requirements = %+v, want one per policy target in order", rep.Requirements)
	}
	for i, w := range want {
		if rep.Requirements[i] != w {
			t.Fatalf("requirement %d = %+v, want %+v", i, rep.Requirements[i], w)
		}
	}
}

// TestSummariseMarksVaryingCoordinates: a group whose members carry
// different vector coordinates says so, so the printed example is never
// mistaken for the whole set.
func TestSummariseMarksVaryingCoordinates(t *testing.T) {
	var rep Report
	rep.record(blockedResult("a.jpg", noEvidence("cloudbox", "needs run 45")))
	rep.record(blockedResult("b.jpg", noEvidence("cloudbox", "needs run 46")))
	rep.summarise([]string{"cloudbox"})

	if len(rep.Blocked) != 1 || rep.Blocked[0].Files != 2 {
		t.Fatalf("blocked groups = %+v, want one group of two files", rep.Blocked)
	}
	if rep.Blocked[0].UniformDetail {
		t.Fatalf("group = %+v, want the differing coordinates flagged", rep.Blocked[0])
	}
}

// pathN names a distinct blocked file, zero-padded so the report's
// example path is predictable.
func pathN(i int) string { return fmt.Sprintf("f%02d.jpg", i) }
