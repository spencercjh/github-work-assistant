package app

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"
)

// This exact suite runs against SQLite, PostgreSQL, MySQL and seekdb. Separate
// handles ensure correctness comes from the database, not a process-local mutex.
func TestConcurrentFirstObservationAndImmutableTask(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	other := openTestStore(t)
	stores := []Persistence{s, other}
	start := make(chan struct{})
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 12)
	var wg sync.WaitGroup
	body := []byte(`{"title":"first"}`)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, e := stores[i%2].Observe(ctx, "example/repo#1", body)
			results <- result{id, e}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	created := 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.id != "" {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("first observation created %d events", created)
	}
	events, err := s.Events(ctx, false)
	if err != nil || len(events) != 1 {
		t.Fatalf("outbox: %d %v", len(events), err)
	}
	snapshot, version, err := other.Snapshot(ctx, "example/repo#1")
	if err != nil || !bytes.Equal(snapshot, body) || version != events[0].Version {
		t.Fatal("snapshot/outbox diverged", err)
	}
	// Two independent writers cannot assign the same resource to different work.
	start = make(chan struct{})
	errs := make(chan error, 2)
	for i, id := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"} {
		go func(i int, id string) {
			<-start
			_, e := stores[i].BindTask(ctx, "example/repo#1", WorkInput{EventID: events[0].ID, TaskID: id, EvidenceVersion: version, GuideVersion: hash([]byte("guide"))}, "#work")
			errs <- e
		}(i, id)
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		if <-errs == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("conflicting task assignments accepted: %d", success)
	}
}

func TestFailedOutboxTransactionLeavesNoSnapshot(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	failure := errors.New("outbox insert failed")
	callbacks := s.db.Session(ctx).Callback().Create()
	if err := callbacks.Before("gorm:create").Register("test:fail_outbox", func(tx *gorm.DB) {
		if tx.Statement.Table == "ghraft_events" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { callbacks.Remove("test:fail_outbox") })
	// Fail the event insert in the actual observation path, after it saves the
	// snapshot. The application must roll back instead of losing a notification.
	if id, err := s.Observe(ctx, "example/rollback#1", []byte(`{"title":"new"}`)); !errors.Is(err, failure) || id != "" {
		t.Fatal("failed outbox write was accepted", id, err)
	}
	if _, _, err := s.Snapshot(ctx, "example/rollback#1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed transaction retained snapshot", err)
	}
	events, err := s.Events(ctx, false)
	if err != nil || len(events) != 0 {
		t.Fatal("failed transaction retained event", err)
	}
}

func TestSchemaReadinessRequiresResourceLocks(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	if err := s.SchemaReady(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE ghraft_locks"); err != nil {
		t.Fatal(err)
	}
	if err := s.SchemaReady(ctx); err == nil {
		t.Fatal("diagnostics accepted a schema missing the resource lock table")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SchemaReady(ctx); err != nil {
		t.Fatal(err)
	}
}
