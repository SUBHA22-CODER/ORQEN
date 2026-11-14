package safety

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/safefs"
)

// Record is one auditable safety decision. Every field exists so that a
// non-technical reader (or an external reviewer) can reconstruct what ORQEN
// observed, what the AI claimed, what ORQEN decided, and what happened.
// File-level SHA-256 provenance continues to live in repairs.json; this file
// records the decision trail around it.
type Record struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	// Signature is the failure fingerprint the record is about ("" for
	// healthy-stack observations).
	Signature string `json:"signature,omitempty"`
	// Phase is where in the gate the record was written: preflight |
	// review | outcome.
	Phase string `json:"phase"`
	// Decision and Reason capture the ruling.
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	// EvidenceHash is the SHA-256 of the canonical evidence snapshot, so the
	// exact observations behind a decision can be verified later.
	EvidenceHash string `json:"evidence_hash,omitempty"`
	// EvidenceSummary holds the human-readable evidence lines (bounded).
	EvidenceSummary []string `json:"evidence_summary,omitempty"`
	// Diagnosis is the AI claim under review (raw text, bounded).
	Diagnosis string `json:"diagnosis,omitempty"`
	// Conflicts lists detected contradictions at decision time.
	Conflicts []string `json:"conflicts,omitempty"`
	// Confidence and Risk are the engine's independent scores.
	Confidence float64 `json:"confidence,omitempty"`
	Risk       string  `json:"risk,omitempty"`
	// Approval records who approved a RequireApproval decision, when.
	Approval string `json:"approval,omitempty"`
	// Validation is the sandbox verdict for the proposed patch.
	Validation *SandboxResult `json:"validation,omitempty"`
	// PatchHashes are SHA-256 of each proposed file content.
	PatchHashes map[string]string `json:"patch_hashes,omitempty"`
	// Outcome records what ultimately happened: applied | rejected |
	// rolled_back | nothing_to_do | failed.
	Outcome string `json:"outcome,omitempty"`
}

// Audit is the append-only safety decision trail persisted as safety.json.
type Audit struct {
	mu      sync.Mutex
	path    string
	records []Record
	maxRecs int
}

// NewAudit loads (or creates) an audit trail at path. Empty path means
// memory-only.
func NewAudit(path string) *Audit {
	a := &Audit{path: path, maxRecs: 500}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var wrapper struct {
				Records []Record `json:"records"`
			}
			if json.Unmarshal(data, &wrapper) == nil {
				a.records = wrapper.Records
			}
		}
	}
	return a
}

// Append adds a record, fills in identity/timestamp, and persists atomically.
func (a *Audit) Append(rec Record) Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rec.ID == "" {
		rec.ID = "safety-" + strconv.Itoa(len(a.records)+1)
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	a.records = append(a.records, rec)
	if len(a.records) > a.maxRecs {
		a.records = a.records[len(a.records)-a.maxRecs:]
	}
	_ = a.save()
	return rec
}

// Records returns a copy of the trail.
func (a *Audit) Records() []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Record, len(a.records))
	copy(out, a.records)
	return out
}

func (a *Audit) save() error {
	if a.path == "" {
		return nil
	}
	wrapper := struct {
		Records []Record `json:"records"`
	}{Records: a.records}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteFileAtomic(a.path, append(data, '\n'), 0o600)
}

// HashEvidence computes the audit hash of an evidence set: the SHA-256 of its
// canonical JSON, so any tampering with the recorded observations is
// detectable.
func HashEvidence(s Set) string {
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashString returns the lowercase hex SHA-256 of s.
func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
