// Package runevents defines the shared in-process progress event shape
// that the index and sync packages emit while a long-running operation
// is in flight. The desktop UI subscribes to these events to drive its
// live-progress SSE channel; the CLI ignores them.
//
// The type lives in its own neutral package so neither index nor sync
// has to import the other. Duplicate-and-keep-separate was considered
// but the two surfaces report into the same UI widget, and a divergent
// shape would force the UI layer to fan-in two near-identical structs.
package runevents

// Stage names the phase the producer is currently in. Kept to a small
// fixed vocabulary so the UI can render a stable badge per stage rather
// than echoing free-form strings.
const (
	StageWalking   = "walking"   // index: filesystem traversal in progress
	StageHashing   = "hashing"   // index: regular files being hashed
	StageUploading = "uploading" // sync: rclone copy in progress
	StageDone      = "done"      // terminal — used by the final frame
)

// Progress is one in-flight datapoint from an index or sync run. All
// fields are optional from the consumer's point of view: zero values
// render as "unknown". Bytes fields are only populated by the sync
// path; the index path reports file counts only because the indexer
// does not know the total bytes up front and emitting them would
// require an extra stat pass.
type Progress struct {
	Stage      string
	Done       int64
	Total      int64
	BytesDone  int64
	BytesTotal int64
	Message    string
}
