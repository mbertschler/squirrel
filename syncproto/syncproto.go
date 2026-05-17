// Package syncproto defines the JSON wire types exchanged between an
// initiator and a receiver during a node-to-node sync. The types are
// shared by daemon (server-side handlers) and sync (client-side
// driver) so the wire contract has exactly one definition.
//
// Endpoint summary (all under /v1/sync/, all bearer-authenticated):
//
//	POST /v1/sync/begin   handshake → receiver allocates a run id
//	POST /v1/sync/plan    initiator sends index slice; receiver
//	                      returns per-path dispositions
//	POST /v1/sync/verify  initiator notifies "rclone done"; receiver
//	                      re-hashes transfer+supersede paths
//	POST /v1/sync/close   initiator finalises; receiver commits the
//	                      index updates and advances the watermark
//
// Wire-level invariants:
//
//   - Paths are always relative to the volume root (matches the
//     index's FileRow.Path representation, never the absolute
//     filesystem path).
//   - BLAKE3 digests travel as 64-char lowercase hex strings (not raw
//     bytes), so payloads stay JSON-clean and human-readable in
//     captured traffic.
//   - Field names are part of the protocol; do not rename them
//     without bumping the version path.
package syncproto

// DispositionAlreadyCorrect — both sides have the same (path, blake3).
// rclone need not touch this path.
const DispositionAlreadyCorrect = "already-correct"

// DispositionTransfer — receiver has no live row at this path; the
// initiator's bytes are new content for the receiver.
const DispositionTransfer = "transfer"

// DispositionSupersede — receiver has a live row at this path with a
// different blake3, *and* the row's provenance traces back to this
// initiator at or before the last shared watermark. The receiver
// pre-moves the prior bytes to `.squirrel-history/run-<id>/` before
// responding. Ordinary supersession.
const DispositionSupersede = "supersede"

// DispositionConflict — receiver has a live row at this path with a
// different blake3 that is not traceable to this initiator's prior
// writes (local write on the receiver, or sourced from a different
// peer post-watermark). The receiver pre-moves the prior bytes to
// `.squirrel-conflicts/run-<id>/<path>` and seeds a new `present` row
// at that path carrying the prior blake3 + prior provenance, so both
// versions remain reachable by hash and by path. The initiator wins
// live: rclone delivers its bytes to the original path and /close
// inserts a new `present` row there with `source_node_id = initiator`.
const DispositionConflict = "conflict"

// BeginRequest opens a peer-sync session.
type BeginRequest struct {
	// Volume is the volume name (matched against the receiver's
	// config + index). A volume name unknown to the receiver fails
	// the handshake — silent volume materialisation is exactly the
	// kind of surprise the design forbids.
	Volume string `json:"volume"`
	// InitiatorNodeName is the initiator's declared identity. The
	// receiver looks up a peer-nodes row by this name (creating one
	// on first contact). A name collision with a different existing
	// row fails the handshake.
	InitiatorNodeName string `json:"initiator_node_name"`
	// InitiatorEndpoint is the URL the receiver could in theory dial
	// back to reach the initiator. Stored on the peer nodes row for
	// future symmetry; not used by PR 3.
	InitiatorEndpoint string `json:"initiator_endpoint,omitempty"`
	// InitiatorRunID is the initiator's *local* runs.id for this
	// sync. The receiver records it as runs.correlated_run_id and
	// later as peer_sync_state.last_shared_run_id; the two sides
	// thus share one logical identifier without negotiating a UUID.
	InitiatorRunID int64 `json:"initiator_run_id"`
}

// BeginResponse closes the handshake.
type BeginResponse struct {
	// ReceiverRunID is the receiver-side runs.id allocated for this
	// session. Every subsequent /v1/sync/* request from the
	// initiator references it.
	ReceiverRunID int64 `json:"receiver_run_id"`
	// ReceiverNodeName is the receiver's self-row name. The
	// initiator stores it on its own runs row (via
	// runs.peer_node_id) so the local audit log is symmetric with
	// the receiver's.
	ReceiverNodeName string `json:"receiver_node_name"`
}

// PlanRequest carries the initiator's index slice.
type PlanRequest struct {
	ReceiverRunID int64        `json:"receiver_run_id"`
	Entries       []IndexEntry `json:"entries"`
}

// IndexEntry is one (path, blake3, size, mtime) tuple sent from the
// initiator's index. mtime_ns travels for symmetry with the schema
// even though the plan decision is by blake3 + path + provenance, not
// timestamp.
type IndexEntry struct {
	Path      string `json:"path"`
	Blake3Hex string `json:"blake3"`
	SizeBytes int64  `json:"size_bytes"`
	MtimeNs   int64  `json:"mtime_ns"`
}

