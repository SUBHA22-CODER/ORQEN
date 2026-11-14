package safety

import (
	"fmt"
	"sort"
	"strings"
)

// Claim is one falsifiable assertion inside an AI-generated diagnosis. Claims
// are extracted from model output by the gate; they are treated as hypotheses,
// never as proof.
type Claim struct {
	// About is the entity the claim speaks about: a service name, an
	// infrastructure name, or "stack".
	About string `json:"about"`
	// Statement is the assertion, e.g. "postgres is not running",
	// "the api cannot reach redis", "the build lacks dependency X".
	Statement string `json:"statement"`
	// Kind narrows the assertion for machine checking: state | connectivity |
	// resource | configuration | dependency | other.
	Kind string `json:"kind,omitempty"`
	// Asserted is what the model says is true: "true" (up/healthy/working) or
	// "false" (down/broken). Empty means the statement is not a boolean state
	// claim and cannot be cross-checked automatically.
	Asserted string `json:"asserted,omitempty"`
	// Evidence the model cites for the claim; never counted as proof.
	CitedEvidence string `json:"cited_evidence,omitempty"`
}

// Diagnosis is the structured AI root-cause hypothesis under review.
type Diagnosis struct {
	Claims     []Claim `json:"claims,omitempty"`
	Raw        string  `json:"raw,omitempty"`
	SourceKind string  `json:"source_kind,omitempty"` // "llm" | "deterministic"
}

// Conflict is one place where a diagnosis contradicts deterministic evidence.
type Conflict struct {
	Claim       Claim    `json:"claim"`
	Observed    string   `json:"observed"` // human-readable contradicting evidence
	Evidence    Evidence `json:"evidence"`
	Explanation string   `json:"explanation"`
}

// Validator cross-checks diagnosis claims against a deterministic evidence set.
// Its single rule: a claim that contradicts observed evidence is rejected; a
// claim with no contradicting evidence remains merely unverified and never
// raises confidence on its own.
type Validator struct{}

// CrossCheck returns every conflict between the diagnosis and the evidence.
// An empty result means no contradictions were found — it does NOT mean the
// diagnosis is confirmed.
func (Validator) CrossCheck(d Diagnosis, ev Set) []Conflict {
	var out []Conflict
	for _, claim := range d.Claims {
		if c, ok := crossCheckClaim(claim, ev); ok {
			out = append(out, c)
		}
	}
	return out
}

// crossCheckClaim tests one claim against evidence. Only boolean state claims
// (Asserted "true"/"false") are automatically falsifiable; everything else is
// passed through unverified rather than guessed at.
func crossCheckClaim(c Claim, ev Set) (Conflict, bool) {
	asserted := strings.ToLower(strings.TrimSpace(c.Asserted))
	if asserted != "true" && asserted != "false" {
		return Conflict{}, false
	}
	wantUp := asserted == "true"

	target := strings.ToLower(strings.TrimSpace(c.About))
	var about []Evidence
	for _, e := range ev.Items {
		if matchesClaimTarget(e, target, c.About) {
			about = append(about, e)
		}
	}
	if len(about) == 0 {
		return Conflict{}, false // nothing observed about this entity: unverified, not contradicted
	}

	// Pick the strongest (highest-weight) observation for the entity.
	strongest := about[0]
	for _, e := range about[1:] {
		if e.Weight() > strongest.Weight() {
			strongest = e
		}
	}

	// Contradiction: claim says up, strongest evidence says broken; or claim
	// says down, strongest evidence says healthy.
	if wantUp && strongest.Signature != "" {
		return Conflict{
			Claim:       c,
			Observed:    strongest.String(),
			Evidence:    strongest,
			Explanation: fmt.Sprintf("diagnosis claims %q is healthy/working, but observed evidence contradicts it: %s", c.About, strongest.String()),
		}, true
	}
	if !wantUp && strongest.Healthy && strongest.Signature == "" {
		return Conflict{
			Claim:       c,
			Observed:    strongest.String(),
			Evidence:    strongest,
			Explanation: fmt.Sprintf("diagnosis claims %q is down/broken, but observed evidence shows it healthy: %s", c.About, strongest.String()),
		}, true
	}
	return Conflict{}, false
}

// matchesClaimTarget reports whether evidence e speaks about the claimed
// entity. Claims may name a service directly or use "the <name>" phrasing.
func matchesClaimTarget(e Evidence, target, raw string) bool {
	if target == "" || target == "stack" || target == "the stack" {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimPrefix(name, "the ")
	return strings.EqualFold(e.Service, name) ||
		strings.EqualFold(e.ID, "container:"+name) ||
		strings.EqualFold(e.ID, "dependency:"+name) ||
		strings.EqualFold(e.ID, "http:"+name) ||
		strings.EqualFold(e.ID, "log:"+name) ||
		strings.EqualFold(e.ID, "resource:"+name)
}

// ExtractClaims turns free-form diagnosis text into candidate claims. It is
// deliberately conservative: only explicit entity-state patterns become
// checkable claims, because a false extraction would manufacture a conflict
// out of nothing. Sentences like "postgres is running" or "api is down" are
// recognized; everything else stays raw.
func ExtractClaims(text string) []Claim {
	var out []Claim
	for _, sentence := range splitSentences(text) {
		low := strings.ToLower(sentence)
		about, ok := leadingEntity(low)
		if !ok {
			continue
		}
		claim := Claim{About: about, Statement: strings.TrimSpace(sentence), Kind: "state"}
		switch {
		case containsAny(low, "is running", "is up", "is healthy", "is working", "is reachable"):
			claim.Asserted = "true"
		case containsAny(low, "is down", "is not running", "is stopped", "is dead", "is crashed",
			"is unreachable", "is not reachable", "is unhealthy", "is not responding", "is failing"):
			claim.Asserted = "false"
		default:
			continue
		}
		out = append(out, claim)
	}
	return out
}

func splitSentences(text string) []string {
	cleaned := strings.NewReplacer("\n", ". ", "; ", ". ").Replace(text)
	parts := strings.Split(cleaned, ".")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// leadingEntity extracts the subject of a sentence when it names a service or
// a stack noun, skipping the leading article.
func leadingEntity(low string) (string, bool) {
	words := strings.Fields(low)
	if len(words) == 0 {
		return "", false
	}
	first := strings.Trim(words[0], `,()'"`)
	if first == "the" || first == "service" || first == "container" {
		if len(words) < 2 {
			return "", false
		}
		first = strings.Trim(words[1], `,()'"`)
	}
	if first == "" {
		return "", false
	}
	return first, true
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ConflictsSummary renders conflicts for logs, audit, and LLM feedback rounds.
func ConflictsSummary(conflicts []Conflict) string {
	if len(conflicts) == 0 {
		return ""
	}
	var lines []string
	for _, c := range conflicts {
		lines = append(lines, "- "+c.Explanation)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
