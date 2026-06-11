package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbertschler/squirrel/sync"
)

func TestVerifyUnknownDestination(t *testing.T) {
	fx := writeSyncFixture(t)
	_, err := runCLIExpectErr(t, "verify", "nope", "--config", fx.configPath)
	if !strings.Contains(err.Error(), `unknown destination "nope"`) {
		t.Fatalf("err = %v, want unknown destination", err)
	}
}

func TestVerifyRefusesMirrorDestination(t *testing.T) {
	fx := writeSyncFixture(t)
	_, err := runCLIExpectErr(t, "verify", "scratch", "--config", fx.configPath)
	if !strings.Contains(err.Error(), "content-addressed") {
		t.Fatalf("err = %v, want content-addressed refusal", err)
	}
}

func TestVerifyNoContentAddressedDestinations(t *testing.T) {
	fx := writeSyncFixture(t)
	_, err := runCLIExpectErr(t, "verify", "--config", fx.configPath)
	if !strings.Contains(err.Error(), "no content-addressed destinations") {
		t.Fatalf("err = %v, want no-destinations refusal", err)
	}
}

// TestPrintVerifyReportErrorSuppressesSummary: when the pass errored
// before producing object counts, the report shows only the error on
// stderr — never the misleading "no recorded objects" summary.
func TestPrintVerifyReportErrorSuppressesSummary(t *testing.T) {
	var out, errOut strings.Builder
	printVerifyReport(&out, &errOut, sync.RemoteVerifyReport{Destination: "offsite"}, errors.New("rclone exploded"))
	if strings.Contains(out.String(), "no recorded objects") || out.Len() != 0 {
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
	if !strings.Contains(out.String(), "no recorded objects") {
		t.Fatalf("stdout = %q, want the no-objects summary on a clean run", out.String())
	}
}
