package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/syncproto"
)

// putContent issues one upload against the real routed handler and
// returns the status and body, so tests can assert on the diagnostic as
// well as the code.
func putContent(t *testing.T, srv *Server, runID int64, blake3Hex string, body []byte) (int, string) {
	t.Helper()
	return putContentAs(t, srv, "test-token", runID, blake3Hex, body)
}

func putContentAs(t *testing.T, srv *Server, token string, runID int64, blake3Hex string, body []byte) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, syncproto.ContentPath(runID, blake3Hex), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// plannedMtimeNs is the mtime the fake /plan verdicts declare, so a test
// can assert the receiver stamps it rather than the wall clock.
const plannedMtimeNs = int64(1700000000000000000)

// awaitContent registers a transfer-disposition entry so the session is
// waiting for content at rel, exactly as /plan would have left it.
func (f *preStageFixture) awaitContent(sess *peerSession, rel string, content []byte) {
	sum := blakeSum(content)
	sess.dispositions[rel] = &sessionEntry{
		disposition: syncproto.DispositionTransfer,
		blake3:      sum,
		size:        int64(len(content)),
		mtimeNs:     plannedMtimeNs,
	}
}

func blakeSum(content []byte) []byte {
	sum := blake3.Sum256(content)
	return sum[:]
}

// TestPutContentLandsBytes is the happy path: a body that hashes to the
// digest it was addressed to and matches the declared size lands at the
// path the session was waiting on, with the plan's mtime applied.
func TestPutContentLandsBytes(t *testing.T) {
	f := newPreStageFixture(t)
	sess := f.newSession()
	content := []byte("alpha bytes")
	f.awaitContent(sess, "sub/a.txt", content)
	f.router.storeSession(sess)

	code, body := putContent(t, f.srv, f.recvRun, blakeHex(content), content)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, body)
	}
	var resp syncproto.ContentResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Paths) != 1 || resp.Paths[0] != "sub/a.txt" {
		t.Fatalf("Paths = %v, want [sub/a.txt]", resp.Paths)
	}
	got, err := os.ReadFile(filepath.Join(f.vol.Path, "sub/a.txt"))
	if err != nil {
		t.Fatalf("read landed file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("landed %q, want %q", got, content)
	}
	// The plan's mtime is applied, so the receiver's next index does not
	// mistake a freshly delivered file for one that changed under it.
	info, err := os.Stat(filepath.Join(f.vol.Path, "sub/a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime().UnixNano(); got != plannedMtimeNs {
		t.Errorf("mtime = %d, want the plan's %d", got, plannedMtimeNs)
	}
}

// TestPutContentMaterializesEveryAwaitingPath is the payoff of keying by
// content rather than by path: the same bytes wanted at several paths in
// one run travel the wire once and the receiver fans them out.
func TestPutContentMaterializesEveryAwaitingPath(t *testing.T) {
	f := newPreStageFixture(t)
	sess := f.newSession()
	content := []byte("shared bytes")
	for _, rel := range []string{"b/dup.txt", "a/orig.txt"} {
		f.awaitContent(sess, rel, content)
	}
	f.router.storeSession(sess)

	code, body := putContent(t, f.srv, f.recvRun, blakeHex(content), content)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, body)
	}
	var resp syncproto.ContentResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Sorted, so the response and the materialisation order are stable.
	if len(resp.Paths) != 2 || resp.Paths[0] != "a/orig.txt" || resp.Paths[1] != "b/dup.txt" {
		t.Fatalf("Paths = %v, want [a/orig.txt b/dup.txt]", resp.Paths)
	}
	for _, rel := range resp.Paths {
		got, err := os.ReadFile(filepath.Join(f.vol.Path, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("%s = %q, want %q", rel, got, content)
		}
	}
}

