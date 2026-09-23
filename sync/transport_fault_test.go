package sync

import (
	"context"
	"errors"
	"io"
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
// it fails, as if nothing were left to make it.
type faultTransport struct {
	transport
	crashAt func(transportCall) bool
	mode    crashMode
	crashed bool
	calls   []transportCall
}

// do runs one call through the crash point. call receives whether it
// should land only part of its effect.
func (f *faultTransport) do(c transportCall, call func(midway bool) error) error {
	f.calls = append(f.calls, c)
	if f.crashed {
		return errInjectedCrash
	}
	if f.crashAt == nil || !f.crashAt(c) {
		return call(false)
	}
	f.crashed = true
	switch f.mode {
	case crashAfter:
		_ = call(false)
	case crashMidway:
		_ = call(true)
	}
	return errInjectedCrash
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

func (f *faultTransport) Remove(ctx context.Context, name string) error {
	return f.do(transportCall{op: "remove", name: name}, func(bool) error {
		return f.transport.Remove(ctx, name)
	})
}
