package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
)

// Per-check status markers. statusFail is fatal (the command exits
// non-zero); statusWarn is advisory (surfaced, but the config is usable);
// statusOK is a clean line.
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "FAIL"
)

// newConfigCheckCmd returns `squirrel config check`: parse + resolve the
// whole config (env vars included), stat volume paths and node byte-paths,
// validate offload policies against target capabilities, and print an
// affirmative summary (F4). It is strictly read-only — it never creates a
// path, cert, or database — per the "CLI is for change and for questions"
// principle: this is a question.
func newConfigCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Parse and resolve the config, stat its paths, and report what it declares",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigCheck(cmd)
		},
	}
}

// checkTally accumulates fatal/advisory counts across the report sections.
type checkTally struct {
	fails int
	warns int
}

func (t *checkTally) add(status string) {
	switch status {
	case statusFail:
		t.fails++
	case statusWarn:
		t.warns++
	}
}

func runConfigCheck(cmd *cobra.Command) error {
	// requireConfig both loads and resolves: a parse error, an unknown
	// field, or an unset { env = "VAR" } secret surfaces here as the
	// check's failure, which is exactly the "did everything resolve?"
	// question this command answers.
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "config: %s\n\n", cfg.Path)

	var tally checkTally
	checkVolumes(cfg, out, &tally)
	checkDestinations(cfg, out)
	checkNodes(cfg, out, &tally)
	checkOffloadPolicies(cfg, out, &tally)

	return printSummary(out, cfg, tally)
}

// printCheckLine renders one aligned status line: "  ok    name  detail".
func printCheckLine(out io.Writer, status, name, detail string) {
	if detail == "" {
		fmt.Fprintf(out, "  %-4s  %s\n", status, name)
		return
	}
	fmt.Fprintf(out, "  %-4s  %s  %s\n", status, name, detail)
}

func checkVolumes(cfg *config.Config, out io.Writer, tally *checkTally) {
	fmt.Fprintf(out, "volumes (%d)\n", len(cfg.Volumes))
	for _, name := range sortedKeys(cfg.Volumes) {
		vol := cfg.Volumes[name]
		status, detail := statVolumePath(vol.Path)
		tally.add(status)
		printCheckLine(out, status, name, joinDetail(vol.Path, detail))
	}
}

func checkDestinations(cfg *config.Config, out io.Writer) {
	fmt.Fprintf(out, "destinations (%d)\n", len(cfg.Destinations))
	for _, name := range sortedKeys(cfg.Destinations) {
		d := cfg.Destinations[name]
		parts := []string{d.Type, d.Layout}
		if d.Crypt != nil {
			parts = append(parts, "crypt")
		}
		// Destinations resolved (Load succeeded), so they are all ok here;
		// their remote reachability is a sync-time concern, not something a
		// read-only check probes.
		printCheckLine(out, statusOK, name, strings.Join(parts, " "))
	}
}

func checkNodes(cfg *config.Config, out io.Writer, tally *checkTally) {
	fmt.Fprintf(out, "nodes (%d)\n", len(cfg.Nodes))
	for _, name := range sortedKeys(cfg.Nodes) {
		n := cfg.Nodes[name]
		status, detail := statNodeBytePath(n.Path)
		tally.add(status)
		endpoint := n.Endpoint.String()
		printCheckLine(out, status, name, joinDetail(endpoint, "byte-path "+n.Path+detail))
	}
}

func checkOffloadPolicies(cfg *config.Config, out io.Writer, tally *checkTally) {
	names := offloadVolumeNames(cfg)
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(out, "offload policies (%d)\n", len(names))
	for _, name := range names {
		for _, target := range cfg.Volumes[name].OffloadRequires {
			status, detail := offloadTargetStatus(cfg, target)
			tally.add(status)
			printCheckLine(out, status, name+" → "+target, detail)
		}
	}
}

