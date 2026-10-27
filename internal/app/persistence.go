package app

import (
	"context"
	"encoding/json"

	"github-raft-app/internal/database"
)

var ErrNotFound = database.ErrNotFound

// Persistence is the application contract. Implementations must preserve atomic
// observation/outbox writes, immutable report fingerprints and pending delivery
// envelopes, and stale evidence rejection. It deliberately exposes no SQL connection or transaction.
// A non-SQL implementation can satisfy this interface without changing services.
type Persistence interface {
	ForkProgress(context.Context, string) (ForkProgress, error)
	ObserveFork(context.Context, ForkSpec, ForkProgress, ForkObservation) error
	SaveForkProgress(context.Context, string, int64, ForkProgress, *WorkResult) error
	Close() error
	Migrate(context.Context) error
	Ping(context.Context) error
	SchemaReady(context.Context) error
	SaveSecret(context.Context, string, []byte, int64) error
	Secret(context.Context, string) ([]byte, error)
	DeleteSecret(context.Context, string) error
	Observe(context.Context, string, []byte) (string, error)
	RequestWork(context.Context, string, string, string, string) (Event, error)
	Snapshot(context.Context, string) (json.RawMessage, string, error)
	ObservedAt(context.Context, string) (int64, error)
	CurrentEvent(context.Context, string, string) (string, error)
	Event(context.Context, string) (Event, error)
	TaskLinked(context.Context, string) (bool, error)
	Events(context.Context, bool) ([]Event, error)
	EventsPage(context.Context, bool, int) ([]Event, error)
	Delivery(context.Context, string, []byte, bool, int) error
	ResetDelivery(context.Context, string) error
	Result(context.Context, string, string, []byte) error
	RegisterWatch(context.Context, Watch) error
	Watches(context.Context) ([]Watch, error)
	Meta(context.Context, string, string) (string, error)
	PutMeta(context.Context, string, string) error
	PrepareDelivery(context.Context, string, string) (string, json.RawMessage, error)
	Discussion(context.Context, string) (Discussion, error)
	DiscussionCandidates(context.Context, string, string) ([]TaskDispatch, error)
	PrepareDiscussion(context.Context, string, Discussion, string) (Discussion, error)
	ConfirmDiscussion(context.Context, string, Discussion, TaskDispatch) error
	Cleanup(context.Context, int, bool) (CleanupStats, error)
	CollectionStatus(context.Context, string, error) error
	RecordTriage(context.Context, Triage) error
	UnprocessedPage(context.Context, int) ([]map[string]any, error)
	ImportLegacy(context.Context, Config, []byte) error
	History(context.Context, string) ([]json.RawMessage, error)
	Status(context.Context) (map[string]any, error)
	RegisterAccess(context.Context, string, string) error
	SeedAccess(context.Context) error
	WatchSource(context.Context, string) (string, error)
	WatchBlocked(context.Context, string) (bool, error)
	SetBlocked(context.Context, string, bool) error
	ReadWork(context.Context, string) (WorkContext, error)
	BindTask(context.Context, string, WorkInput, string) (TaskLink, error)
	SaveWorkResult(context.Context, string, WorkResult) error
	CompletionReady(context.Context, string, string, bool) (bool, error)
}

var _ Persistence = (*Store)(nil)
