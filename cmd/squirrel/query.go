package main

import (
	"database/sql"
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
		fromNode   string
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

			filter, err := resolveOriginFilter(cmd, s, fromNode)
			if err != nil {
				return err
			}

			switch {
			case duplicates:
				if len(args) != 0 {
					return errors.New("--duplicates does not take a positional argument")
				}
				return queryDuplicates(cmd, s, filter)
			case missing:
				if len(args) != 0 {
					return errors.New("--missing does not take a positional argument")
				}
				return queryMissing(cmd, s, filter)
			default:
				if len(args) == 1 {
					return queryArg(cmd, s, args[0], history, filter)
				}
				if filter.active {
					return queryByOrigin(cmd, s, filter)
				}
				return errors.New("query requires <hash>, <path>, --duplicates, --missing, or --from")
			}
		},
	}
	cmd.Flags().BoolVar(&duplicates, "duplicates", false, "list hashes that appear at more than one path")
	cmd.Flags().BoolVar(&missing, "missing", false, "list previously-indexed paths no longer on disk")
	cmd.Flags().BoolVar(&history, "history", false, "when querying a path, also print the full content history at that path")
	cmd.Flags().StringVar(&fromNode, "from", "", "restrict results to rows whose content originates at this node (use the self-node name for locally introduced content)")
	return cmd
}

// originFilter encodes the result of `--from <name>`. active=false means
// no filter; nodeID.Valid==true filters to that node id; nodeID.Valid==false
// (and active==true) means "self / locally introduced" (origin_node_id IS
// NULL).
type originFilter struct {
	active bool
	nodeID sql.NullInt64
}

// matches reports whether the row's origin_node_id passes the filter.
// A non-active filter matches everything.
func (f originFilter) matches(rowOrigin sql.NullInt64) bool {
	if !f.active {
		return true
	}
	if !f.nodeID.Valid {
		return !rowOrigin.Valid
	}
	return rowOrigin.Valid && rowOrigin.Int64 == f.nodeID.Int64
}

// resolveOriginFilter turns the --from <name> argument into an
// originFilter. The self-node's name resolves to "match NULL origin"
// (locally introduced content); any other named node resolves to that
// node's id. An empty name produces an inactive filter.
func resolveOriginFilter(cmd *cobra.Command, s *store.Store, name string) (originFilter, error) {
	if name == "" {
		return originFilter{}, nil
	}
	self, err := s.GetSelfNode(cmd.Context())
	if err != nil {
		return originFilter{}, fmt.Errorf("lookup self node: %w", err)
	}
	if name == self.Name {
		return originFilter{active: true}, nil
	}
	node, err := s.GetNodeByName(cmd.Context(), name)
	if err != nil {
		if store.IsNotFound(err) {
			return originFilter{}, fmt.Errorf("no node named %q (use the self-node name %q for locally introduced content)", name, self.Name)
		}
		return originFilter{}, fmt.Errorf("lookup node %q: %w", name, err)
	}
	return originFilter{active: true, nodeID: sql.NullInt64{Int64: node.ID, Valid: true}}, nil
}

// queryArg disambiguates between a path lookup and a hex digest lookup. A
// 64-char hex string that exists on disk (or contains a path separator) is
// treated as a path; otherwise it is interpreted as a BLAKE3 digest. This
// protects content-addressed workloads where filenames are themselves hex.
// withHistory is only meaningful for path queries — hash lookups already
// list every row for the digest.
func queryArg(cmd *cobra.Command, s *store.Store, arg string, withHistory bool, filter originFilter) error {
	if !looksLikePath(arg) && isHashLike(arg) {
		if withHistory {
			return errors.New("--history applies to path queries, not hash queries (hash queries already list every row)")
		}
		return queryByHash(cmd, s, arg, filter)
	}
	return queryByPath(cmd, s, arg, withHistory, filter)
}

func looksLikePath(arg string) bool {
	if strings.ContainsAny(arg, string(filepath.Separator)+"/") {
		return true
	}
	_, err := os.Stat(arg)
	return err == nil
}

func queryByHash(cmd *cobra.Command, s *store.Store, hexDigest string, filter originFilter) error {
	digest, err := hex.DecodeString(hexDigest)
	if err != nil {
		return fmt.Errorf("decode hash: %w", err)
	}
	rows, err := s.GetByBlake3(cmd.Context(), digest)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var any bool
	for _, r := range rows {
		if !filter.matches(r.File.OriginNodeID) {
			continue
		}
		fmt.Fprintf(out, "%s\t%s\t%d\n", r.File.Status, joinVolumePath(r.Volume.Path, r.File.Path), r.File.SizeBytes)
		any = true
	}
	if !any {
		if filter.active {
			return fmt.Errorf("no rows for blake3 %s matching --from filter", hexDigest)
		}
		return fmt.Errorf("no rows for blake3 %s", hexDigest)
	}
	return nil
}

func queryByPath(cmd *cobra.Command, s *store.Store, arg string, withHistory bool, filter originFilter) error {
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
	if !filter.matches(fv.File.OriginNodeID) {
		return fmt.Errorf("row at %s does not match --from filter", absPath)
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

func queryDuplicates(cmd *cobra.Command, s *store.Store, filter originFilter) error {
	rows, err := s.ListDuplicates(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var lastHex string
	for _, r := range rows {
		if !filter.matches(r.File.OriginNodeID) {
			continue
		}
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

func queryMissing(cmd *cobra.Command, s *store.Store, filter originFilter) error {
	rows, err := s.ListMissing(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, r := range rows {
		if !filter.matches(r.File.OriginNodeID) {
			continue
		}
		fmt.Fprintf(out, "%s\t%s\n", hex.EncodeToString(r.File.Blake3), joinVolumePath(r.Volume.Path, r.File.Path))
	}
	return nil
}

// queryByOrigin lists every present row across volumes whose content
// origin matches the filter — the bare `--from <name>` case with no
// positional, duplicates, or missing flag. The underlying
// ListPresentByOrigin is per-volume so we iterate (today's volume
// counts are small); cross-volume widening is out of scope for #15.
func queryByOrigin(cmd *cobra.Command, s *store.Store, filter originFilter) error {
	vols, err := s.ListVolumes(cmd.Context())
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	out := cmd.OutOrStdout()
	for _, v := range vols {
		for row, err := range s.ListPresentByOrigin(cmd.Context(), v.ID, filter.nodeID) {
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s\t%s\n", hex.EncodeToString(row.Blake3), joinVolumePath(v.Path, row.Path))
		}
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
