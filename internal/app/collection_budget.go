package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// No successful-result TTL: only a caller that actually waited for this token
// may reuse its result. A later operation always performs a new collection.
type ForkCollectionLease struct {
	Token    string `json:"token"`
	Until    int64  `json:"until"`
	Revision int64  `json:"revision"`
	Complete bool   `json:"complete"`
	Acquired bool   `json:"-"`
	Reused   bool   `json:"-"`
}

func collectionName(key, scope string) string {
	return "fork-collection:" + hash([]byte(key+"\x00"+scope))
}

func (s *Store) BeginForkCollection(ctx context.Context, key, scope, waited string) (ForkCollectionLease, error) {
	name := collectionName(key, scope)
	tx, err := s.db.BeginWrite(ctx, name)
	if err != nil {
		return ForkCollectionLease{}, err
	}
	defer tx.Rollback()
	var raw string
	var lease ForkCollectionLease
	err = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, name).Scan(&raw)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return lease, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &lease); err != nil {
			return lease, err
		}
	}
	p, err := readForkProgress(ctx, tx, key)
	if err != nil {
		return lease, err
	}
	if waited != "" && waited == lease.Token && lease.Complete && p.Revision == lease.Revision {
		lease.Reused = true
		return lease, nil
	}
	if lease.Until > time.Now().UnixNano() {
		return lease, nil
	}
	var nonce [24]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return lease, err
	}
	lease = ForkCollectionLease{Token: hex.EncodeToString(nonce[:]), Until: time.Now().Add(10 * time.Minute).UnixNano(), Revision: p.Revision}
	if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(lease))}, []string{"name"}, []string{"value"}); err != nil {
		return lease, err
	}
	lease.Acquired = true
	return lease, tx.Commit()
}

func (s *Store) FinishForkCollection(ctx context.Context, key, scope string, held ForkCollectionLease, success bool) error {
	name := collectionName(key, scope)
	tx, err := s.db.BeginWrite(ctx, name)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM ghraft_meta WHERE name=?`, name).Scan(&raw); err != nil {
		return err
	}
	var current ForkCollectionLease
	if err = json.Unmarshal([]byte(raw), &current); err != nil {
		return err
	}
	if current.Token != held.Token {
		return nil // An expired owner's completion cannot release its successor.
	}
	p, err := readForkProgress(ctx, tx, key)
	if err != nil {
		return err
	}
	// A semantic update during collection requires the waiting caller's own
	// refresh. This also covers successful collection that discovers changes.
	current.Complete = success && current.Until > time.Now().UnixNano() && p.Revision == held.Revision
	current.Until = 0
	if err = tx.Upsert(ctx, &metaRecord{name, string(canonical(current))}, []string{"name"}, []string{"value"}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) coordinateForkCollection(ctx context.Context, f ForkSpec, collect func(context.Context) error) error {
	// A crashed process releases by expiry. A live owner stops its network work
	// before that lease can be acquired by another process.
	ctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()
	scope := hash(canonical(s.Config))
	waited := ""
	for {
		lease, err := s.Store.BeginForkCollection(ctx, f.Key(), scope, waited)
		if err != nil || lease.Reused {
			return err
		}
		if lease.Acquired {
			err = collect(ctx)
			releaseCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			releaseErr := s.Store.FinishForkCollection(releaseCtx, f.Key(), scope, lease, err == nil)
			stop()
			return errors.Join(err, releaseErr)
		}
		waited = lease.Token
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
