package outbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
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

type namedFailure struct{}

func (namedFailure) Error() string { return "secret detail" }

type otherFailure struct{}

func (otherFailure) Error() string { return "other secret" }

// The label is the operator's only clue why a sweep is failing: one failure keeps its own.
func TestAComposedSweepKeepsTheLabelOfASingleFailure(t *testing.T) {
	for name, members := range map[string][]Sweep{
		"one member": {{Every: time.Minute, Run: func(context.Context) error { return namedFailure{} }}},
		"one of two fails": {
			{Every: time.Minute, Run: func(context.Context) error { return nil }},
			{Every: time.Minute, Run: func(context.Context) error { return namedFailure{} }},
		},
	} {
		err := ComposeSweeps(members...).Run(context.Background())
		if got := kernel.ErrorLabel(err); got != "outbox.namedFailure" {
			t.Errorf("%s: label %q", name, got)
		}
		if _, ok := err.(namedFailure); !ok {
			t.Errorf("%s: the error was wrapped: %T", name, err)
		}
	}
}

func TestAComposedSweepNamesEveryFailingMemberInItsLabelWithoutContent(t *testing.T) {
	err := ComposeSweeps(
		Sweep{Every: time.Minute, Run: func(context.Context) error { return namedFailure{} }},
		Sweep{Every: time.Minute, Run: func(context.Context) error { return otherFailure{} }},
	).Run(context.Background())
	label := kernel.ErrorLabel(err)
	if !strings.Contains(label, "namedFailure") || !strings.Contains(label, "otherFailure") {
		t.Fatalf("label %q does not name both members", label)
	}
	if strings.Contains(label, "secret") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("content leaked: %q / %q", label, err.Error())
	}
	if !errors.As(err, new(namedFailure)) || !errors.As(err, new(otherFailure)) {
		t.Error("the members are not reachable through errors.As")
	}
}

// The time is taken once per composed run: a slow member does not count against the periods of
// the ones after it.
func TestAComposedRunJudgesDueMembersAtTheTimeItStarted(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var log []string
	s := composeSweeps(func() time.Time { return now }, []Sweep{
		{Every: time.Hour, Run: func(context.Context) error { log = append(log, "slow"); now = now.Add(2 * time.Minute); return nil }},
		{Every: 5 * time.Minute, Run: func(context.Context) error { log = append(log, "five"); return nil }},
	})
	_ = s.Run(context.Background())
	now = now.Add(4 * time.Minute) // 6 min after the first run began, 4 after the slow one ended
	log = nil
	_ = s.Run(context.Background())
	if !slices.Equal(log, []string{"five"}) {
		t.Fatalf("second run: %v", log)
	}
}
