package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/sync"
)

func TestVerifyUnknownDestination(t *testing.T) {
	fx := writeSyncFixture(t)
	_, err := runCLIExpectErr(t, "verify", "nope", "--config", fx.configPath)
	if !strings.Contains(err.Error(), `unknown destination "nope"`) {
		t.Fatalf("err = %v, want unknown destination", err)
	}
}

// TestVerifyNativeMirrorNeedsNoRclone: a local mirror is verifiable, and a
// pass over one with nothing recorded yet runs without rclone.
func TestVerifyNativeMirrorNeedsNoRclone(t *testing.T) {
	fx := writeSyncFixture(t)
	t.Setenv("PATH", t.TempDir())
	out := runCLI(t, "verify", "scratch", "--config", fx.configPath)
	if !strings.Contains(out, "verify scratch: nothing recorded to verify") {
		t.Fatalf("output = %q, want the empty summary", out)
	}
}

// TestVerifyTargetNamesRefusesRcloneMirrors: an rclone mirror records
// nothing to re-check, so naming one is refused, and a config holding only
// such mirrors has no verify targets.
func TestVerifyTargetNamesRefusesRcloneMirrors(t *testing.T) {
	cfg := &config.Config{Path: "squirrel.toml", Destinations: map[string]*config.Destination{
		"cloudbox": {Name: "cloudbox", Type: "sftp", Layout: config.LayoutMirror, Crypt: &config.Crypt{Password: "x"}},
	}}
	if _, err := verifyTargetNames(cfg, "cloudbox"); err == nil || !strings.Contains(err.Error(), "rclone mirror") {
		t.Fatalf("err = %v, want an rclone-mirror refusal", err)
	}
	if _, err := verifyTargetNames(cfg, ""); err == nil || !strings.Contains(err.Error(), "no verifiable destinations") {
		t.Fatalf("err = %v, want no-destinations refusal", err)
	}
}

// TestPrintVerifyReportErrorSuppressesSummary: when the pass errored
// before producing object counts, the report shows only the error on
// stderr — never the misleading "no recorded objects" summary.
func TestPrintVerifyReportErrorSuppressesSummary(t *testing.T) {
	var out, errOut strings.Builder
	printVerifyReport(&out, &errOut, sync.RemoteVerifyReport{Destination: "offsite"}, errors.New("rclone exploded"))
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on an error run", out.String())
	}
	if !strings.Contains(errOut.String(), "rclone exploded") {
		t.Fatalf("stderr = %q, want the error surfaced", errOut.String())
	}
}

// TestPrintVerifyReportCleanEmptyShowsSummary: a clean run with no
// recorded objects still prints its summary line.
func TestPrintVerifyReportCleanEmptyShowsSummary(t *testing.T) {
	var out, errOut strings.Builder
	printVerifyReport(&out, &errOut, sync.RemoteVerifyReport{Destination: "offsite"}, nil)
	if !strings.Contains(out.String(), "nothing recorded to verify") {
		t.Fatalf("stdout = %q, want the no-objects summary on a clean run", out.String())
	}
}
