package safety

import (
	"fmt"
	"regexp"
	"strings"
)

// Natural-language approval recognition: a deterministic classifier that maps
// judge utterances onto approval actions. It NEVER grants authorization by
// itself — it only classifies language. Every authorization decision goes
// through VerifyAuthorization, which checks identity, binding, expiry, and
// policy in code.
//
// The classifier distinguishes:
//   - explicit authorization ("I approve this high-risk repair")
//   - explicit rejection ("I reject this proposed modification")
//   - questions ("Can I approve ORQEN-101?") — never authorization
//   - hypotheticals ("If I approved ORQEN-101...") — never authorization
//   - quoted text ("He said 'approve it'") — never authorization
//   - diagnostics-only scopes ("Approve diagnostics only")
//   - ambiguous instructions — routed to clarification

// NLIntentKind is the classified approval action.
type NLIntentKind string

const (
	NLApprove     NLIntentKind = "approve"
	NLReject      NLIntentKind = "reject"
	NLShow        NLIntentKind = "show"
	NLInvestigate NLIntentKind = "investigate"
	NLUnclear     NLIntentKind = "unclear"
)

// NLScope records what the utterance actually authorizes. Approving
// diagnostics never authorizes a repair, and vice versa.
type NLScope string

const (
	NLScopeRepair      NLScope = "repair"
	NLScopeDiagnostics NLScope = "diagnostics"
	NLScopeUnspecified NLScope = ""
)

// NLApprovalIntent is the structured reading of one judge utterance.
type NLApprovalIntent struct {
	Kind         NLIntentKind `json:"kind"`
	Scope        NLScope      `json:"scope,omitempty"`
	ApprovalID   string       `json:"approval_id,omitempty"`
	Explicit     bool         `json:"explicit"`
	Hypothetical bool         `json:"hypothetical,omitempty"`
	Quoted       bool         `json:"quoted,omitempty"`
	Ambiguities  []string     `json:"ambiguities,omitempty"`
	Confidence   float64      `json:"confidence"`
}

// approval id patterns: ORQEN-101 style ids, the store's apr-hex ids, and
// bare numeric references after the word "approval".
var (
	orqenIDRe = regexp.MustCompile(`(?i)\bORQEN[- ]?(\d{1,6})\b`)
	aprIDRe   = regexp.MustCompile(`(?i)\b(apr-[0-9a-f]{6,16})\b`)
	bareIDRe  = regexp.MustCompile(`(?i)\bapproval\s+#?(\d{1,6})\b`)
)

var (
	// questionRe detects interrogative framing. Imperative negations ("do
	// not approve") are deliberately excluded: they are explicit rejections,
	// not questions.
	questionRe = regexp.MustCompile(`(?i)\?|^\s*(can|could|would|should|shall|may|is|are|what if|who|when|where|why|how (do|does|can|about))\b`)
	hypoRe     = regexp.MustCompile(`(?i)\b(if|suppose|assuming|in case|were i to|what if|hypothetically)\b`)
	quoteRe    = regexp.MustCompile(`"[^"]+"|'[^']+'|` + "`" + `[^` + "`" + `]+` + "`")

	approveMarkers = []string{
		"i approve", "approve this", "approve the", "approve approval", "approve orqen", "approve apr-",
		"approve it", "approve them", "approve now", "approve!",
		"proceed with approval", "authorized to proceed", "i authorize", "authorize the",
		"yes, apply", "yes apply", "go ahead and apply", "apply the approved", "sign off on",
	}
	rejectMarkers = []string{
		"i reject", "reject this", "reject the", "reject approval", "reject orqen", "reject apr-",
		"i deny", "deny this", "decline this", "decline the", "do not approve", "don't approve",
		"do not proceed", "don't proceed", "do not apply", "don't apply", "veto",
	}
	showMarkers = []string{
		"show me the changes", "show the changes", "show the diff", "what changed",
		"show me the approval", "show approval", "approval details", "before approval", "before i approve",
	}
	investigateMarkers = []string{
		"investigate further", "investigate more", "more investigation", "approve diagnostics",
		"diagnostics only", "diagnostic only", "approval for diagnostics", "keep investigating",
		"gather more evidence", "do not repair yet", "don't repair yet",
	}
	repairScopeMarkers = []string{
		"repair", "fix", "apply the change", "apply the patch", "execute the repair",
		"proposed modification", "proposed repair", "high-risk repair",
	}
)