// TestPutContentRefusesWrongBytes is the refuse-over-wrong-write
// invariant at the one place a peer's bytes enter the volume. A body
// that does not hash to the digest it was addressed to — or is not the
// length /plan declared — must leave the destination absent, not
// half-written. Each is 400: the caller's bytes are what is wrong, as
// distinct from a receiver-side write failure, which is a 500 the
// initiator can only retry.
func TestPutContentRefusesWrongBytes(t *testing.T) {
	declared := []byte("the planned bytes")
	for _, tc := range []struct {
		name string
		sent []byte
		want string
	}{
		{"different content, same length", []byte("the PLANNED bytes"), "hashes to"},
		{"truncated", []byte("the planned"), "plan declared"},
		{"longer than declared", []byte("the planned bytes and more"), "exceeds the"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPreStageFixture(t)
			sess := f.newSession()
			f.awaitContent(sess, "doc.md", declared)
			f.router.storeSession(sess)

			code, body := putContent(t, f.srv, f.recvRun, blakeHex(declared), tc.sent)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", code, body)
			}
			if tc.want != "" && !strings.Contains(body, tc.want) {
				t.Errorf("body = %s, want substring %q", body, tc.want)
			}
			if _, err := os.Stat(filepath.Join(f.vol.Path, "doc.md")); !os.IsNotExist(err) {
				t.Errorf("doc.md exists after a refused upload (stat err = %v)", err)
			}
			// The temp file must go too — a refused upload leaves nothing.
			entries, err := os.ReadDir(f.vol.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".squirrel-incoming-") {
					t.Errorf("refused upload left temp file %s", e.Name())
				}
			}
		})
	}
}

// TestPutContentRejectsMalformedDigest guards the one peer-supplied
// component of the URL. Anything but 64 lowercase hex characters is
// refused before the handler looks at a session or a path, so a
// traversal segment or a case-variant duplicate cannot address the
// filesystem.
func TestPutContentRejectsMalformedDigest(t *testing.T) {
	f := newPreStageFixture(t)
	f.router.storeSession(f.newSession())

	for _, tc := range []struct{ name, digest string }{
		{"too short", "abc123"},
		{"not hex", strings.Repeat("z", 64)},
		{"uppercase", strings.ToUpper(blakeHex([]byte("x")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := putContent(t, f.srv, f.recvRun, tc.digest, []byte("x"))
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", code, body)
			}
		})
	}
}

// TestPutContentRefusesUnawaitedContent: the session's own /plan verdicts
// are the allow-list. Content no path in this run asked for is refused,
// so a token-holding peer cannot write arbitrary objects into a volume by
// opening a session and uploading whatever it likes.
func TestPutContentRefusesUnawaitedContent(t *testing.T) {
	f := newPreStageFixture(t)
	sess := f.newSession()
	f.awaitContent(sess, "wanted.txt", []byte("wanted"))
	f.router.storeSession(sess)

	unwanted := []byte("not in the plan")
	code, body := putContent(t, f.srv, f.recvRun, blakeHex(unwanted), unwanted)
	if code != http.StatusConflict {
		t.Fatalf("status = %d (%s), want 409", code, body)
	}
	if !strings.Contains(body, "awaits content") {
		t.Errorf("body = %s, want the no-path-awaits diagnostic", body)
	}
}

// TestPutContentUnknownSession: an upload addressed to a run with no live
// session is a 404, not an implicit session.
func TestPutContentUnknownSession(t *testing.T) {
	f := newPreStageFixture(t)
	content := []byte("orphan")
	code, body := putContent(t, f.srv, f.recvRun+999, blakeHex(content), content)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want 404", code, body)
	}
}

// TestPutContentRequiresBearer: the upload route carries the same auth as
// every other sync endpoint. Losing it on the one endpoint that writes
// would be the worst place to lose it.
func TestPutContentRequiresBearer(t *testing.T) {
	f := newPreStageFixture(t)
	sess := f.newSession()
	content := []byte("bytes")
	f.awaitContent(sess, "a.txt", content)
	f.router.storeSession(sess)

	code, _ := putContentAs(t, f.srv, "wrong-token", f.recvRun, blakeHex(content), content)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if _, err := os.Stat(filepath.Join(f.vol.Path, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("a.txt exists after an unauthenticated upload (stat err = %v)", err)
	}
}
