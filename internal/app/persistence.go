package app

import (
	"context"
	"encoding/json"

	"github-raft-app/internal/database"
)

var ErrNotFound = database.ErrNotFound

// Persistence is the application contract. Implementations must preserve atomic
// observation/outbox writes, immutable reports and delivery envelopes, and stale
// evidence rejection. It deliberately exposes no SQL connection or transaction.
// A non-SQL implementation can satisfy this interface without changing services.
type Persistence interface {
	Close() error
	Migrate(context.Context) error
	Ping(context.Context) error
	SchemaReady(context.Context) error
	SaveSecret(context.Context, string, []byte, int64) error
	Secret(context.Context, string) ([]byte, error)
	DeleteSecret(context.Context, string) error
	Observe(context.Context, string, []byte) (string, error)
	Snapshot(context.Context, string) (json.RawMessage, string, error)
	ObservedAt(context.Context, string) (int64, error)
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
	InsertMeta(context.Context, string, string) error
	CollectionStatus(context.Context, string, error) error
	RecordTriage(context.Context, Triage) error
	Unprocessed(context.Context) ([]map[string]any, error)
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
	SaveWorkResult(context.Context, string, WorkResult, RepositoryGuide) error
}

var _ Persistence = (*Store)(nil)
