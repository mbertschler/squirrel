package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"path"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// transportChecksums reads a native destination's checksums through
// squirrel's own transport: a local disk's artifacts are read back through
// BLAKE3, and through any other hash their upload rows recorded; an sftp
// server's are hashed by its hash command, and fingerprint nothing on a
// server without one.
type transportChecksums struct {
	tr   transport
	dest *config.Destination
}

func (c transportChecksums) objects(ctx context.Context, rows []store.RemoteObjectRecord) (artifactChecksums, error) {
	names := namerFor(c.dest)
	want := make(map[string]string, len(rows))
	for _, r := range rows {
		want[names.object(r.Blake3)] = r.ChecksumAlgo.String
	}
	return c.read(ctx, ObjectsDirName, want)
}

func (c transportChecksums) packs(ctx context.Context, packs []store.RemotePackRecord) (artifactChecksums, error) {
	names := namerFor(c.dest)
	want := make(map[string]string, len(packs))
	for _, p := range packs {
		want[names.pack(p.PackKey)] = p.ChecksumAlgo.String
	}
	return c.read(ctx, PacksDirName, want)
}

// read lists dir and hashes each file a row recorded; want maps a
// basename to the algo its row recorded, "" while pending. A dir that does
// not exist holds nothing.
func (c transportChecksums) read(ctx context.Context, dir string, want map[string]string) (artifactChecksums, error) {
	out := artifactChecksums{byName: map[string]map[string]string{}, unchecked: map[string]bool{}}
	entries, err := c.tr.List(ctx, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return artifactChecksums{}, err
	}
	for _, e := range entries {
		if e.kind != kindFile {
			continue
		}
		recorded, ok := want[e.name]
		if !ok {
			out.byName[e.name] = nil
			continue
		}
		hashes, err := c.hash(ctx, path.Join(dir, e.name), recorded)
		if err != nil {
			return artifactChecksums{}, fmt.Errorf("hash %s: %w", e.name, err)
		}
		out.byName[e.name] = hashes
		out.unchecked[e.name] = hashes == nil
	}
	return out, nil
}

// hash is the checksums of the file at name: its BLAKE3, and the hash its
// row recorded when this machine computes that one too, read back from a
// local disk; the server's hash command's answer on sftp. It is nil when
// the sftp server cannot answer for the row: no command squirrel trusts, or
// a row recorded under another hash_algo.
func (c transportChecksums) hash(ctx context.Context, name, recorded string) (map[string]string, error) {
	if !readsBack(c.dest) {
		if recorded != "" && recorded != c.dest.HashAlgo {
			return nil, nil
		}
		cs, err := c.tr.ServerHash(ctx, name)
		if errors.Is(err, errNoServerHash) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]string{cs.Algo: cs.Value}, nil
	}
	hashers := map[string]hash.Hash{store.ChecksumAlgoBlake3: blake3.New()}
	if h := newArtifactHash(recorded); h != nil {
		hashers[recorded] = h
	}
	writers := make([]io.Writer, 0, len(hashers))
	for _, h := range hashers {
		writers = append(writers, h)
	}
	if err := copyOut(ctx, c.tr, name, io.MultiWriter(writers...)); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(hashers))
	for algo, h := range hashers {
		out[algo] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}
