package outbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestComposeSweepsOfNothingIsNil(t *testing.T) {
	if ComposeSweeps() != nil {
		t.Fatal("no sweeps must give no sweep (the worker then has none)")
	}
}

func TestComposeSweepsOfOneIsThatSweep(t *testing.T) {
	ran := 0
	s := ComposeSweeps(Sweep{Every: 5 * time.Minute, Run: func(context.Context) error { ran++; return nil }})
	if s == nil || s.Every != 5*time.Minute {
		t.Fatalf("composed: %+v", s)
	}
	if err := s.Run(context.Background()); err != nil || ran != 1 {
		t.Fatalf("ran %d: %v", ran, err)
	}
}

// Each member keeps its own period; the composite wakes at the shortest one. A member that
// fails or panics does not stop the others.
func TestComposedSweepsKeepTheirOwnPeriodAndIsolateFailures(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var log []string
	s := composeSweeps(func() time.Time { return now }, []Sweep{
		{Every: time.Minute, Run: func(context.Context) error { log = append(log, "fast"); return errors.New("secret-detail") }},
		{Every: 10 * time.Minute, Run: func(context.Context) error { log = append(log, "slow"); panic("secret-panic") }},
		{Every: time.Hour, Run: func(context.Context) error { log = append(log, "hourly"); return nil }},
	})
	if s.Every != time.Minute {
		t.Fatalf("Every = %s, want the shortest", s.Every)
	}
	run := func() error { return s.Run(context.Background()) }

	err := run()
	if !slices.Equal(log, []string{"fast", "slow", "hourly"}) {
		t.Fatalf("first run: %v", log)
	}
	if err == nil {
		t.Fatal("failures of members are lost")
	}
	var hp handlerPanic
	if !errors.As(err, &hp) || len(hp.frames) == 0 {
		t.Errorf("the panic of a member is not reported as one: %v", err)
	}
	if strings.Contains(err.Error(), "secret-panic") {
		t.Errorf("the panic value reached the error: %v", err)
	}

	log = nil
	now = now.Add(time.Minute)
	_ = run()
	if !slices.Equal(log, []string{"fast"}) {
		t.Fatalf("after 1 min: %v", log)
	}
	log = nil
	now = now.Add(9 * time.Minute)
	_ = run()
	if !slices.Equal(log, []string{"fast", "slow"}) {
		t.Fatalf("after 10 min: %v", log)
	}
	log = nil
	now = now.Add(50 * time.Minute)
	_ = run()
	if !slices.Equal(log, []string{"fast", "slow", "hourly"}) {
		t.Fatalf("after 1 h: %v", log)
	}
}

func TestComposedSweepsPassTheContextAndSkipAMemberWithoutRun(t *testing.T) {
	type key struct{}
	var got any
	s := ComposeSweeps(Sweep{Every: time.Minute}, Sweep{Every: time.Minute, Run: func(ctx context.Context) error {
		got = ctx.Value(key{})
		return nil
	}})
	if err := s.Run(context.WithValue(context.Background(), key{}, "v")); err != nil || got != "v" {
		t.Fatalf("%v %v", got, err)
	}
}