// RecognizeApproval classifies one utterance deterministically.
func RecognizeApproval(text string) NLApprovalIntent {
	raw := strings.TrimSpace(text)
	res := NLApprovalIntent{Kind: NLUnclear, Scope: NLScopeUnspecified, Confidence: 0}
	if raw == "" {
		res.Ambiguities = append(res.Ambiguities, "empty instruction")
		return res
	}

	// Quoted text is evidence, never authorization.
	if m := quoteRe.FindString(raw); m != "" && strings.Contains(strings.ToLower(m), "approv") {
		res.Quoted = true
		res.Ambiguities = append(res.Ambiguities, "approval language appears inside quoted text: "+m)
	}
	// Analyze text outside quotes.
	analysis := quoteRe.ReplaceAllString(raw, " ")
	lower := strings.ToLower(analysis)

	// Questions and hypotheticals are never authorization, even when they
	// contain approval words ("Can I approve ORQEN-101?").
	if questionRe.MatchString(lower) {
		res.Hypothetical = true
		res.Ambiguities = append(res.Ambiguities, "phrased as a question; questions do not authorize")
	}
	if hypoRe.MatchString(lower) {
		res.Hypothetical = true
		res.Ambiguities = append(res.Ambiguities, "hypothetical framing; hypotheticals do not authorize")
	}

	// Approval id extraction.
	if m := orqenIDRe.FindStringSubmatch(raw); m != nil {
		res.ApprovalID = "ORQEN-" + m[1]
	} else if m := aprIDRe.FindStringSubmatch(raw); m != nil {
		res.ApprovalID = strings.ToLower(m[1])
	} else if m := bareIDRe.FindStringSubmatch(raw); m != nil {
		res.ApprovalID = "ORQEN-" + m[1]
	}

	// Scope: diagnostics-only language narrows the grant.
	diagOnly := containsAny(lower, investigateMarkers...)
	repairWord := containsAny(lower, repairScopeMarkers...)

	// Classify by the strongest imperative marker present.
	switch {
	case diagOnly && containsAny(lower, "investigate", "investigation", "diagnostics", "diagnostic", "evidence"):
		res.Kind = NLInvestigate
		res.Scope = NLScopeDiagnostics
		res.Explicit = !res.Hypothetical
		res.Confidence = 0.9
	case containsAny(lower, rejectMarkers...):
		res.Kind = NLReject
		res.Scope = NLScopeRepair
		res.Explicit = !res.Hypothetical
		res.Confidence = 0.9
		if diagOnly {
			res.Scope = NLScopeDiagnostics
		}
	case containsAny(lower, showMarkers...) && !containsAny(lower, approveMarkers...):
		res.Kind = NLShow
		res.Explicit = false
		res.Confidence = 0.85
	case containsAny(lower, approveMarkers...):
		res.Kind = NLApprove
		res.Explicit = !res.Hypothetical
		res.Confidence = 0.9
		res.Scope = NLScopeRepair
		if diagOnly && !repairWord {
			res.Kind = NLInvestigate
			res.Scope = NLScopeDiagnostics
		}
		if strings.Contains(lower, "diagnostics only") || strings.Contains(lower, "diagnostic only") || strings.Contains(lower, "approval for diagnostics") {
			res.Scope = NLScopeDiagnostics
		}
	default:
		res.Kind = NLUnclear
		res.Explicit = false
		res.Confidence = 0.2
		res.Ambiguities = append(res.Ambiguities, "no explicit approval or rejection instruction found")
		return res
	}

	// Hypothetical or quoted framing invalidates authorization language for
	// the action kinds that grant or deny something. Informational kinds
	// (show) keep their confidence.
	if res.Kind != NLShow {
		if res.Hypothetical {
			res.Explicit = false
			res.Confidence = 0.3
		}
		if res.Quoted {
			res.Explicit = false
			res.Confidence = 0.3
		}
	}
	return res
}

// BindIntent resolves the classified intent against the pending approval
// set: it binds an utterance without an explicit id to the single pending
// approval, and flags ambiguity when the target is unclear. Binding never
// grants authorization; it only resolves WHICH request the judge means.
func BindIntent(intent NLApprovalIntent, pending []*Approval) NLApprovalIntent {
	if intent.Kind == NLUnclear || intent.Kind == NLShow {
		return intent
	}
	if intent.ApprovalID != "" {
		return intent
	}
	switch {
	case len(pending) == 0:
		intent.Ambiguities = append(intent.Ambiguities, "no pending approval requests to bind this instruction to")
		intent.Kind = NLUnclear
		intent.Explicit = false
		intent.Confidence = 0.2
	case len(pending) == 1:
		intent.ApprovalID = pending[0].ApprovalID
	default:
		ids := make([]string, 0, len(pending))
		for _, a := range pending {
			ids = append(ids, a.ApprovalID)
		}
		intent.Ambiguities = append(intent.Ambiguities,
			fmt.Sprintf("multiple pending approvals (%s); name the approval id explicitly", strings.Join(ids, ", ")))
		intent.Kind = NLUnclear
		intent.Explicit = false
		intent.Confidence = 0.3
	}
	return intent
}
