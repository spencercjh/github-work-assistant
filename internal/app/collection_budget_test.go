package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type watchedCollectionStore struct {
	Persistence
	waiting chan struct{}
	once    sync.Once
}

func (s *watchedCollectionStore) BeginForkCollection(ctx context.Context, key, scope, token string) (ForkCollectionLease, error) {
	l, err := s.Persistence.BeginForkCollection(ctx, key, scope, token)
	if err == nil && !l.Acquired && !l.Reused {
		s.once.Do(func() { close(s.waiting) })
	}
	return l, err
}

func TestForkCollectionSharesOnlyOverlappingSuccessfulRefresh(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "progress-change", "canceled-waiter"} {
		t.Run(outcome, func(t *testing.T) {
			store := freshTestStore(t)
			other := &watchedCollectionStore{Persistence: openTestStore(t), waiting: make(chan struct{})}
			a, b := &Service{Store: store}, &Service{Store: other}
			f := ForkSpec{ID: "fixture"}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			first, second := make(chan error, 1), make(chan error, 1)
			var calls atomic.Int32
			go func() {
				first <- a.coordinateForkCollection(ctx, f, func(context.Context) error {
					calls.Add(1)
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					if outcome == "failure" {
						return ErrGitHubAccessDenied
					}
					return nil
				})
			}()
			<-started
			waitCtx, stop := context.WithCancel(ctx)
			defer stop()
			collect := func(context.Context) error { calls.Add(1); return nil }
			go func() { second <- b.coordinateForkCollection(waitCtx, f, collect) }()
			select {
			case <-other.waiting:
			case <-ctx.Done():
				t.Fatal("second connection did not observe in-flight collection")
			}
			if outcome == "progress-change" {
				// A concurrent executor commits a semantic revision while network
				// work is in flight. The waiter must perform its own refresh.
				if err := store.PutMeta(ctx, forkProgressName(f.Key()), string(canonical(ForkProgress{Revision: 1}))); err != nil {
					t.Fatal(err)
				}
			}
			if outcome == "canceled-waiter" {
				stop()
				if err := <-second; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-first; (outcome == "failure") != errors.Is(err, ErrGitHubAccessDenied) || (outcome != "failure" && err != nil) {
				t.Fatal(err)
			}
			if outcome != "canceled-waiter" {
				if err := <-second; err != nil {
					t.Fatal(err)
				}
			}
			want := int32(1)
			if outcome == "failure" || outcome == "progress-change" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatalf("collections=%d want=%d", calls.Load(), want)
			}
			// A subsequent operation never uses a time-based successful cache.
			if err := b.coordinateForkCollection(ctx, f, collect); err != nil || calls.Load() != want+1 {
				t.Fatal("sequential operation did not refresh", err, calls.Load())
			}
		})
	}
}

func TestForkCollectionLeaseExpiryAndConfigurationIsolation(t *testing.T) {
	s, ctx := freshTestStore(t), t.Context()
	other := openTestStore(t)
	old, err := s.BeginForkCollection(ctx, "fork:fixture", "config-a", "")
	if err != nil || !old.Acquired {
		t.Fatal(old, err)
	}
	different, err := other.BeginForkCollection(ctx, "fork:fixture", "config-b", "")
	if err != nil || !different.Acquired {
		t.Fatal("different authorization configuration shared results", err)
	}
	old.Until = time.Now().Add(-time.Second).UnixNano()
	if err = s.PutMeta(ctx, collectionName("fork:fixture", "config-a"), string(canonical(old))); err != nil {
		t.Fatal(err)
	}
	next, err := other.BeginForkCollection(ctx, "fork:fixture", "config-a", old.Token)
	if err != nil || !next.Acquired || next.Token == old.Token {
		t.Fatal("expired owner blocked recovery", next, err)
	}
	if err = s.FinishForkCollection(ctx, "fork:fixture", "config-a", old, true); err != nil {
		t.Fatal(err)
	}
	current, err := s.BeginForkCollection(ctx, "fork:fixture", "config-a", old.Token)
	if err != nil || current.Acquired || current.Reused || current.Token != next.Token {
		t.Fatal("late completion released a different owner", current, err)
	}
	if err = other.FinishForkCollection(ctx, "fork:fixture", "config-a", next, true); err != nil {
		t.Fatal(err)
	}
	// Even after a successful completion, a later semantic change prevents
	// a waiter with the old token from consuming it.
	if err = s.PutMeta(ctx, forkProgressName("fork:fixture"), string(canonical(ForkProgress{Revision: 2}))); err != nil {
		t.Fatal(err)
	}
	current, err = s.BeginForkCollection(ctx, "fork:fixture", "config-a", next.Token)
	if err != nil || !current.Acquired || current.Reused {
		t.Fatal("completed lease survived a later progress change", current, err)
	}
}
