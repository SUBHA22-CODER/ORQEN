package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

// Natural-language judge approval through the agent chat. The LLM only
// ROUTES the utterance here via intent detection; the approval decision is
// made exclusively by deterministic code: RecognizeApproval classifies the
// language, VerifyAuthorization checks identity/binding/policy, and the
// approval store performs the atomic decision.

// handleApprovalIntent executes one natural-language approval instruction.
func (a *Agent) handleApprovalIntent(ctx context.Context, it Intent) (string, error) {
	if a.opts.Approvals == nil {
		return "The judge approval backend is not available in this session. Use `orqen approvals approve <id>` instead.", nil
	}
	na := safety.NLApprovalIntent{Kind: safety.NLUnclear}
	if it.Approval != nil {
		na = *it.Approval
	} else if it.Raw != "" {
		na = safety.RecognizeApproval(it.Raw)
	}

	// Bind the instruction to a concrete pending approval when the judge did
	// not name one. Binding resolves WHICH request; it grants nothing.
	na = safety.BindIntent(na, a.opts.Approvals.List(safety.ApprovalPending, ""))

	// Authorization gate: only explicit, non-hypothetical, non-quoted
	// instructions may proceed to verification and decision. Everything else
	// routes to clarification.
	if na.Kind != safety.NLUnclear && na.Kind != safety.NLShow && !na.Explicit {
		na.Kind = safety.NLUnclear
		if len(na.Ambiguities) == 0 {
			na.Ambiguities = append(na.Ambiguities, "the instruction is not an explicit authorization")
		}
	}

	switch na.Kind {
	case safety.NLUnclear:
		var b strings.Builder
		b.WriteString("I need a clearer instruction before I can act on approvals.\n")
		for _, amb := range na.Ambiguities {
			b.WriteString("- " + amb + "\n")
		}
		b.WriteString("You can say, for example: `approve ORQEN-101`, `reject this repair`, `show me the changes before approval`, or `approve diagnostics only`.")
		return b.String(), nil

	case safety.NLShow:
		return a.showApprovalDetails(na.ApprovalID), nil

	case safety.NLInvestigate:
		return a.decideApproval(ctx, na, safety.DecisionInvestigate)

	case safety.NLReject:
		return a.rejectApproval(na)

	case safety.NLApprove:
		if na.Scope == safety.NLScopeDiagnostics {
			return a.decideApproval(ctx, na, safety.DecisionInvestigate)
		}
		return a.decideApproval(ctx, na, safety.DecisionRepair)
	}
	return "Unclear approval instruction.", nil
}

// rejectApproval verifies the judge and records the refusal. The proposed
// modification is never executed; the feedback says so explicitly.
func (a *Agent) rejectApproval(na safety.NLApprovalIntent) (string, error) {
	v := safety.VerifyRequest{
		Identity:      a.opts.Judge,
		ApprovalID:    na.ApprovalID,
		RequiredScope: safety.DecisionRepair,
	}
	a2, err := safety.VerifyAuthorization(a.opts.Approvals, v)
	if err != nil {
		return "", err
	}
	decided, derr := a.opts.Approvals.Decide(safety.ApprovalDecision{
		ApprovalID: a2.ApprovalID,
		Approved:   false,
		ActionHash: a2.ActionHash,
		ProjectID:  a2.ProjectID,
		SessionID:  a2.SessionID,
		DecidedBy:  a.opts.Judge.User + " (chat)",
		Reason:     "rejected via natural-language instruction",
	})
	if derr != nil {
		return "", derr
	}
	return fmt.Sprintf("Approval verified for %s.\nDecision recorded: REJECTED by %s.\nThe proposed modification was NOT executed.", decided.ApprovalID, decided.DecidedBy), nil
}

// decideApproval runs the authorization checklist and records the ruling.
// Only deterministic code decides; the LLM played no part in authorization.
func (a *Agent) decideApproval(ctx context.Context, na safety.NLApprovalIntent, scope safety.DecisionKind) (string, error) {
	v := safety.VerifyRequest{
		Identity:      a.opts.Judge,
		ApprovalID:    na.ApprovalID,
		RequiredScope: scope,
	}
	// Approve path: full verification, then the atomic decision. Execution
	// belongs to the repair pipeline that created the request (it holds the
	// exact proposed bytes and revalidates the hash before applying).
	a2, err := safety.VerifyAuthorization(a.opts.Approvals, v)
	if err != nil {
		return "", err
	}
	decided, derr := a.opts.Approvals.Decide(safety.ApprovalDecision{
		ApprovalID: a2.ApprovalID,
		Decision:   scope,
		Approved:   true,
		ActionHash: a2.ActionHash,
		ProjectID:  a2.ProjectID,
		SessionID:  a2.SessionID,
		DecidedBy:  a.opts.Judge.User + " (chat)",
	})
	if derr != nil {
		return "", derr
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Approval verified for %s.\n", decided.ApprovalID)
	fmt.Fprintf(&b, "Authorized action: %s.\n", decided.ActionSummary)
	fmt.Fprintf(&b, "Action hash verified.\n")
	fmt.Fprintf(&b, "Safety checks passed.\n")
	if decided.Decision == safety.DecisionInvestigate {
		fmt.Fprintf(&b, "Diagnostics authorized only: no repair will run. Investigation continues.")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "Executing approved repair: the paused repair pipeline will revalidate the hash, run sandbox checks, apply exactly the approved change, and verify health.")
	return b.String(), nil
}

// showApprovalDetails renders one approval (or the pending list).
func (a *Agent) showApprovalDetails(id string) string {
	if id == "" {
		pending := a.opts.Approvals.List(safety.ApprovalPending, "")
		if len(pending) == 0 {
			return "No pending approval requests."
		}
		var b strings.Builder
		b.WriteString("Pending approvals:\n")
		for _, p := range pending {
			fmt.Fprintf(&b, "- %s [%s] risk=%s confidence=%s: %s\n",
				p.ApprovalID, p.Status, p.RiskLevel, p.ConfidenceTier, p.ActionSummary)
		}
		b.WriteString("Say `show me the changes for <id>` or `approve <id>` to proceed.")
		return b.String()
	}
	ap, ok := a.opts.Approvals.Get(id)
	if !ok {
		return fmt.Sprintf("Approval %s not found.", id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Approval %s [%s]\n", ap.ApprovalID, ap.Status)
	fmt.Fprintf(&b, "Action: %s (%s)\n", ap.ActionSummary, ap.ActionType)
	fmt.Fprintf(&b, "Risk: %s | Confidence: %s (%.2f)\n", ap.RiskLevel, ap.ConfidenceTier, ap.ConfidenceScore)
	if len(ap.Evidence) > 0 {
		b.WriteString("Evidence:\n")
		for _, e := range ap.Evidence {
			b.WriteString("  - " + e + "\n")
		}
	}
	if ap.ProposedDiff != "" {
		b.WriteString("Proposed diff:\n" + ap.ProposedDiff + "\n")
	}
	fmt.Fprintf(&b, "Action hash: %s\n", shortHashN(ap.ActionHash, 16))
	return b.String()
}

func shortHashN(h string, n int) string {
	if len(h) > n {
		return h[:n] + "..."
	}
	return h
}
