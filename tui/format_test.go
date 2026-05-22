package tui

import (
	"testing"
	"time"
)

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"}, // clock skew clamps to zero
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{time.Minute + 30*time.Second, "1m 30s"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{time.Hour + 30*time.Minute, "1h 30m"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{36 * time.Hour, "1d 12h"},
		{10 * 24 * time.Hour, "10d"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestParentOf(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"foo":           "",
		"foo/bar":       "foo",
		"foo/bar/baz":   "foo/bar",
		"a/b/c/d/e/f/g": "a/b/c/d/e/f",
	}
	for in, want := range cases {
		if got := parentOf(in); got != want {
			t.Errorf("parentOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFolderDisplayName(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"foo":         "foo",
		"foo/bar":     "bar",
		"foo/bar/baz": "baz",
	}
	for in, want := range cases {
		if got := folderDisplayName(in); got != want {
			t.Errorf("folderDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHexShort(t *testing.T) {
	full := []byte{0x12, 0x34, 0xab, 0xcd}
	if got := hexShort(full, 32); got != "1234abcd" {
		t.Errorf("hexShort short input = %q, want %q", got, "1234abcd")
	}
	long := make([]byte, 32)
	for i := range long {
		long[i] = byte(i)
	}
	got := hexShort(long, 8)
	// 8 hex chars = first 4 bytes -> 00010203
	want := "00010203…"
	if got != want {
		t.Errorf("hexShort 32B@8 = %q, want %q", got, want)
	}
}
