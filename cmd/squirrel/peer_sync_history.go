package main

import (
	"database/sql"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newPeerSyncHistoryCmd returns the `squirrel peer-sync history <volume>
// <peer>` subcommand. It prints the append-only watermark transition log
// (peer_sync_state_history) for one (volume, peer) pair, oldest first —
// the forensic surface for SAFETY-AUDIT H6, where the live
// peer_sync_state row only ever shows the current watermark.
func newPeerSyncHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <volume> <peer>",
		Short: "List the watermark transition log for a (volume, peer) pair",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPeerSyncHistory(cmd, args[0], args[1])
		},
	}
}

func runPeerSyncHistory(cmd *cobra.Command, volumeName, peerName string) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	vol, err := s.GetVolumeByName(cmd.Context(), volumeName)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no volume named %q", volumeName)
		}
		return fmt.Errorf("lookup volume %q: %w", volumeName, err)
	}
	peer, err := s.GetNodeByName(cmd.Context(), peerName)
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("no node named %q", peerName)
		}
		return fmt.Errorf("lookup node %q: %w", peerName, err)
	}

	history, err := s.ListPeerSyncStateHistory(cmd.Context(), vol.ID, peer.ID)
	if err != nil {
		return err
	}
	return printPeerSyncHistory(cmd.OutOrStdout(), history)
}

func printPeerSyncHistory(out io.Writer, history []store.PeerSyncStateHistory) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tLAST_SHARED_RUN_ID")
	for _, h := range history {
		fmt.Fprintf(tw, "%s\t%s\n",
			time.Unix(0, h.AtNs).UTC().Format(time.RFC3339),
			watermarkLabel(h.LastSharedRunID))
	}
	return tw.Flush()
}

// watermarkLabel renders the nullable watermark for the listing: the
// decimal run id when set, or "—" for the first-contact NULL state.
func watermarkLabel(v sql.NullInt64) string {
	if !v.Valid {
		return "—"
	}
	return fmt.Sprintf("%d", v.Int64)
}