// PlanResponse carries the receiver's per-path verdict.
type PlanResponse struct {
	// Dispositions has one entry per path the initiator sent (so the
	// client can drive rclone in one pass without re-cross-referencing
	// against PlanRequest).
	Dispositions []PlanDisposition `json:"dispositions"`
	// Conflicts captures the paths whose disposition was "conflict",
	// with the prior bytes' preserved path on the receiver attached so
	// the initiator's CLI can render a meaningful "preserved at ..."
	// line. A conflict is no longer a fatal disposition: the receiver
	// has already pre-staged the loser under .squirrel-conflicts/ and
	// the initiator's bytes are still in scope for the rclone transfer.
	Conflicts []ConflictDetail `json:"conflicts,omitempty"`
}

// PlanDisposition is the receiver's verdict on one path.
type PlanDisposition struct {
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	// Blake3Hex is the *initiator's* digest, echoed back so the
	// client can sanity-check that path↔hash didn't get reordered
	// across the wire.
	Blake3Hex string `json:"blake3"`
}

// ConflictDetail names a conflicting path and surfaces enough metadata
// for the CLI to render a meaningful summary. ReceiverBlake3Hex is the
// digest the receiver had at this path before the pre-stage move;
// Reason classifies why the supersede disposition was refused (e.g.
// "local write on receiver", "sourced from a different peer").
// PreservedAtPath is the volume-relative path the receiver moved the
// prior bytes to (`.squirrel-conflicts/run-<id>/<path>`) so the CLI
// can point the operator at the preserved version.
type ConflictDetail struct {
	Path               string `json:"path"`
	InitiatorBlake3Hex string `json:"initiator_blake3"`
	ReceiverBlake3Hex  string `json:"receiver_blake3"`
	Reason             string `json:"reason"`
	PreservedAtPath    string `json:"preserved_at_path,omitempty"`
}

// VerifyRequest tells the receiver "rclone said it finished, please
// re-hash the affected paths now".
type VerifyRequest struct {
	ReceiverRunID int64 `json:"receiver_run_id"`
	// Paths, when non-empty, narrows the verify scope to just those
	// relative paths. Empty means "everything in the transfer +
	// supersede buckets from the original plan" — the default.
	// Initiator-driven retries set this to the failing subset so the
	// second verify pass doesn't re-hash already-verified files.
	Paths []string `json:"paths,omitempty"`
}

// VerifyResponse is the post-transfer reconciliation report.
type VerifyResponse struct {
	// Matched lists paths whose on-disk BLAKE3 agrees with the plan's
	// initiator digest. The receiver may still drop the contents (for
	// memory bounds) — only counts are guaranteed when len exceeds a
	// CLI-friendly threshold. PR 3 returns full lists; future
	// hardening may bound them.
	Matched []string `json:"matched"`
	// Mismatched, Missing, Unexpected describe the deltas. Empty for
	// a clean transfer.
	Mismatched []VerifyMismatch `json:"mismatched,omitempty"`
	Missing    []string         `json:"missing,omitempty"`
	Unexpected []string         `json:"unexpected,omitempty"`
}

// VerifyMismatch documents one path whose on-disk hash differs from
// what the initiator's index claimed.
type VerifyMismatch struct {
	Path        string `json:"path"`
	ExpectedHex string `json:"expected"`
	ActualHex   string `json:"actual"`
}

// CloseRequest finalises the receiver-side session.
type CloseRequest struct {
	ReceiverRunID int64 `json:"receiver_run_id"`
	// Status is the initiator's terminal verdict: "success" when
	// verify came back clean, "partial" when one or more retries
	// failed and we're accepting the leftover delta until the next
	// sync replans. "failed" aborts without advancing the watermark.
	Status string `json:"status"`
	// FailedPaths carries the verifier's last-known mismatched /
	// missing entries so the receiver can refrain from committing
	// new file rows for them (the prior superseded row stays
	// superseded; no new live row materialises until a future sync
	// re-transfers). Empty on Status = "success".
	FailedPaths []string `json:"failed_paths,omitempty"`
}

// CloseResponse acknowledges and surfaces the receiver-side run id +
// committed file count for diagnostic clarity. No new wire data is
// required; the initiator already has the disposition list from
// /plan.
type CloseResponse struct {
	ReceiverRunID int64 `json:"receiver_run_id"`
	Committed     int   `json:"committed"`
}

// ErrorResponse is the uniform error body. Mirrors daemon's
// errorResponse type but is exported so client-side decoding can name it.
type ErrorResponse struct {
	Error string `json:"error"`
}