// offloadTargetStatus classifies one offload_requires target. A destination
// that can never yield offload-grade evidence (a mirror-layout crypt
// destination, F21) is a fatal misconfiguration — the gate would wait
// forever. A peer node yields peer-blake3 evidence directly. A name that is
// neither is an external target whose evidence must arrive relayed via a
// peer; that is legitimate, so it is reported without alarm.
func offloadTargetStatus(cfg *config.Config, target string) (status, detail string) {
	if d, ok := cfg.Destinations[target]; ok {
		if capable, reason := d.CanEverGateOffload(); !capable {
			return statusFail, "never yields offload evidence: " + reason
		}
		return statusOK, "destination"
	}
	if _, ok := cfg.Nodes[target]; ok {
		return statusOK, "peer node (peer-blake3 evidence)"
	}
	return statusOK, "external target — evidence must arrive relayed via a peer"
}

func printSummary(out io.Writer, cfg *config.Config, tally checkTally) error {
	fmt.Fprintln(out)
	counts := fmt.Sprintf("%d volumes, %d destinations, %d nodes",
		len(cfg.Volumes), len(cfg.Destinations), len(cfg.Nodes))
	switch {
	case tally.fails > 0:
		fmt.Fprintf(out, "%s — %s, %s\n", counts,
			plural(tally.fails, "problem"), plural(tally.warns, "warning"))
		return fmt.Errorf("config check found %d problem(s)", tally.fails)
	case tally.warns > 0:
		fmt.Fprintf(out, "%s — resolvable, %s\n", counts, plural(tally.warns, "warning"))
		return nil
	default:
		fmt.Fprintf(out, "%s — all resolvable\n", counts)
		return nil
	}
}

// statVolumePath stats a local volume root. A missing path or non-directory
// is fatal; an empty directory is advisory (F8: a typo'd or unmounted path
// looks identical to a genuinely new volume, so it must be flagged, not
// hard-failed).
func statVolumePath(path string) (status, detail string) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return statusFail, "does not exist"
	case err != nil:
		return statusFail, err.Error()
	case !info.IsDir():
		return statusFail, "not a directory"
	}
	empty, err := dirIsEmpty(path)
	if err != nil {
		return statusWarn, "unreadable: " + err.Error()
	}
	if empty {
		return statusWarn, "empty directory — new volume or wrong mount?"
	}
	return statusOK, ""
}

// statNodeBytePath stats a node's rclone byte-path (F34). An rclone remote
// spec ("remote:path") is not a local path, so it is reported as unchecked
// rather than statted. A missing local mount is advisory: the share may
// legitimately be down when the check runs, but a silent absence is exactly
// what turns into undiagnosable transfer errors, so it is surfaced. The
// returned detail is a suffix appended after the path.
func statNodeBytePath(path string) (status, detail string) {
	if isRcloneRemoteSpec(path) {
		return statusOK, " (rclone remote spec — not checked)"
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return statusWarn, " (does not exist — mount not up?)"
	case err != nil:
		return statusWarn, " (" + err.Error() + ")"
	case !info.IsDir():
		return statusWarn, " (not a directory)"
	}
	return statusOK, ""
}

// isRcloneRemoteSpec reports whether p is an rclone "remote:path" reference
// rather than a filesystem path. An absolute path (leading /) is always a
// filesystem path; otherwise a leading "name:" segment marks a remote.
func isRcloneRemoteSpec(p string) bool {
	if strings.HasPrefix(p, "/") {
		return false
	}
	i := strings.IndexByte(p, ':')
	return i > 0
}

// dirIsEmpty reports whether dir contains no entries, reading just one name
// rather than the whole listing.
func dirIsEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// offloadVolumeNames returns the sorted names of volumes that declare an
// offload policy.
func offloadVolumeNames(cfg *config.Config) []string {
	var names []string
	for name, vol := range cfg.Volumes {
		if len(vol.OffloadRequires) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// joinDetail appends a parenthetical/extra detail to a base string when the
// detail is non-empty.
func joinDetail(base, detail string) string {
	if detail == "" {
		return base
	}
	return base + "  " + detail
}

// plural renders "N thing" / "N things" for a count.
func plural(n int, thing string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, thing)
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// sortedKeys returns the map keys sorted, for stable report ordering.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
