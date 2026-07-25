package durable

import (
	"encoding/json"
	"fmt"
	"time"
)

// Document is the versioned JSON interchange and backup format for one run.
type Document struct {
	SchemaVersion int         `json:"schema_version"`
	Snapshot      RunSnapshot `json:"snapshot"`
	Events        []Event     `json:"events"`
}

// EncodeDocument serializes a current-version durable document.
func EncodeDocument(document Document) ([]byte, error) {
	document.SchemaVersion = SchemaVersion
	document.Snapshot.SchemaVersion = SchemaVersion
	return json.Marshal(document)
}

// DecodeDocument decodes a durable document and migrates older supported versions.
func DecodeDocument(data []byte) (Document, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Document{}, fmt.Errorf("durable: decode document: %w", err)
	}
	switch header.SchemaVersion {
	case SchemaVersion:
		var document Document
		if err := json.Unmarshal(data, &document); err != nil {
			return Document{}, fmt.Errorf("durable: decode v2 document: %w", err)
		}
		document.Snapshot.SchemaVersion = SchemaVersion
		return document, nil
	case 1:
		return migrateV1(data)
	default:
		return Document{}, fmt.Errorf("durable: unsupported schema version %d", header.SchemaVersion)
	}
}

// V1Document models the simulated v1 format retained solely for migration tests and imports.
type V1Document struct {
	SchemaVersion int       `json:"schema_version"`
	TenantID      TenantID  `json:"tenant_id"`
	RunID         RunID     `json:"run_id"`
	Revision      uint64    `json:"revision"`
	EventSequence uint64    `json:"event_sequence"`
	Status        RunStatus `json:"status"`
	Cancelled     bool      `json:"cancelled,omitempty"`
	BudgetTokens  int64     `json:"budget_tokens,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Events        []Event   `json:"events,omitempty"`
}

func migrateV1(data []byte) (Document, error) {
	var old V1Document
	if err := json.Unmarshal(data, &old); err != nil {
		return Document{}, fmt.Errorf("durable: decode v1 document: %w", err)
	}
	snapshot := RunSnapshot{
		SchemaVersion: SchemaVersion, TenantID: old.TenantID, RunID: old.RunID,
		Revision: old.Revision, EventSequence: old.EventSequence, Status: old.Status,
		CreatedAt: old.CreatedAt.UTC(), UpdatedAt: old.UpdatedAt.UTC(),
		CumulativeBudget: BudgetCounters{InputTokens: old.BudgetTokens},
	}
	if snapshot.Status == "" {
		snapshot.Status = RunPending
	}
	if old.Cancelled {
		snapshot.Cancellation = &Cancellation{RequestedAt: old.UpdatedAt.UTC()}
	}
	if snapshot.EventSequence == 0 {
		snapshot.EventSequence = uint64(len(old.Events))
	}
	for i := range old.Events {
		if old.Events[i].TenantID == "" {
			old.Events[i].TenantID = old.TenantID
		}
		if old.Events[i].RunID == "" {
			old.Events[i].RunID = old.RunID
		}
		if old.Events[i].Sequence == 0 {
			old.Events[i].Sequence = uint64(i + 1)
		}
	}
	return Document{SchemaVersion: SchemaVersion, Snapshot: snapshot, Events: old.Events}, nil
}
