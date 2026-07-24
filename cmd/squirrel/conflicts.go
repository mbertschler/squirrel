package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newConflictsCmd returns the `squirrel conflicts` command: the question
// half of the contested-freeze pair (#158, F27). It lists the paths this
// node knows are frozen by a peer-sync conflict — divergent edits that
// stopped ping-ponging once the receiver refused to keep minting
// `.squirrel-conflicts/` copies. Both versions stay preserved; the listing
// shows which is live, which is preserved and where, when the freeze
// landed, and which peer it involves. The change half — `conflicts
// resolve` — is the explicit human act that clears a freeze.
func newConflictsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "List unresolved contested paths (frozen peer-sync conflicts)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConflicts(cmd)
		},
	}
	cmd.AddCommand(newConflictsResolveCmd())
	return cmd
}

func runConflicts(cmd *cobra.Command) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	contested, err := s.ListContestedPaths(cmd.Context())
	if err != nil {
		return fmt.Errorf("list contested paths: %w", err)
	}
	out := cmd.OutOrStdout()
	if len(contested) == 0 {
		fmt.Fprintln(out, "no contested paths — nothing frozen")
		return nil
	}
	volumes, err := loadVolumeNames(cmd, s)
	if err != nil {
		return err
	}
	nodes := loadContestedNodeNames(cmd, s, contested)
	return printContested(out, contested, volumes, nodes)
}

// loadContestedNodeNames resolves the peer_node_id of every contested row
// to a name. A dropped or unattributed (zero) id is simply omitted; the
// renderer falls back to "—". Best-effort: a lookup error skips the id
// rather than failing the whole listing.
func loadContestedNodeNames(cmd *cobra.Command, s *store.Store, rows []store.ContestedPath) map[int64]string {
	out := map[int64]string{}
	for _, c := range rows {
		if c.PeerNodeID == 0 {
			continue
		}
		if _, ok := out[c.PeerNodeID]; ok {
			continue
		}
		node, err := s.GetNodeByID(cmd.Context(), c.PeerNodeID)
		if err != nil {
			continue
		}
		out[c.PeerNodeID] = node.Name
	}
	return out
}

func printContested(w io.Writer, rows []store.ContestedPath, volumes, nodes map[int64]string) error {
	fmt.Fprintf(w, "%d contested path(s) — resolve each with `squirrel conflicts resolve <volume> <path>`:\n\n", len(rows))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VOLUME\tPATH\tLIVE\tPRESERVED\tPRESERVED AT\tSINCE\tPEER")
	for _, c := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			volumeLabel(sql.NullInt64{Int64: c.VolumeID, Valid: true}, volumes),
			c.Path,
			shortDigest(c.LiveBlake3),
			shortDigest(c.PreservedBlake3),
			dashIfEmpty(c.PreservedAtPath),
			formatStarted(c.RaisedAtNs),
			contestedPeerLabel(c.PeerNodeID, nodes),
		)
	}
	return tw.Flush()
}

// contestedPeerLabel names the peer a freeze involves, or "—" when
// unattributed (zero id) or the node row has vanished.
func contestedPeerLabel(peerNodeID int64, nodes map[int64]string) string {
	if peerNodeID == 0 {
		return "—"
	}
	if name, ok := nodes[peerNodeID]; ok {
		return name
	}
	return fmt.Sprintf("id=%d", peerNodeID)
}

// shortDigest renders the first 12 hex chars of a digest for a compact
// column, or "—" for an unknown (nil) digest.
func shortDigest(b []byte) string {
	if len(b) == 0 {
		return "—"
	}
	h := hex.EncodeToString(b)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
