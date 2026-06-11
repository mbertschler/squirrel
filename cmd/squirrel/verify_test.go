package main

import (
	"strings"
	"testing"
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
