package sync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"
)

// errInjectedCrash is the failure a faultTransport reports once it has
// crashed.
var errInjectedCrash = errors.New("injected crash")

// transportCall names one call a faultTransport saw.
type transportCall struct {
	op, name, to string
}

// crashMode is how the crashing call behaves.
type crashMode int

const (
	crashBefore crashMode = iota // the call never happens
	crashAfter                   // the call completes, then reports failure
	crashMidway                  // a Put lands one byte of its body
)

// faultTransport stands in for a process that dies at one transport call:
// the first call crashAt selects behaves as mode says, and every call after
// it fails, as if nothing were left to make it. With powerCut the machine
// loses power instead: the crash also takes back every Put and Rename
// since the last Flush, newest first, except the oldest keep(n) of those n
// (none without keep), as a disk that persists its names in order does.
// A kept Put's name survives but not its bytes: it holds as many zeros,
// with its mtime. Calls run one at a time, so a crash lands between whole
// calls.
type faultTransport struct {
	transport
	crashAt  func(transportCall) bool
	mode     crashMode
	powerCut bool
	keep     func(n int) int

	mu        sync.Mutex
	crashed   bool
	calls     []transportCall
	unflushed []transportCall
}

// do runs one call through the crash point. call receives whether it
// should land only part of its effect.
func (f *faultTransport) do(c transportCall, call func(midway bool) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.crashed {
		return errInjectedCrash
	}
	if f.crashAt == nil || !f.crashAt(c) {
		err := call(false)
		f.landed(c, err)
		return err
	}
	f.crashed = true
	switch f.mode {
	case crashAfter:
		f.landed(c, call(false))
	case crashMidway:
		f.landed(c, call(true))
	}
	if f.powerCut {
		f.cutPower()
	}
	return errInjectedCrash
}

// landed notes what a call that succeeded leaves for the next Flush.
func (f *faultTransport) landed(c transportCall, err error) {
	if err != nil {
		return
	}
	switch c.op {
	case "put", "rename":
		f.unflushed = append(f.unflushed, c)
	case "flush":
		f.unflushed = nil
	}
}

// cutPower takes back the puts and renames no Flush covered: a put file
// vanishes, a rename is reversed, and a put file whose name survives loses
// its bytes.
func (f *faultTransport) cutPower() {
	kept := 0
	if f.keep != nil {
		kept = f.keep(len(f.unflushed))
	}
	ctx := context.Background()
	for i, c := range f.unflushed[:kept] {
		if c.op == "put" {
			f.loseBytes(ctx, followRenames(c.name, f.unflushed[i+1:kept]))
		}
	}
	for i := len(f.unflushed) - 1; i >= kept; i-- {
		c := f.unflushed[i]
		switch c.op {
		case "put":
			if err := f.transport.Remove(ctx, c.name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				panic(err)
			}
		case "rename":
			if err := f.transport.Rename(ctx, c.to, c.name); err != nil {
				panic(err)
			}
		}
	}
	f.unflushed = nil
}

func (f *faultTransport) Stat(ctx context.Context, name string) (entry, error) {
	var e entry
	err := f.do(transportCall{op: "stat", name: name}, func(bool) error {
		var err error
		e, err = f.transport.Stat(ctx, name)
		return err
	})
	return e, err
}

func (f *faultTransport) List(ctx context.Context, dir string) ([]entry, error) {
	var es []entry
	err := f.do(transportCall{op: "list", name: dir}, func(bool) error {
		var err error
		es, err = f.transport.List(ctx, dir)
		return err
	})
	return es, err
}

func (f *faultTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	return f.do(transportCall{op: "put", name: name}, func(midway bool) error {
		if midway {
			r = io.LimitReader(r, 1)
		}
		return f.transport.Put(ctx, name, r, mtime)
	})
}

func (f *faultTransport) Rename(ctx context.Context, from, to string) error {
	return f.do(transportCall{op: "rename", name: from, to: to}, func(bool) error {
		return f.transport.Rename(ctx, from, to)
	})
}

// loseBytes replaces the file at name with as many zeros and its mtime.
func (f *faultTransport) loseBytes(ctx context.Context, name string) {
	e, err := f.transport.Stat(ctx, name)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		panic(err)
	}
	if err := f.transport.Remove(ctx, name); err != nil {
		panic(err)
	}
	if err := f.transport.Put(ctx, name, bytes.NewReader(make([]byte, e.size)), e.mtime); err != nil {
		panic(err)
	}
}

// followRenames is where the file put at name sits after renames, which
// may move it or a directory above it.
func followRenames(name string, renames []transportCall) string {
	for _, c := range renames {
		switch {
		case c.op != "rename":
		case c.name == name:
			name = c.to
		case strings.HasPrefix(name, c.name+"/"):
			name = c.to + strings.TrimPrefix(name, c.name)
		}
	}
	return name
}

func (f *faultTransport) Flush(ctx context.Context) error {
	return f.do(transportCall{op: "flush"}, func(bool) error {
		return f.transport.Flush(ctx)
	})
}

func (f *faultTransport) Remove(ctx context.Context, name string) error {
	return f.do(transportCall{op: "remove", name: name}, func(bool) error {
		return f.transport.Remove(ctx, name)
	})
}
