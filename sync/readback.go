package sync

import (
	"context"
	"errors"
	"io"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
)

// errReadBackMismatch marks a stored file whose bytes, read back, are not
// the bytes squirrel sent: the disk kept something else.
var errReadBackMismatch = errors.New("the stored bytes read back differently from the bytes sent")

// readsBack reports whether dest is a disk squirrel reads back, BLAKE3 over
// what it just stored, before trusting it: a local destination. An sftp
// server's bytes are read back only through its hash command.
func readsBack(dest *config.Destination) bool {
	return dest.Type == "local"
}

// readBack hashes the file at name as the destination returns it. The
// local transport reads past the page cache where the platform allows, so
// the hash covers what reached the disk.
func readBack(ctx context.Context, tr transport, name string) ([]byte, error) {
	rc, err := tr.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	h := blake3.New()
	if _, err := io.CopyBuffer(h, ctxReader{ctx: ctx, r: rc}, make([]byte, copyBufferSize)); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
