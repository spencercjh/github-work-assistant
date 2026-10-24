package app

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

// This exact suite runs against SQLite, PostgreSQL, MySQL and seekdb. Separate
// handles ensure correctness comes from the database, not a process-local mutex.
func TestConcurrentFirstObservationAndImmutableTask(t *testing.T) {
	s := freshTestStore(t)
	ctx := context.Background()
	other, err := openTestStore(t, ctx, disposableTestDSN(t), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
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
	tx, err := s.db.BeginWrite(ctx, "resource:example/rollback#1")
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "INSERT INTO ghraft_snapshots(resource_key,version_hash,body,observed_at) VALUES(?,?,?,?)", "example/rollback#1", hash([]byte("snapshot")), "{}", 1)
	if err != nil {
		t.Fatal(err)
	}
	// A real constraint violation AFTER the snapshot write must roll back both.
	_, err = tx.ExecContext(ctx, "INSERT INTO ghraft_events(event_id,resource_key,version_hash,payload,state,created_at) VALUES(?,?,?,?,?,?)", hash([]byte("event")), "example/rollback#1", hash([]byte("snapshot")), nil, "pending", 1)
	if err == nil {
		t.Fatal("NOT NULL constraint was ignored")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Snapshot(ctx, "example/rollback#1"); !errors.Is(err, ErrNotFound) {
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
	if _, err := s.db.Exec("DROP TABLE ghraft_locks"); err != nil {
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
