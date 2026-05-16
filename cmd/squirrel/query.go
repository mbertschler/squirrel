package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newQueryCmd returns the `squirrel query` cobra command which looks up
// rows by hex digest, by path, or lists --duplicates / --missing.
func newQueryCmd() *cobra.Command {
	var (
		duplicates bool
		missing    bool
		history    bool
	)
	cmd := &cobra.Command{
		Use:   "query [<hash-or-path>]",
		Short: "Look up the index by hash, path, or list duplicates/missing",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := tryLoadConfig(cmd)
			if err != nil {
				return err
			}
			s, err := openStore(cmd, cfg)
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
				return queryArg(cmd, s, args[0], history)
			}
		},
	}
	cmd.Flags().BoolVar(&duplicates, "duplicates", false, "list hashes that appear at more than one path")
	cmd.Flags().BoolVar(&missing, "missing", false, "list previously-indexed paths no longer on disk")
	cmd.Flags().BoolVar(&history, "history", false, "when querying a path, also print the full content history at that path")
	return cmd
}

// queryArg disambiguates between a path lookup and a hex digest lookup. A
// 64-char hex string that exists on disk (or contains a path separator) is
// treated as a path; otherwise it is interpreted as a BLAKE3 digest. This
// protects content-addressed workloads where filenames are themselves hex.
// withHistory is only meaningful for path queries — hash lookups already
// list every row for the digest.
func queryArg(cmd *cobra.Command, s *store.Store, arg string, withHistory bool) error {
	if !looksLikePath(arg) && isHashLike(arg) {
		if withHistory {
			return errors.New("--history applies to path queries, not hash queries (hash queries already list every row)")
		}
		return queryByHash(cmd, s, arg)
	}
	return queryByPath(cmd, s, arg, withHistory)
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
		fmt.Fprintf(out, "%s\t%s\t%d\n", r.File.Status, joinVolumePath(r.Volume.Path, r.File.Path), r.File.SizeBytes)
	}
	return nil
}

func queryByPath(cmd *cobra.Command, s *store.Store, arg string, withHistory bool) error {
	absPath, err := filepath.Abs(arg)
	if err != nil {
		return err
	}
	fv, err := s.GetByAbsolutePath(cmd.Context(), absPath)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no row for path %s (not under any indexed volume)", absPath)
		}
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "volume:        %s\n", fv.Volume.Name)
	fmt.Fprintf(out, "volume_path:   %s\n", fv.Volume.Path)
	fmt.Fprintf(out, "path:          %s\n", fv.File.Path)
	fmt.Fprintf(out, "full:          %s\n", joinVolumePath(fv.Volume.Path, fv.File.Path))
	fmt.Fprintf(out, "blake3:        %s\n", hex.EncodeToString(fv.File.Blake3))
	fmt.Fprintf(out, "size_bytes:    %d\n", fv.File.SizeBytes)
	fmt.Fprintf(out, "mtime_ns:      %d\n", fv.File.MtimeNs)
	fmt.Fprintf(out, "status:        %s\n", fv.File.Status)
	fmt.Fprintf(out, "first_seen_run: %d\n", fv.File.FirstSeenRunID)
	fmt.Fprintf(out, "last_seen_run:  %d\n", fv.File.LastSeenRunID)
	fmt.Fprintf(out, "indexed_at_ns: %d\n", fv.File.IndexedAtNs)

	if withHistory {
		if err := printPathHistory(cmd, s, fv.Volume.ID, fv.File.Path); err != nil {
			return err
		}
	}
	return nil
}

// printPathHistory appends a table of every row in the path's content
// history — the live row from the block above plus any superseded
// predecessors. Useful for inspecting "what content has lived at this
// path?" without dropping into SQL.
func printPathHistory(cmd *cobra.Command, s *store.Store, volumeID int64, relPath string) error {
	history, err := s.ListHistoryByPath(cmd.Context(), volumeID, relPath)
	if err != nil {
		return fmt.Errorf("list history: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "history:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  STATUS\tBLAKE3\tSIZE\tFIRST_SEEN_RUN\tLAST_SEEN_RUN\tINDEXED_AT_NS")
	for _, r := range history {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\t%d\n",
			r.Status, hex.EncodeToString(r.Blake3), r.SizeBytes,
			r.FirstSeenRunID, r.LastSeenRunID, r.IndexedAtNs)
	}
	return tw.Flush()
}

func queryDuplicates(cmd *cobra.Command, s *store.Store) error {
	rows, err := s.ListDuplicates(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var lastHex string
	for _, r := range rows {
		h := hex.EncodeToString(r.File.Blake3)
		if h != lastHex {
			if lastHex != "" {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "%s\n", h)
			lastHex = h
		}
		fmt.Fprintf(out, "  %s\n", joinVolumePath(r.Volume.Path, r.File.Path))
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
		fmt.Fprintf(out, "%s\t%s\n", hex.EncodeToString(r.File.Blake3), joinVolumePath(r.Volume.Path, r.File.Path))
	}
	return nil
}

// joinVolumePath reconstructs an absolute filesystem path from a stored
// (volume.path, file.path) pair. Returns volumePath when rel is empty or ".".
func joinVolumePath(volumePath, rel string) string {
	if rel == "" || rel == "." {
		return volumePath
	}
	return filepath.Join(volumePath, rel)
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
