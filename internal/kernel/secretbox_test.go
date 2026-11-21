package kernel_test

import (
	"errors"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// fakeBox "seals" as key+":"+plain.
type fakeBox struct{ key string }

func (b fakeBox) Seal(p string) ([]byte, error) { return []byte(b.key + ":" + p), nil }
func (b fakeBox) Open(s []byte) (string, error) {
	pre := b.key + ":"
	if len(s) < len(pre) || string(s[:len(pre)]) != pre {
		return "", errors.New("bad key")
	}
	return string(s[len(pre):]), nil
}

func TestReseal(t *testing.T) {
	oldB, newB := fakeBox{"old"}, fakeBox{"new"}
	sOld, _ := oldB.Seal("secret")
	sNew, _ := newB.Seal("secret")
	sOther, _ := fakeBox{"zzz"}.Seal("secret")

	if o := kernel.Reseal(sNew, oldB, newB); o.Kind != "current" || o.Sealed != nil {
		t.Fatalf("current: %+v", o)
	}
	o := kernel.Reseal(sOld, oldB, newB)
	if o.Kind != "resealed" {
		t.Fatalf("resealed: %+v", o)
	}
	if p, err := newB.Open(o.Sealed); err != nil || p != "secret" {
		t.Fatalf("new key opens result: %q %v", p, err)
	}
	if o := kernel.Reseal(sOther, oldB, newB); o.Kind != "unreadable" || o.Sealed != nil {
		t.Fatalf("unreadable: %+v", o)
	}
}

type failingSeal struct{ fakeBox }

func (failingSeal) Seal(string) ([]byte, error) { return nil, errors.New("seal failed") }

func TestResealSealFailureIsUnreadable(t *testing.T) {
	sOld, _ := fakeBox{"old"}.Seal("secret")
	if o := kernel.Reseal(sOld, fakeBox{"old"}, failingSeal{fakeBox{"new"}}); o.Kind != "unreadable" || o.Sealed != nil {
		t.Fatalf("got %+v", o)
	}
}

// The new key is tried first: with from == to == new key, trying from first would reseal.
func TestResealTriesNewKeyFirst(t *testing.T) {
	newB := fakeBox{"new"}
	sNew, _ := newB.Seal("secret")
	if o := kernel.Reseal(sNew, newB, newB); o.Kind != "current" {
		t.Fatalf("got %+v", o)
	}
}
