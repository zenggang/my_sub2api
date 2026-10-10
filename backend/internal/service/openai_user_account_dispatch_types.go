package service

import (
	"context"
	"time"
)

const (
	SettingKeyOpenAIUserAccountDispatchEnabled = "openai_user_account_dispatch_enabled"
	OpenAIDispatchPreviewTTL                   = 120 * time.Second
	OpenAIDispatchOperationTTL                 = 24 * time.Hour
	OpenAIDispatchObservationTTL               = 15 * time.Minute
	OpenAIStickySnapshotMaxAge                 = time.Hour
	OpenAIStickyVersionMinTTL                  = OpenAIStickySnapshotMaxAge + 5*time.Minute
)

// OpenAIStickyScope describes one physical primary/legacy binding, not a user namespace.
type OpenAIStickyScope struct {
	GroupID     int64  `json:"group_id"`
	Hash        string `json:"hash"`
	LegacyHash  string `json:"legacy_hash"`
	Fallback    bool   `json:"fallback"`
	DualWrite   bool   `json:"dual_write"`
	UserID      int64  `json:"user_id"`
	Fingerprint string `json:"fingerprint"`
	Protocol    string `json:"protocol"`
	Supported   bool   `json:"supported"`
	Own         bool   `json:"own"`
}

type OpenAIDispatchMarker struct {
	OperationID    string `json:"operation_id"`
	SessionRef     string `json:"session_ref"`
	RebindRevision string `json:"rebind_revision"`
	DeadlineMS     int64  `json:"deadline_ms"`
}

type OpenAIStickySnapshot struct {
	AccountID        int64                 `json:"account_id"`
	Revision         string                `json:"revision"`
	CapturedAtMS     int64                 `json:"captured_at_ms"`
	TTLMillis        int64                 `json:"ttl_millis"`
	OwnerID          int64                 `json:"owner_id"`
	OwnerConflict    bool                  `json:"owner_conflict"`
	IdentityConflict bool                  `json:"identity_conflict"`
	HasWS            bool                  `json:"has_ws"`
	Supported        bool                  `json:"supported"`
	Marker           *OpenAIDispatchMarker `json:"marker,omitempty"`
	Scope            OpenAIStickyScope     `json:"scope"`
}

type OpenAIStickyMutation struct {
	Action          string `json:"action"`
	AccountID       int64  `json:"account_id"`
	TTLMillis       int64  `json:"ttl_millis"`
	LegacyTTLMillis int64  `json:"legacy_ttl_millis"`
	NewRevision     string `json:"new_revision"`
}

type OpenAIDispatchIdentity struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type OpenAIDispatchSession struct {
	SessionRef        string     `json:"session_ref"`
	GroupID           int64      `json:"group_id"`
	RebindResult      string     `json:"rebind_result"`
	RebindRevision    string     `json:"rebind_revision,omitempty"`
	ReboundAt         *time.Time `json:"rebound_at,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	Observation       string     `json:"observation"`
	AssignedAccountID *int64     `json:"assigned_account_id,omitempty"`
	AssignedAt        *time.Time `json:"assigned_at,omitempty"`
	Queued            bool       `json:"queued"`
	CurrentBinding    any        `json:"current_binding"`
	BindingCheckedAt  *time.Time `json:"binding_checked_at,omitempty"`
	InFlight          int64      `json:"in_flight"`
}

type OpenAIDispatchCounts struct {
	Rebindable     int   `json:"rebindable"`
	InFlight       int64 `json:"in_flight"`
	Skipped        int   `json:"skipped"`
	Rebound        int   `json:"rebound"`
	Unresolved     int   `json:"unresolved"`
	AssignedTarget int   `json:"assigned_target"`
	AssignedOther  int   `json:"assigned_other"`
	Awaiting       int   `json:"awaiting_observation"`
}

type OpenAIDispatchCoverage struct {
	Truncated bool   `json:"truncated"`
	Scope     string `json:"scope"`
}

type OpenAIDispatchTarget struct {
	AccountID      int64  `json:"account_id"`
	Name           string `json:"name"`
	Compatible     bool   `json:"compatible"`
	Reason         string `json:"reason,omitempty"`
	MaxConcurrency int    `json:"max_concurrency"`
	Active         int    `json:"active"`
	Waiting        int    `json:"waiting"`
}

type OpenAIDispatchPreview struct {
	PreviewID       string                  `json:"preview_id"`
	ExpiresAt       time.Time               `json:"expires_at"`
	User            OpenAIDispatchIdentity  `json:"user"`
	SourceAccount   OpenAIDispatchIdentity  `json:"source_account"`
	Sessions        []OpenAIDispatchSession `json:"sessions"`
	SkippedSessions []OpenAIDispatchSession `json:"skipped_sessions"`
	Targets         []OpenAIDispatchTarget  `json:"targets"`
	Coverage        OpenAIDispatchCoverage  `json:"coverage"`
	Counts          OpenAIDispatchCounts    `json:"counts"`
}

type OpenAIDispatchOperation struct {
	OperationID         string                  `json:"operation_id"`
	UserID              int64                   `json:"user_id"`
	SourceAccountID     int64                   `json:"source_account_id"`
	TargetAccountID     int64                   `json:"target_account_id"`
	CreatedAt           time.Time               `json:"created_at"`
	ExecutionDeadline   time.Time               `json:"execution_deadline"`
	ObservationDeadline time.Time               `json:"observation_deadline"`
	Accepted            bool                    `json:"accepted"`
	State               string                  `json:"state"`
	Counts              OpenAIDispatchCounts    `json:"counts"`
	Sessions            []OpenAIDispatchSession `json:"sessions"`
}

type OpenAIDispatchPreviewRecord struct {
	OwnerHash string                 `json:"owner_hash"`
	Preview   OpenAIDispatchPreview  `json:"preview"`
	Bindings  []OpenAIStickySnapshot `json:"bindings"`
}

type OpenAIDispatchOperationRecord struct {
	OwnerHash string                  `json:"owner_hash"`
	BodyHash  string                  `json:"body_hash"`
	Operation OpenAIDispatchOperation `json:"operation"`
	Bindings  []OpenAIStickySnapshot  `json:"bindings"`
}

// The extension is optional for in-memory legacy test doubles; the production Redis cache implements it.
type OpenAIUserDispatchCache interface {
	ReadOpenAIStickySnapshot(context.Context, OpenAIStickyScope, string) (*OpenAIStickySnapshot, error)
	MutateOpenAIStickyBinding(context.Context, OpenAIStickyScope, OpenAIStickySnapshot, OpenAIStickyMutation) (*OpenAIStickySnapshot, bool, error)
	ListOpenAIStickyUserBindings(context.Context, int64, int64) ([]OpenAIStickyScope, bool, error)
	SaveOpenAIDispatchPreview(context.Context, *OpenAIDispatchPreviewRecord) error
	GetOpenAIDispatchPreview(context.Context, string) (*OpenAIDispatchPreviewRecord, error)
	FindOpenAIDispatchOperation(context.Context, string, string) (*OpenAIDispatchOperationRecord, error)
	ReserveOpenAIDispatchOperation(context.Context, string, *OpenAIDispatchOperationRecord) (*OpenAIDispatchOperationRecord, error)
	GetOpenAIDispatchOperation(context.Context, string) (*OpenAIDispatchOperationRecord, error)
	RebindOpenAIDispatchSession(context.Context, *OpenAIDispatchOperationRecord, int, time.Duration, time.Duration, string, bool) error
	ObserveOpenAIDispatchAdmission(context.Context, OpenAIStickyScope, OpenAIDispatchMarker, int64, bool) error
}
