package app

// Database records are separate from API objects. GORM owns dialect-specific
// column types and schema migration; existing table and column names are kept.
type accessRecord struct {
	ResourceKey string `gorm:"size:240;primaryKey"`
	Source      string `gorm:"size:16;not null"`
	Blocked     bool   `gorm:"not null;default:false"`
}

func (accessRecord) TableName() string { return "ghraft_watch_access" }

type workLinkRecord struct {
	ResourceKey     string `gorm:"size:240;primaryKey"`
	TaskID          string `gorm:"size:36;not null"`
	Target          string `gorm:"size:240;not null"`
	EventID         string `gorm:"size:64;not null"`
	EvidenceVersion string `gorm:"size:64;not null"`
	GuideVersion    string `gorm:"size:64;not null"`
	UpdatedAt       int64  `gorm:"not null;autoUpdateTime:false"`
}

func (workLinkRecord) TableName() string { return "ghraft_work_links" }

type watchRecord struct {
	ResourceKey string  `gorm:"size:240;primaryKey"`
	Repo        string  `gorm:"size:220;not null"`
	Number      int     `gorm:"not null"`
	LastAttempt int64   `gorm:"not null;default:0"`
	LastSuccess int64   `gorm:"not null;default:0"`
	LastError   *string `gorm:"type:text"`
}

func (watchRecord) TableName() string { return "ghraft_watches" }

type metaRecord struct {
	Name  string `gorm:"size:120;primaryKey"`
	Value string `gorm:"size:16777215;not null"` // Includes complete fork provenance proofs; MySQL TEXT is too small.
}

func (metaRecord) TableName() string { return "ghraft_meta" }

type legacyRecord struct {
	Marker     string `gorm:"size:160;primaryKey"`
	Body       string `gorm:"size:16777215;not null"`
	CreatedAt  int64  `gorm:"not null;default:0;autoCreateTime:false"`
	BodyPruned bool   `gorm:"not null;default:false"`
}

func (legacyRecord) TableName() string { return "ghraft_legacy_history" }

type triageRecord struct {
	BodyHash   string `gorm:"size:64;not null;default:''"`
	BodyPruned bool   `gorm:"not null;default:false"`
	EventID    string `gorm:"size:64;primaryKey"`
	Body       string `gorm:"type:text;not null"`
	CreatedAt  int64  `gorm:"not null;autoCreateTime:false"`
}

func (triageRecord) TableName() string { return "ghraft_triage" }

type snapshotRecord struct {
	ResourceKey string `gorm:"size:240;primaryKey"`
	VersionHash string `gorm:"size:64;not null"`
	Body        string `gorm:"size:16777215;not null"`
	ObservedAt  int64  `gorm:"not null"`
}

func (snapshotRecord) TableName() string { return "ghraft_snapshots" }

type eventRecord struct {
	AcceptedAt  int64   `gorm:"not null;default:0"`
	CompactedAt int64   `gorm:"not null;default:0"`
	EventID     string  `gorm:"size:64;primaryKey"`
	ResourceKey string  `gorm:"size:240;not null"`
	VersionHash string  `gorm:"size:64;not null"`
	Payload     string  `gorm:"size:16777215;not null"`
	State       string  `gorm:"size:16;not null"`
	Receipt     *string `gorm:"type:text"`
	Attempts    int     `gorm:"not null;default:0"`
	NextAttempt int64   `gorm:"not null;default:0"`
	CreatedAt   int64   `gorm:"not null;autoCreateTime:false"`
}

func (eventRecord) TableName() string { return "ghraft_events" }

type secretRecord struct {
	SecretKey  string `gorm:"size:200;primaryKey"`
	Ciphertext []byte `gorm:"not null"`
	ExpiresAt  int64  `gorm:"not null"`
}

func (secretRecord) TableName() string { return "ghraft_private" }

type resultRecord struct {
	BodyHash   string `gorm:"size:64;not null;default:''"`
	BodyPruned bool   `gorm:"not null;default:false"`
	EventID    string `gorm:"size:64;primaryKey"`
	ReportID   string `gorm:"size:160;primaryKey"`
	Body       string `gorm:"size:16777215;not null"`
	CreatedAt  int64  `gorm:"not null;autoCreateTime:false"`
}

func (resultRecord) TableName() string { return "ghraft_results" }
