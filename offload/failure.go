package offload

import "sort"

// FailureKind classifies a gate refusal by the condition that failed, so
// callers can group and count equivalent refusals without parsing prose.
// The string values are stable and safe to print.
type FailureKind string

const (
	// FailureNoEvidence: the target's durability vector holds no
	// component at all for the content's origin node.
	FailureNoEvidence FailureKind = "no-evidence"
	// FailureEvidenceBehind: a component exists but covers an older
	// origin run than the gated content needs.
	FailureEvidenceBehind FailureKind = "evidence-behind"
	// FailureEvidenceStale: coverage is sound, but the evidence has not
	// been re-verified inside the volume's offload_max_evidence_age.
	FailureEvidenceStale FailureKind = "evidence-stale"
	// FailureNotPushed: no completed whole-volume push covers the run in
	// which the path became present (or, for a relayed target, no push
	// freshness has been reported for the origin).
	FailureNotPushed FailureKind = "not-pushed"
	// FailureNotVerified: the component rests on presence rather than on
	// content verification, and no verified fingerprint backs the object.
	FailureNotVerified FailureKind = "not-content-verified"
)

// Failure is one required target's refusal of one file. The three text
// fields separate the three questions an operator asks, so a report can
// aggregate the first two and keep the third for the bug report:
//
//   - Summary says what is wrong, in words that explain themselves;
//   - Advice says what makes it pass, and where that has to happen;
//   - Detail carries the durability-vector coordinates verbatim (runs,
//     methods, provenance) — precise, and per-file.
//
// Summary and Advice are deliberately free of per-file numbers: files
// refused for the same cause share them exactly, which is what makes
// (Target, Kind, Summary) a sound grouping key.
type Failure struct {
	Target  string
	Kind    FailureKind
	Summary string
	Advice  string
	Detail  string
}

// String renders the one-line per-file form: the target and what is
// wrong with it.
func (f Failure) String() string {
	return f.Target + ": " + f.Summary
}

// BlockedGroup aggregates every file one target refused for the same
// reason: the unit of the aggregated report ("25 files blocked by
// cloudbox, because …"). Grouping is by (Target, Kind, Summary), so a
// heterogeneous refusal set never collapses — 25 files blocked on one
// target and one blocked on another stay two groups, as do two different
// causes on the same target.
type BlockedGroup struct {
	Target  string
	Summary string
	Advice  string
	// Files counts the files in this group. A file blocked by several
	// targets is counted once per group it lands in, so the group counts
	// can sum to more than Report.NotDurable.
	Files int
	// ExamplePath and ExampleDetail carry one member's path and its exact
	// vector coordinates, so the precise numbers stay one line away from
	// the aggregate.
	ExamplePath   string
	ExampleDetail string
	// UniformDetail is true when every file in the group carries the same
	// Detail — then ExampleDetail describes all of them rather than
	// standing in for a set that varies.
	UniformDetail bool
}

// RequirementStatus reports how one offload_requires target fared across
// the evaluated files. It exists so a report can say what *passed*:
// "one target away" and "nothing is durable anywhere" are different
// situations, and only the affirmative half distinguishes them.
type RequirementStatus struct {
	Target    string
	Satisfied int
	Blocked   int
}

// groupKey is the aggregation identity of a refusal: same target, same
// condition, same explanation.
type groupKey struct {
	target  string
	kind    FailureKind
	summary string
}

// summarise fills Blocked and Requirements from the per-file results.
// require is the volume's policy, in config order, which is also the
// order Requirements comes back in. Called once per invocation, after
// the per-file pass.
func (r *Report) summarise(require []string) {
	status := make([]RequirementStatus, len(require))
	index := make(map[string]int, len(require))
	for i, target := range require {
		status[i] = RequirementStatus{Target: target}
		index[target] = i
	}
	groups := make(map[groupKey]*BlockedGroup)
	for _, res := range r.Results {
		if len(res.Failures) == 0 {
			for i := range status {
				status[i].Satisfied++
			}
			continue
		}
		blocked := make(map[string]bool, len(res.Failures))
		for _, f := range res.Failures {
			addToGroup(groups, res.Path, f)
			blocked[f.Target] = true
		}
		for i := range status {
			if blocked[status[i].Target] {
				status[i].Blocked++
			} else {
				status[i].Satisfied++
			}
		}
	}
	r.Requirements = status
	r.Blocked = sortedGroups(groups, index)
}

// addToGroup folds one failure into its group, creating it on first
// sight and tracking whether the group's coordinates are uniform.
func addToGroup(groups map[groupKey]*BlockedGroup, path string, f Failure) {
	key := groupKey{target: f.Target, kind: f.Kind, summary: f.Summary}
	g, ok := groups[key]
	if !ok {
		g = &BlockedGroup{
			Target: f.Target, Summary: f.Summary, Advice: f.Advice,
			ExamplePath: path, ExampleDetail: f.Detail, UniformDetail: true,
		}
		groups[key] = g
	}
	if f.Detail != g.ExampleDetail {
		g.UniformDetail = false
	}
	g.Files++
}

// sortedGroups flattens the group map into policy order (the order the
// targets appear in offload_requires), then most-blocked first, then by
// explanation — a total order, so the report is deterministic.
func sortedGroups(groups map[groupKey]*BlockedGroup, index map[string]int) []BlockedGroup {
	out := make([]BlockedGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if index[a.Target] != index[b.Target] {
			return index[a.Target] < index[b.Target]
		}
		if a.Files != b.Files {
			return a.Files > b.Files
		}
		return a.Summary < b.Summary
	})
	return out
}
