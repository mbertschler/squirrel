package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Kopia is a configured kopia wrapper. Like rclone, kopia is treated as
// an opaque child process: squirrel owns the argv, points every
// invocation at a destination-scoped config file under ConfigDir
// (kopia-<destination>.config, sibling to rclone.conf), and hands the
// repository password to the child via KOPIA_PASSWORD in its
// environment — the password stays out of argv, logs, and error
// strings. The user's own kopia configuration is left untouched.
type Kopia struct {
	Binary    string
	ConfigDir string
}

// FindKopia locates the kopia binary on PATH and roots the wrapper's
// destination config files at configDir.
func FindKopia(configDir string) (*Kopia, error) {
	bin, err := exec.LookPath("kopia")
	if err != nil {
		return nil, fmt.Errorf("kopia not found on PATH (required for kopia destinations): %w", err)
	}
	return &Kopia{Binary: bin, ConfigDir: configDir}, nil
}

// configFile returns the per-destination kopia config file path. One
// file per destination because each kopia destination is its own
// repository.
func (k *Kopia) configFile(destName string) string {
	return filepath.Join(k.ConfigDir, "kopia-"+destName+".config")
}

// environWithout returns the process environment with every entry for
// key removed, so a single appended override is the one value the child
// sees regardless of what the parent shell exported.
func environWithout(key string) []string {
	env := os.Environ()
	out := env[:0]
	prefix := key + "="
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// run executes one kopia subcommand against the given config file,
// returning captured stdout. Stderr is folded into the error on failure
// for diagnostics.
func (k *Kopia) run(ctx context.Context, cfgFile, password string, args ...string) ([]byte, error) {
	full := append(append([]string(nil), args...), "--config-file", cfgFile)
	cmd := exec.CommandContext(ctx, k.Binary, full...)
	cmd.Env = append(environWithout("KOPIA_PASSWORD"), "KOPIA_PASSWORD="+password)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		verb := strings.Join(args[:min(2, len(args))], " ")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("kopia %s: %w: %s", verb, err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("kopia %s: %w", verb, err)
	}
	return stdout.Bytes(), nil
}

// ensureRepository connects the destination-scoped config file to the
// filesystem repository at repoPath, creating the repository when
// connect reports nothing usable there (first use). Connect runs on
// every push so a repository path changed in squirrel's config is
// re-pointed rather than silently snapshotting into the old one.
func (k *Kopia) ensureRepository(ctx context.Context, cfgFile, password, repoPath string) error {
	_, connectErr := k.run(ctx, cfgFile, password, "repository", "connect", "filesystem", "--path", repoPath)
	if connectErr == nil {
		return nil
	}
	if _, createErr := k.run(ctx, cfgFile, password, "repository", "create", "filesystem", "--path", repoPath); createErr != nil {
		return fmt.Errorf("kopia repository at %s: connect failed (%w); create failed: %w", repoPath, connectErr, createErr)
	}
	return nil
}

// kopiaSnapshot is the subset of the manifest `kopia snapshot create
// --json` prints that squirrel reports on: the manifest id plus the
// root directory summary's counts. Field names follow kopia's JSON
// casing exactly.
type kopiaSnapshot struct {
	ID        string `json:"id"`
	RootEntry struct {
		Summary struct {
			Size          int64 `json:"size"`
			Files         int64 `json:"files"`
			FatalErrors   int64 `json:"numFailed"`
			IgnoredErrors int64 `json:"numIgnoredErrors"`
		} `json:"summ"`
	} `json:"rootEntry"`
}

// snapshotCreate snapshots sourcePath into the connected repository and
// parses the resulting manifest.
func (k *Kopia) snapshotCreate(ctx context.Context, cfgFile, password, sourcePath string) (kopiaSnapshot, error) {
	out, err := k.run(ctx, cfgFile, password, "snapshot", "create", sourcePath, "--json")
	if err != nil {
		return kopiaSnapshot{}, err
	}
	var snap kopiaSnapshot
	if err := json.Unmarshal(bytes.TrimSpace(out), &snap); err != nil {
		return kopiaSnapshot{}, fmt.Errorf("parse kopia snapshot manifest: %w", err)
	}
	if snap.ID == "" {
		return kopiaSnapshot{}, fmt.Errorf("kopia snapshot manifest carries no id: %q", bytes.TrimSpace(out))
	}
	return snap, nil
}

// snapshotVerify runs kopia's own consistency check, scoped to the
// given snapshot manifest id.
func (k *Kopia) snapshotVerify(ctx context.Context, cfgFile, password, snapshotID string) error {
	_, err := k.run(ctx, cfgFile, password, "snapshot", "verify", snapshotID)
	return err
}

// kopiaHandler pushes a volume into a kopia repository: connect (or
// first-use create), `kopia snapshot create <volume path>`, then
// `kopia snapshot verify`. The runs row matches the other sync targets
// (kind='sync', destination=name); shallow is always false because
// kopia verifies its own content hashes.
type kopiaHandler struct {
	store *store.Store
	kopia *Kopia
	vol   *config.Volume
	dest  *config.Destination
}

func (h *kopiaHandler) TargetName() string { return h.dest.Name }

func (h *kopiaHandler) Push(ctx context.Context, opts Options) (Report, error) {
	rep := Report{Volume: h.vol.Name, Destination: h.dest.Name}
	// Stamped up front so output renderers key kopia formatting off the
	// method even when the push fails before a snapshot exists.
	rep.Verification.Method = VerifyMethodKopia
	if opts.DryRun {
		return rep, fmt.Errorf("destination %q: kopia has no dry-run mode — run without --dry-run", h.dest.Name)
	}
	volID, err := requireIndexedVolume(ctx, h.store, h.vol)
	if err != nil {
		return rep, err
	}
	runID, err := beginSyncRunGuarded(ctx, h.store, false, store.SyncRunSpec{
		VolumeID:    volID,
		Destination: h.dest.Name,
	}, h.vol.Name)
	if err != nil {
		return rep, err
	}
	rep.RunID = runID
	if opts.OnRunID != nil {
		opts.OnRunID(runID)
	}

	err = h.snapshotAndVerify(ctx, &rep)
	h.finishRun(ctx, &rep, err)
	// Local index snapshot only: the repository is kopia's own format,
	// so the rclone ride-along stays out of it (dest=nil, mirroring the
	// peer flow).
	opts.Snapshot.afterSync(ctx, &rep, h.vol, nil)
	return rep, err
}

func (h *kopiaHandler) sealed() {}

// snapshotAndVerify drives the kopia binary and derives rep.Status and
// rep.Verification. Status starts failed and is promoted: success for a
// clean verified snapshot, partial when the snapshot landed with
// per-file errors kopia tolerated. Verified is reserved for the clean
// path — a snapshot with skipped files is durable but incomplete.
func (h *kopiaHandler) snapshotAndVerify(ctx context.Context, rep *Report) error {
	rep.Status = store.RunStatusFailed
	cfgFile := h.kopia.configFile(h.dest.Name)
	password := h.dest.Params["password"]
	if err := h.kopia.ensureRepository(ctx, cfgFile, password, h.dest.Root); err != nil {
		return err
	}
	snap, err := h.kopia.snapshotCreate(ctx, cfgFile, password, h.vol.Path)
	if err != nil {
		return err
	}
	summ := snap.RootEntry.Summary
	rep.Verification = VerifyResult{
		Method:     VerifyMethodKopia,
		SnapshotID: snap.ID,
		Files:      summ.Files,
		Bytes:      summ.Size,
	}
	if err := h.kopia.snapshotVerify(ctx, cfgFile, password, snap.ID); err != nil {
		return err
	}
	if summ.FatalErrors+summ.IgnoredErrors > 0 {
		rep.Status = store.RunStatusPartial
		return nil
	}
	rep.Status = store.RunStatusSuccess
	rep.Verification.verified = true
	return nil
}

// finishRun writes the kopia run's terminal state, mirroring the shared
// finishRun's contract: a FinishRun failure lands on rep.FinishErr so
// the caller can surface it next to the push outcome.
func (h *kopiaHandler) finishRun(ctx context.Context, rep *Report, runErr error) {
	if rep.RunID == 0 {
		return
	}
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	if err := h.store.FinishRun(ctx, rep.RunID, rep.Status, errMsg, rep.Verification.Files); err != nil {
		rep.FinishErr = err
	}
}
