// Package sync_test pins the handler/hook boundary from the outside:
// VerifyResult's verified flag is unexported, so the strongest claim any
// code outside the sync package — the hook mechanism included — can
// construct is an unverified result. Hooks stay exit-code-only
// (hook.Outcome), and durability reporting stays with the curated
// handlers by construction.
package sync_test

import (
	"testing"

	"github.com/mbertschler/squirrel/sync"
)

func TestVerifyResultUnmintableOutsidePackage(t *testing.T) {
	v := sync.VerifyResult{
		Method:     sync.VerifyMethodBlake3,
		SnapshotID: "abc",
		Files:      100,
		Bytes:      1 << 20,
	}
	if v.Verified() {
		t.Fatalf("a VerifyResult built outside the sync package must report unverified")
	}
}
