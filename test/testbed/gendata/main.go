// Command gendata writes a deterministic pseudo-random file tree for
// the local testbed (design/testbed.md). The same seed produces the
// same bytes, so a testbed walk is reproducible without ever
// committing generated data — only this generator is checked in.
//
// Each invocation writes one kind of tree into -out:
//
//	gendata -out .testbed/seed/photos -kind photos -years 2015-2024
//	gendata -out .testbed/seed/docs   -kind docs
//	gendata -out .testbed/seed/media  -kind media
//
// Per-file content is derived from (seed, relative path), so adding
// years or files later never changes bytes generated earlier.
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	out := flag.String("out", "", "target directory (required)")
	seed := flag.Int64("seed", 1, "master seed")
	kind := flag.String("kind", "", "photos | docs | media")
	years := flag.String("years", "2015-2024", "photos: inclusive year range")
	perYear := flag.Int("per-year", 24, "photos: small JPEGs per year")
	rawPerYear := flag.Int("raw-per-year", 2, "photos: large RAW files per year")
	count := flag.Int("count", 40, "docs: number of files")
	flag.Parse()

	if *out == "" || *kind == "" {
		fmt.Fprintln(os.Stderr, "gendata: -out and -kind are required")
		os.Exit(2)
	}
	var err error
	switch *kind {
	case "photos":
		err = genPhotos(*out, *seed, *years, *perYear, *rawPerYear)
	case "docs":
		err = genDocs(*out, *seed, *count)
	case "media":
		err = genMedia(*out, *seed)
	default:
		err = fmt.Errorf("unknown -kind %q", *kind)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gendata: %v\n", err)
		os.Exit(1)
	}
}

func genPhotos(out string, seed int64, years string, perYear, rawPerYear int) error {
	from, to, err := parseYears(years)
	if err != nil {
		return err
	}
	for y := from; y <= to; y++ {
		dir := filepath.Join(out, strconv.Itoa(y))
		for i := 0; i < perYear; i++ {
			name := fmt.Sprintf("IMG_%d_%04d.jpg", y, i)
			if err := writeFile(dir, name, seed, 40<<10, 400<<10); err != nil {
				return err
			}
		}
		for i := 0; i < rawPerYear; i++ {
			name := fmt.Sprintf("RAW_%d_%02d.dng", y, i)
			if err := writeFile(dir, name, seed, 2<<20, 3<<20); err != nil {
				return err
			}
		}
	}
	return nil
}

func genDocs(out string, seed int64, count int) error {
	dirs := []string{"invoices", "letters", "tax/2024", "notes"}
	exts := []string{".pdf", ".txt", ".md", ".odt"}
	for i := 0; i < count; i++ {
		dir := filepath.Join(out, dirs[i%len(dirs)])
		name := fmt.Sprintf("doc_%03d%s", i, exts[i%len(exts)])
		if err := writeFile(dir, name, seed, 1<<10, 200<<10); err != nil {
			return err
		}
	}
	return nil
}

func genMedia(out string, seed int64) error {
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("Movie_%d (20%02d).mkv", i, 10+i)
		if err := writeFile(filepath.Join(out, "movies"), name, seed, 32<<20, 32<<20); err != nil {
			return err
		}
	}
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("Show_S01E%02d.mkv", i)
		if err := writeFile(filepath.Join(out, "shows"), name, seed, 12<<20, 12<<20); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes minSize..maxSize pseudo-random bytes derived from
// (seed, dir-relative name), creating dir as needed. Existing files
// are overwritten with identical content (same derivation), which
// makes re-running the generator idempotent.
func writeFile(dir, name string, seed int64, minSize, maxSize int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	h := fnv.New64a()
	h.Write([]byte(filepath.Join(filepath.Base(dir), name)))
	rng := rand.New(rand.NewSource(seed ^ int64(h.Sum64())))
	size := minSize
	if maxSize > minSize {
		size += rng.Int63n(maxSize - minSize + 1)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for written := int64(0); written < size; {
		n := int64(len(buf))
		if size-written < n {
			n = size - written
		}
		rng.Read(buf[:n])
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		written += n
	}
	return f.Close()
}

func parseYears(s string) (int, int, error) {
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("years %q: want FROM-TO", s)
	}
	f, err1 := strconv.Atoi(from)
	t, err2 := strconv.Atoi(to)
	if err1 != nil || err2 != nil || f > t {
		return 0, 0, fmt.Errorf("years %q: want FROM-TO with FROM <= TO", s)
	}
	return f, t, nil
}
