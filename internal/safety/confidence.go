package safety

import (
	"fmt"
	"math"
	"strings"
)

// Confidence scores a diagnosis-and-evidence pair in [0,1]. It is computed
// exclusively from deterministic inputs — evidence quality, reproducibility,
// and agreement between independent signals — and is reduced by conflicts.
// Anything the LLM asserts by itself is capped well below the autonomous
// threshold: a model's word is never proof.
const (
	// llmOnlyCap bounds the confidence of an uncorroborated model claim.
	llmOnlyCap = 0.35
	// AutoApplyConfidence is the minimum score for autonomous repair.
	AutoApplyConfidence = 0.7
	// DefaultReproducibilityTarget is the number of repeated observations
	// needed for full reproducibility credit.
	DefaultReproducibilityTarget = 3
)

// ConfidenceInput gathers the raw terms of the score.
type ConfidenceInput struct {
	// Evidence carries the latest deterministic observations.
	Evidence Set
	// Repetitions is how many independent observations saw the same problem
	// signature (sampler runs).
	Repetitions int
	// ReproTarget overrides the repetitions needed for full credit.
	ReproTarget int
	// Conflicts are confirmed contradictions between diagnosis and evidence.
	Conflicts int
	// SignalsAgree is the count of independent evidence kinds that point at
	// the same problem; SignalsTotal is how many kinds were present.
	SignalsAgree, SignalsTotal int
	// ModelOnly is true when the diagnosis rests solely on LLM assertions
	// with no deterministic support.
	ModelOnly bool
}

// Score computes the confidence in [0,1].
func Score(in ConfidenceInput) float64 {
	// Evidence quality: the strongest deterministic observation decides the
	// base; a set with no deterministic evidence has no base at all.
	quality := 0.0
	for _, e := range in.Evidence.Items {
		if e.Source == SourceModel {
			continue
		}
		if w := e.Weight(); w > quality {
			quality = w
		}
	}
	if quality == 0 {
		return 0 // no deterministic evidence at all
	}

	// Reproducibility: repeated observations of the same problem.
	target := in.ReproTarget
	if target <= 0 {
		target = DefaultReproducibilityTarget
	}
	reps := in.Repetitions
	if reps < 1 {
		reps = 1
	}
	repro := math.Min(1, float64(reps)/float64(target))

	// Agreement between independent signal kinds.
	agreement := 1.0
	if in.SignalsTotal > 1 {
		agreement = float64(in.SignalsAgree) / float64(in.SignalsTotal)
	}

	// Weights are chosen so a single observation of the strongest signal
	// stays below the autonomous threshold (0.9 * (0.4+0.35/3+0.25) ≈ 0.69),
	// while repeated corroborated observations clear it (2 samples ≈ 0.79,
	// 3 samples ≈ 0.90).
	score := quality * (0.4 + 0.35*repro + 0.25*agreement)
	score -= 0.4 * float64(in.Conflicts)
	if score < 0 {
		score = 0
	}
	if in.ModelOnly && score > llmOnlyCap {
		score = llmOnlyCap
	}
	if score > 1 {
		score = 1
	}
	return score
}

// RiskLevel classifies how dangerous a proposed repair action is.
type RiskLevel int

const (
	RiskLow RiskLevel = iota
	RiskMedium
	RiskHigh
	RiskCritical
)

// String renders the level for logs and audit records.
func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	case RiskCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ActionKind enumerates repair actions the policy can approve or refuse.
type ActionKind string

const (
	ActionRestart         ActionKind = "restart"          // restart/recreate a service
	ActionManifestPatch   ActionKind = "manifest_patch"   // edit generated Dockerfile/compose
	ActionMultiManifest   ActionKind = "multi_manifest"   // edit several generated files
	ActionInfraChange     ActionKind = "infra_change"     // add/remove/reconfigure backing services
	ActionDataDestructive ActionKind = "data_destructive" // volume/data loss possible
)

// RiskInput describes a proposed action.
type RiskInput struct {
	Action          ActionKind
	FilesTouched    []string
	ServicesTouched []string
	// Reversible is true when a pristine snapshot of everything the action
	// modifies exists (healer manifest snapshots satisfy this).
	Reversible bool
	// DestructiveMarkers are signals found inside the patch content that
	// imply data loss (volume removals, DB drops, deletes).
	DestructiveMarkers []string
}

// destructivePatterns are content markers that make a patch data-destructive
// no matter what the model claims about safety. The list is deliberately
// conservative: a marker raises risk to review even when it might be benign
// in context.
var destructivePatterns = []string{
	"docker volume rm",
	"volume: null",
	"volumes: []",
	"drop table",
	"drop database",
	"rm -rf /",
	"truncate table",
	"--volumes",
	"down -v",
}

// DetectDestructiveMarkers scans patch text for data-loss markers.
func DetectDestructiveMarkers(content string) []string {
	low := strings.ToLower(content)
	var out []string
	for _, p := range destructivePatterns {
		if strings.Contains(low, p) {
			out = append(out, p)
		}
	}
	return out
}

// Assess scores the risk of the action.
func Assess(in RiskInput) RiskLevel {
	if len(in.DestructiveMarkers) > 0 || in.Action == ActionDataDestructive {
		return RiskCritical
	}
	switch in.Action {
	case ActionInfraChange:
		if !in.Reversible {
			return RiskHigh
		}
		return RiskMedium
	case ActionMultiManifest:
		if len(in.ServicesTouched) > 1 {
			return RiskHigh
		}
		return RiskMedium
	case ActionManifestPatch:
		if !in.Reversible {
			return RiskHigh
		}
		if len(in.FilesTouched) > 2 {
			return RiskMedium
		}
		return RiskLow
	case ActionRestart:
		return RiskLow
	default:
		return RiskMedium
	}
}

// Explain renders why a risk level was assigned.
func Explain(in RiskInput, level RiskLevel) string {
	var reasons []string
	if len(in.DestructiveMarkers) > 0 {
		reasons = append(reasons, "patch contains data-loss markers: "+strings.Join(in.DestructiveMarkers, ", "))
	}
	switch in.Action {
	case ActionDataDestructive:
		reasons = append(reasons, "action may destroy persistent data")
	case ActionInfraChange:
		reasons = append(reasons, "changes backing infrastructure")
	case ActionMultiManifest:
		reasons = append(reasons, fmt.Sprintf("touches %d generated files", len(in.FilesTouched)))
	case ActionManifestPatch:
		reasons = append(reasons, "edits generated manifests")
	case ActionRestart:
		reasons = append(reasons, "restarts a service (non-destructive)")
	}
	if len(in.ServicesTouched) > 1 {
		reasons = append(reasons, fmt.Sprintf("affects %d services", len(in.ServicesTouched)))
	}
	if in.Reversible {
		reasons = append(reasons, "reversible (pristine snapshot available)")
	} else {
		reasons = append(reasons, "not reversible")
	}
	if len(reasons) == 0 {
		return level.String() + " risk"
	}
	return level.String() + " risk: " + strings.Join(reasons, "; ")
}
