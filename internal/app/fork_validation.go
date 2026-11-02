package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

const (
	forkTestTimeout   = 15 * time.Minute
	forkTestHeartbeat = 30 * time.Second
	forkTestLease     = 2 * time.Minute
	forkTestFollowup  = 5 * time.Minute
)

// Validation is a durable execution record, never a substitute for a saved
// proof. Heartbeats and terminal states are deliberately absent from snapshots:
// they must not invalidate evidence or produce a notification on every tick.
type ForkValidation struct {
	ID          string `json:"id"`
	Round       string `json:"round"`
	Head        string `json:"head"`
	Base        string `json:"base"`
	Guide       string `json:"guide"`
	Log         string `json:"log"`
	State       string `json:"state"`
	StartedAt   int64  `json:"started_at"`
	HeartbeatAt int64  `json:"heartbeat_at"`
	Deadline    int64  `json:"deadline"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
	NotifiedAt  int64  `json:"notified_at,omitempty"`
	HandledAt   int64  `json:"handled_at,omitempty"`
}

// Emit one normal, durable work event for an abandoned validation. Delivery
// resolves the existing task's current owner and rechecks repository access.
// The ordinary bounded retry schedule remains the fallback after that event.
func recoverForkValidation(p *ForkProgress, now int64) bool {
	v := p.Validation
	if v == nil || v.Round != p.Round || v.NotifiedAt != 0 || v.HandledAt != 0 || p.Phase == "synced" || p.Phase == "restart" || p.Phase == "needs-decision" || p.Phase == "awaiting-review" {
		return false
	}
	copy := *v
	switch v.State {
	case "running":
		if now < v.HeartbeatAt+int64(forkTestLease.Seconds()) && now < v.Deadline {
			return false
		}
		copy.State, copy.FinishedAt = "interrupted", now
	case "passed", "failed", "interrupted":
		if v.FinishedAt == 0 || now < v.FinishedAt+int64(forkTestFollowup.Seconds()) {
			return false
		}
	default:
		return false
	}
	copy.NotifiedAt = now
	p.Validation = &copy
	p.Wake++
	p.NextAttempt = now + int64((15 * time.Minute).Seconds())
	return true
}

// Only this attempt's liveness/status may change. A late process cannot revive
// an expired attempt, overwrite a newer run, or fabricate a passing proof.
func (s *Store) UpdateForkValidation(ctx context.Context, key, round, id, state string) error {
	if state != "running" && state != "failed" && state != "interrupted" {
		return errors.New("invalid validation transition")
	}
	tx, err := s.db.BeginWrite(ctx, "resource:"+key)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := readForkProgress(ctx, tx, key)
	if err != nil {
		return err
	}
	v := p.Validation
	if p.Round != round || v == nil || v.ID != id || v.Round != round || v.State != "running" {
		return ErrStaleWork
	}
	if state == "running" && (time.Now().Unix() >= v.Deadline || time.Now().Unix() >= v.HeartbeatAt+int64(forkTestLease.Seconds())) {
		return ErrStaleWork
	}
	v.HeartbeatAt = time.Now().Unix()
	if state != "running" {
		v.State, v.FinishedAt = state, v.HeartbeatAt
		if p.Phase == "working" || p.Phase == "waiting" {
			p.NextAttempt = v.FinishedAt + int64(forkTestFollowup.Seconds())
		}
		p.Revision++
	}
	// Liveness alone is not a semantic progress revision. Otherwise a slow
	// remote collection would always lose its CAS to a 30-second heartbeat.
	if err = writeForkProgress(ctx, tx, key, p); err != nil {
		return err
	}
	return tx.Commit()
}

// A failed heartbeat cancels validation: losing its durable lease must not
// leave an untracked test process that can later save a proof.
func (s *Service) keepForkValidation(ctx context.Context, key, round, id string, cancel context.CancelFunc) func() {
	heartbeatCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(forkTestHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				c, end := context.WithTimeout(heartbeatCtx, 5*time.Second)
				err := s.Store.UpdateForkValidation(c, key, round, id, "running")
				end()
				if err != nil {
					if heartbeatCtx.Err() == nil {
						log.Printf("validation heartbeat: %s: %v", key, err)
						cancel()
					}
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}

// Persist output as it arrives, with the same 16 MiB bound as before. Retain
// the first error even when a caller (for example fmt.Fprintf) ignores it.
type validationLog struct {
	mu   sync.Mutex
	dst  io.Writer
	size int
	err  error
}

func (w *validationLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.size+len(p) > 16<<20 {
		w.err = errors.New("test output exceeds 16 MiB")
		return 0, w.err
	}
	n, err := w.dst.Write(p)
	w.size += n
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func (w *validationLog) Error() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return fmt.Errorf("validation log: %w", w.err)
	}
	return nil
}
