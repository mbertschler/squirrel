package sync

import (
	"context"
	gosync "sync"

	"github.com/mbertschler/squirrel/config"
)

// A native push writes in batches: every file of a batch is staged before
// one Flush makes the batch durable, and every record waits for the flush
// that covers its bytes. A batch holds at most batchMaxFiles files and
// batchMaxBytes bytes; a larger file goes alone.
const (
	batchMaxFiles = 64
	batchMaxBytes = 256 << 20
)

// Default paths in flight when a destination sets no concurrency: a local
// disk gains nothing past a few, a remote link hides its round trips
// behind more.
const (
	defaultLocalConcurrency = 4
	defaultSFTPConcurrency  = 8
)

// pushConcurrency is how many files a push writes to dest at once.
func pushConcurrency(dest *config.Destination) int {
	switch {
	case dest.Concurrency > 0:
		return dest.Concurrency
	case dest.Type == "sftp":
		return defaultSFTPConcurrency
	default:
		return defaultLocalConcurrency
	}
}

// batches splits items, in order, into runs of at most batchMaxFiles items
// and batchMaxBytes bytes by size.
func batches[T any](items []T, size func(T) int64) [][]T {
	var out [][]T
	var cur []T
	var bytes int64
	for _, it := range items {
		n := size(it)
		if len(cur) > 0 && (len(cur) == batchMaxFiles || bytes+n > batchMaxBytes) {
			out = append(out, cur)
			cur, bytes = nil, 0
		}
		cur = append(cur, it)
		bytes += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// inFlight calls fn for each item, at most n at a time and in order of
// starting, and returns once every call has returned. It starts no call
// once ctx is done or stop reports true. With n of 1 the calls run one
// after another, in order.
func inFlight[T any](ctx context.Context, n int, items []T, stop func() bool, fn func(T)) {
	slots := make(chan struct{}, max(n, 1))
	var wg gosync.WaitGroup
	for _, it := range items {
		slots <- struct{}{}
		if ctx.Err() != nil || stop() {
			<-slots
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			fn(it)
		})
	}
	wg.Wait()
}
