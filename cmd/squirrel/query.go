package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newQueryCmd returns the `squirrel query` cobra command which looks up
// rows by hex digest, by path, or lists --duplicates / --missing.
func newQueryCmd() *cobra.Command {
	var (
		duplicates bool
		missing    bool
	)
	cmd := &cobra.Command{
		Use:   "query [<hash-or-path>]",
		Short: "Look up the index by hash, path, or list duplicates/missing",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()

			switch {
			case duplicates:
				if len(args) != 0 {
					return errors.New("--duplicates does not take a positional argument")
				}
				return queryDuplicates(cmd, s)
			case missing:
				if len(args) != 0 {
					return errors.New("--missing does not take a positional argument")
				}
				return queryMissing(cmd, s)
			default:
				if len(args) != 1 {
					return errors.New("query requires <hash>, <path>, --duplicates, or --missing")
				}
				return queryArg(cmd, s, args[0])
			}
		},
	}
	cmd.Flags().BoolVar(&duplicates, "duplicates", false, "list hashes that appear at more than one path")
	cmd.Flags().BoolVar(&missing, "missing", false, "list previously-indexed paths no longer on disk")
	return cmd
}

// queryArg disambiguates between a path lookup and a hex digest lookup. A
// 64-char hex string that exists on disk (or contains a path separator) is
// treated as a path; otherwise it is interpreted as a BLAKE3 digest. This
// protects content-addressed workloads where filenames are themselves hex.
func queryArg(cmd *cobra.Command, s *store.Store, arg string) error {
	if !looksLikePath(arg) && isHashLike(arg) {
		return queryByHash(cmd, s, arg)
	}
	return queryByPath(cmd, s, arg)
}

func looksLikePath(arg string) bool {
	if strings.ContainsAny(arg, string(filepath.Separator)+"/") {
		return true
	}
	_, err := os.Stat(arg)
	return err == nil
}

func queryByHash(cmd *cobra.Command, s *store.Store, hexDigest string) error {
	digest, err := hex.DecodeString(hexDigest)
	if err != nil {
		return fmt.Errorf("decode hash: %w", err)
	}
	rows, err := s.GetByBlake3(cmd.Context(), digest)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no rows for blake3 %s", hexDigest)
	}
	out := cmd.OutOrStdout()
	for _, r := range rows {
		fmt.Fprintf(out, "%s\t%s\t%d\n", r.Status, joinRootPath(r.Root, r.Path), r.SizeBytes)
	}
	return nil
}

func queryByPath(cmd *cobra.Command, s *store.Store, arg string) error {
	absPath, err := filepath.Abs(arg)
	if err != nil {
		return err
	}
	row, err := s.GetByAbsolutePath(cmd.Context(), absPath)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no row for path %s (not under any indexed root)", absPath)
		}
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "root:        %s\n", row.Root)
	fmt.Fprintf(out, "path:        %s\n", row.Path)
	fmt.Fprintf(out, "full:        %s\n", joinRootPath(row.Root, row.Path))
	fmt.Fprintf(out, "blake3:      %s\n", hex.EncodeToString(row.Blake3))
	fmt.Fprintf(out, "size_bytes:  %d\n", row.SizeBytes)
	fmt.Fprintf(out, "mtime_ns:    %d\n", row.MtimeNs)
	fmt.Fprintf(out, "status:      %s\n", row.Status)
	fmt.Fprintf(out, "last_seen:   %d\n", row.LastSeenAt)
	fmt.Fprintf(out, "indexed_at:  %d\n", row.IndexedAt)
	return nil
}

func queryDuplicates(cmd *cobra.Command, s *store.Store) error {
	rows, err := s.ListDuplicates(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var lastHex string
	for _, r := range rows {
		h := hex.EncodeToString(r.Blake3)
		if h != lastHex {
			if lastHex != "" {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "%s\n", h)
			lastHex = h
		}
		fmt.Fprintf(out, "  %s\n", joinRootPath(r.Root, r.Path))
	}
	return nil
}

func queryMissing(cmd *cobra.Command, s *store.Store) error {
	rows, err := s.ListMissing(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, r := range rows {
		fmt.Fprintf(out, "%s\t%s\n", hex.EncodeToString(r.Blake3), joinRootPath(r.Root, r.Path))
	}
	return nil
}

// joinRootPath reconstructs an absolute filesystem path from a stored
// (root, relative) pair. Returns the root itself when rel is empty or ".".
func joinRootPath(root, rel string) string {
	if rel == "" || rel == "." {
		return root
	}
	return filepath.Join(root, rel)
}

// isHashLike reports whether s is a 64-character hex string (the textual form
// of a BLAKE3-256 digest). The decision to *interpret* s as a hash also
// requires that no file by that name exists on disk; see queryArg.
func isHashLike(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
