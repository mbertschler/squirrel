package sync

import (
	"testing"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

func TestHandlerForDispatch(t *testing.T) {
	vol := &config.Volume{Name: "pics", Path: "/tmp/pics"}
	bucket := &config.Destination{Name: "offsite", Type: "s3", Root: "/data"}
	usb := &config.Destination{Name: "usb", Type: "local", Root: "/tmp/dst"}
	box := &config.Destination{Name: "box", Type: "sftp", Root: "/data"}
	cryptBox := &config.Destination{Name: "box", Type: "sftp", Root: "/data", Crypt: &config.Crypt{Password: "pw"}}
	node := &config.Node{Name: "nas"}
	tools := Tools{Rclone: &Rclone{Binary: "rclone"}}

	h, err := HandlerFor(nil, tools, Pair{Volume: vol, Destination: bucket})
	if err != nil {
		t.Fatalf("bucket pair: %v", err)
	}
	if _, ok := h.(*rcloneHandler); !ok || h.TargetName() != "offsite" {
		t.Fatalf("bucket pair resolved to %T (%q), want *rcloneHandler offsite", h, h.TargetName())
	}

	// A local mirror is native, and needs no rclone wrapper.
	h, err = HandlerFor(nil, Tools{}, Pair{Volume: vol, Destination: usb})
	if err != nil {
		t.Fatalf("local mirror pair: %v", err)
	}
	if _, ok := h.(*mirrorHandler); !ok || h.TargetName() != "usb" {
		t.Fatalf("local mirror pair resolved to %T (%q), want *mirrorHandler usb", h, h.TargetName())
	}

	// So is a plain sftp mirror; an encrypted one stays on rclone.
	h, err = HandlerFor(nil, Tools{}, Pair{Volume: vol, Destination: box})
	if err != nil {
		t.Fatalf("sftp mirror pair: %v", err)
	}
	if _, ok := h.(*mirrorHandler); !ok {
		t.Fatalf("sftp mirror pair resolved to %T, want *mirrorHandler", h)
	}
	h, err = HandlerFor(nil, tools, Pair{Volume: vol, Destination: cryptBox})
	if err != nil {
		t.Fatalf("crypt sftp mirror pair: %v", err)
	}
	if _, ok := h.(*rcloneHandler); !ok {
		t.Fatalf("crypt sftp mirror pair resolved to %T, want *rcloneHandler", h)
	}

	h, err = HandlerFor(nil, tools, Pair{Volume: vol, Node: node})
	if err != nil {
		t.Fatalf("node pair: %v", err)
	}
	if _, ok := h.(*peerHandler); !ok || h.TargetName() != "nas" {
		t.Fatalf("node pair resolved to %T (%q), want *peerHandler nas", h, h.TargetName())
	}

	ca := &config.Destination{Name: "offsite", Type: "sftp", Root: "/data", Layout: config.LayoutContentAddressed}
	h, err = HandlerFor(nil, tools, Pair{Volume: vol, Destination: ca})
	if err != nil {
		t.Fatalf("content-addressed pair: %v", err)
	}
	if _, ok := h.(*contentAddressedHandler); !ok || h.TargetName() != "offsite" {
		t.Fatalf("content-addressed pair resolved to %T (%q), want *contentAddressedHandler offsite", h, h.TargetName())
	}

	if _, err := HandlerFor(nil, tools, Pair{Volume: vol}); err == nil {
		t.Fatalf("expected error for pair without a target")
	}
	if _, err := HandlerFor(nil, Tools{}, Pair{Volume: vol, Destination: bucket}); err == nil {
		t.Fatalf("expected error for bucket pair without an rclone wrapper")
	}
}

func TestRcloneVerification(t *testing.T) {
	plain := &config.Destination{Name: "d", Type: "sftp"}
	crypt := &config.Destination{Name: "d", Type: "sftp", Crypt: &config.Crypt{Password: "x"}}
	cases := []struct {
		name         string
		dest         *config.Destination
		opts         Options
		status       string
		hashFallback bool
		wantVerified bool
		wantMethod   string
	}{
		{"checksum success", plain, Options{}, store.RunStatusSuccess, false, true, VerifyMethodChecksum},
		{"checksum partial", plain, Options{}, store.RunStatusPartial, false, false, VerifyMethodChecksum},
		{"shallow success", plain, Options{Shallow: true}, store.RunStatusSuccess, false, false, VerifyMethodSizeMtime},
		{"crypt forces shallow", crypt, Options{}, store.RunStatusSuccess, false, false, VerifyMethodSizeMtime},
		// rclone exited 0 with the integrity flags set, but reported the
		// no-common-hash fallback: the copy was compared by size, so the
		// result must be size+mtime and unverified.
		{"hash fallback downgrades", plain, Options{}, store.RunStatusSuccess, true, false, VerifyMethodSizeMtime},
	}
	for _, c := range cases {
		rep := &Report{Status: c.status}
		rep.RcloneResult = RunResult{Transferred: 2, Checked: 3, Bytes: 42, HashFallback: c.hashFallback}
		v := rcloneVerification(c.dest, c.opts, rep)
		if v.Verified() != c.wantVerified || v.Method != c.wantMethod {
			t.Errorf("%s: verified=%t method=%q, want %t %q", c.name, v.Verified(), v.Method, c.wantVerified, c.wantMethod)
		}
		if v.Files != 5 || v.Bytes != 42 {
			t.Errorf("%s: files=%d bytes=%d, want 5 42", c.name, v.Files, v.Bytes)
		}
	}
}

func TestPeerVerification(t *testing.T) {
	rep := &Report{Status: store.RunStatusSuccess}
	rep.NodeVerify.Matched = []string{"a", "b"}
	v := peerVerification(rep)
	if !v.Verified() || v.Method != VerifyMethodPeer || v.Files != 2 {
		t.Fatalf("verified=%t method=%q files=%d, want true %q 2", v.Verified(), v.Method, v.Files, VerifyMethodPeer)
	}
	rep.Status = store.RunStatusPartial
	if peerVerification(rep).Verified() {
		t.Fatalf("partial peer session must report unverified")
	}
}
