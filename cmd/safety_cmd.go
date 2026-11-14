package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
	"github.com/SUBHA22-CODER/ORQEN/internal/ui"
)

// Safety Score Assessment and Testing CLI:
//   orqen assess <project> [--dry-run]
//   orqen safety-test [safe|high-risk|insufficient-evidence]
//   orqen approvals list|show|approve|reject
//
// All scores are computed by the safety engine from evidence and the proposed
// action only; no LLM participates in scoring.

var (
	assessDryRun     bool
	approvalsProject string
	approvalsReason  string
	approvalsAsJSON  bool
)

var assessCmd = &cobra.Command{
	Use:   "assess <project>",
	Short: "Assess diagnostic confidence, repair risk, and evidence sufficiency",
	Args:  cobra.ExactArgs(1),
	RunE:  runAssess,
}

var safetyTestCmd = &cobra.Command{
	Use:   "safety-test [safe|high-risk|insufficient-evidence]",
	Short: "Run deterministic safety scenarios and report PASS/FAIL/SKIPPED",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runSafetyTest,
}

var approvalsCmd = &cobra.Command{
	Use:   "approvals",
	Short: "Manage judge approval requests",
}

func init() {
	assessCmd.Flags().BoolVar(&assessDryRun, "dry-run", false,
		"assess the deterministic fixture instead of live containers (no Docker needed)")
	rootCmd.AddCommand(assessCmd)
	rootCmd.AddCommand(safetyTestCmd)
	rootCmd.AddCommand(approvalsCmd)

	approvalsCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List approval requests",
		RunE:  func(cmd *cobra.Command, args []string) error { return runApprovalsList(approvalsProject) },
	})
	approvalsCmd.AddCommand(&cobra.Command{
		Use:   "show <approval-id>",
		Short: "Show full approval details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalsShow(args[0], approvalsAsJSON)
		},
	})
	approvalsCmd.AddCommand(&cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve the exact recorded action (one-time, hash-bound)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalsDecide(args[0], true)
		},
	})
	approvalsCmd.AddCommand(&cobra.Command{
		Use:   "reject <approval-id>",
		Short: "Reject the approval request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApprovalsDecide(args[0], false)
		},
	})
	approvalsCmd.PersistentFlags().StringVar(&approvalsAsUser, "as", "", "act as this authenticated user (admin/testing)")
	approvalsCmd.PersistentFlags().StringVar(&approvalsProject, "project", "", "filter by project id")
	approvalsCmd.PersistentFlags().StringVar(&approvalsReason, "reason", "", "reason recorded with the decision")
	approvalsCmd.PersistentFlags().BoolVar(&approvalsAsJSON, "json", false, "print raw JSON")
}

// --- orqen assess -----------------------------------------------------------

func runAssess(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if assessDryRun {
		return runAssessDryRun()
	}

	mgr, lock, err := resolveProject(args[0])
	if err != nil {
		return err
	}
	if err := requireDocker(ctx); err != nil {
		return err
	}

	col := &safety.Collector{
		Compose:  composeFor(mgr),
		Prober:   probeFor(),
		Services: lock.Services,
		PortMap:  lock.PortMap,
	}
	ev, err := col.Collect(ctx)
	if err != nil {
		return err
	}
	report := assessReportFor(ev, 1, lock)
	printAssessment("LIVE assessment of "+mgr.ID, report)
	return nil
}

// runAssessDryRun assesses the deterministic fixture instead of live
// containers: same inputs, same scores, same decision, no Docker, and never
// any modification of real infrastructure.
func runAssessDryRun() error {
	s, ok := safety.SafetyScenarios()["safe"]
	if !ok {
		return fmt.Errorf("safe fixture missing")
	}
	report := safety.AssessEvidence(s.Evidence, s.Repetitions, s.RiskInput)
	printAssessment("DRY-RUN assessment (deterministic fixture — nothing touched)", report)
	return nil
}

// assessReportFor computes the assessment for a live evidence set. The
// prospective action is the baseline manifest repair across the project's app
// services — clearly labeled as prospective, since no patch exists yet.
func assessReportFor(ev safety.Set, reps int, lock *domain.Lock) safety.ScoreReport {
	files := []string{"docker-compose.yml"}
	services := []string{}
	for _, s := range lock.Services {
		if s != nil && !s.IsWorker {
			files = append(files, "Dockerfile."+s.Name)
			services = append(services, s.Name)
		}
	}
	riskIn := safety.RiskInput{
		Action:          safety.ActionMultiManifest,
		FilesTouched:    files,
		ServicesTouched: services,
		Reversible:      true,
	}
	return safety.AssessEvidence(ev, reps, riskIn)
}

func printAssessment(header string, report safety.ScoreReport) {
	fmt.Println(ui.TitleStyle.Render("SAFETY ASSESSMENT — " + header))
	fmt.Println()

	fmt.Println(ui.Section("Evidence collected"))
	if len(report.EvidenceLines) == 0 {
		fmt.Println(ui.MutedStyle.Render("  (none)"))
	} else {
		for _, line := range report.EvidenceLines {
			fmt.Println("  " + line)
		}
	}
	fmt.Println()

	fmt.Println(ui.Section("Scores"))
	fmt.Printf("  Diagnostic confidence : %3d/100  (%s)\n", report.Confidence, report.ConfidenceTier)
	fmt.Printf("  Repair risk           : %3d/100  (%s)\n", report.Risk, report.RiskLabel)
	fmt.Printf("  Evidence sufficiency  : %3d/100  (%s)  [measured separately]\n", report.Sufficiency, report.SufficiencyTier)
	fmt.Println()

	fmt.Println(ui.Section("Evidence influencing confidence"))
	for _, f := range report.ConfidenceFactors {
		fmt.Println("  - " + f)
	}
	fmt.Println()

	fmt.Println(ui.Section("Risk factors"))
	for _, f := range report.RiskFactors {
		fmt.Println("  - " + f)
	}
	fmt.Println()

	fmt.Println(ui.Section("Evidence sufficiency factors"))
	for _, f := range report.SufficiencyFactors {
		fmt.Println("  - " + f)
	}
	fmt.Println()

	fmt.Println(ui.Section("Policy decision"))
	fmt.Println("  Decision: " + ui.TitleStyle.Render(report.Decision.String()))
	fmt.Println("  Why: " + report.DecisionReason)
	fmt.Println()
	fmt.Println(ui.MutedStyle.Render("  Assessment is read-only: nothing was modified."))
}

// --- orqen safety-test ------------------------------------------------------

func runSafetyTest(cmd *cobra.Command, args []string) error {
	names := args
	if len(names) == 1 && strings.EqualFold(names[0], "all") {
		names = nil
	}
	summary := safety.RunSafetyScenarios(names...)

	fmt.Println(ui.TitleStyle.Render("ORQEN SAFETY TEST — deterministic scenarios"))
	fmt.Println()
	for _, r := range summary.Results {
		status := r.Status
		switch status {
		case "PASS":
			status = ui.SuccessStyle.Render("PASS")
		case "FAIL":
			status = ui.ErrorStyle.Render("FAIL")
		default:
			status = ui.WarnStyle.Render("SKIPPED")
		}
		fmt.Printf("%s  %s\n", status, r.Name)
		if r.Status == "SKIPPED" {
			fmt.Println("        " + r.Detail)
			continue
		}
		fmt.Println("        " + r.Description)
		fmt.Printf("        confidence %d/100 (%s) | risk %d/100 (%s) | sufficiency %d/100 (%s)\n",
			r.Report.Confidence, r.Report.ConfidenceTier,
			r.Report.Risk, r.Report.RiskLabel,
			r.Report.Sufficiency, r.Report.SufficiencyTier)
		fmt.Println("        decision: " + r.Report.Decision.String() + " (expected " + r.Expected.String() + ")")
		fmt.Println("        why: " + r.Report.DecisionReason)
		if r.Detail != "" {
			fmt.Println("        " + r.Detail)
		}
		if !r.Pass {
			fmt.Println("        expectation: " + r.Expectation)
		}
	}
	fmt.Println()
	fmt.Printf("RESULTS: %d passed, %d failed, %d skipped\n", summary.Passed, summary.Failed, summary.Skipped)
	if summary.Failed > 0 {
		os.Exit(1)
	}
	return nil
}

// --- orqen approvals --------------------------------------------------------

func runApprovalsList(projectID string) error {
	store := sharedApprovalStore()
	store.CancelExpired()
	all := store.List("", projectID)
	if len(all) == 0 {
		fmt.Println(ui.MutedStyle.Render("No approval requests."))
		return nil
	}
	fmt.Println(ui.TitleStyle.Render("APPROVALS"))
	fmt.Printf("%-22s %-10s %-14s %-6s %-40s %s\n", "ID", "STATUS", "RISK", "CONF", "ACTION", "EXPIRES")
	for _, a := range all {
		expiry := "-"
		if !a.ExpiresAt.IsZero() {
			expiry = a.ExpiresAt.Format("2006-01-02 15:04")
		}
		fmt.Printf("%-22s %-10s %-14s %-6d %-40s %s\n",
			a.ApprovalID, a.Status, a.RiskLevel, int(a.ConfidenceScore*100), truncate40(a.ActionSummary), expiry)
	}
	return nil
}

func truncate40(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 40 {
		return s[:37] + "..."
	}
	return s
}

func runApprovalsShow(id string, asJSON bool) error {
	store := sharedApprovalStore()
	a, ok := store.Get(id)
	if !ok {
		return fmt.Errorf("approval %s not found", id)
	}
	if asJSON {
		data, err := json.MarshalIndent(a, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Println(ui.TitleStyle.Render("APPROVAL " + a.ApprovalID))
	fmt.Printf("  Status      : %s\n", a.Status)
	fmt.Printf("  Project     : %s\n", a.ProjectID)
	if a.SessionID != "" {
		fmt.Printf("  Session     : %s\n", a.SessionID)
	}
	fmt.Printf("  Action      : %s (%s)\n", a.ActionSummary, a.ActionType)
	fmt.Printf("  Risk        : %s\n", a.RiskLevel)
	fmt.Printf("  Confidence  : %s (%.2f)\n", a.ConfidenceTier, a.ConfidenceScore)
	fmt.Printf("  Uncertain   : %v\n", a.Uncertain)
	fmt.Printf("  Simulated   : %v\n", a.Simulated)
	fmt.Printf("  Files       : %s\n", strings.Join(a.AffectedFiles, ", "))
	fmt.Printf("  Services    : %s\n", strings.Join(a.AffectedServices, ", "))
	fmt.Printf("  Action hash : %s\n", a.ActionHash)
	if !a.ExpiresAt.IsZero() {
		fmt.Printf("  Expires     : %s\n", a.ExpiresAt.Format(time.RFC3339))
	}
	if len(a.Evidence) > 0 {
		fmt.Println("  Evidence:")
		for _, e := range a.Evidence {
			fmt.Println("    - " + e)
		}
	}
	if a.ProposedDiff != "" {
		fmt.Println("  Proposed diff:")
		fmt.Println(indent(a.ProposedDiff, "    "))
	}
	if a.DecidedBy != "" {
		fmt.Printf("  Decision    : %s by %s at %s\n", a.Decision, a.DecidedBy, a.DecidedAt.Format(time.RFC3339))
	}
	if a.RejectionReason != "" {
		fmt.Printf("  Rejection   : %s\n", a.RejectionReason)
	}
	if a.VerificationResult != "" {
		fmt.Printf("  Verification: %s\n", a.VerificationResult)
	}
	if a.RollbackResult != "" {
		fmt.Printf("  Rollback    : %s\n", a.RollbackResult)
	}
	return nil
}

// runApprovalsDecide approves or rejects. Authorization is verified in code:
// the authenticated identity must hold judge/reviewer permissions, and the
// decision is bound to the exact recorded action hash - the CLI authorizes
// exactly the stored proposal, never a class of actions, and the store
// enforces one-time use, project/session scope, and expiry.
func runApprovalsDecide(id string, approved bool) error {
	store := sharedApprovalStore()
	identity := currentJudgeIdentity()

	// Full authorization preflight: identity, permissions, pending state,
	// expiry, bindings, and safety-policy revalidation.
	a, err := safety.VerifyAuthorization(store, safety.VerifyRequest{
		Identity:      identity,
		ApprovalID:    id,
		RequiredScope: safety.DecisionRepair,
	})
	if err != nil {
		return err
	}

	verbLine := ui.ErrorStyle.Render("REJECT")
	if approved {
		verbLine = ui.SuccessStyle.Render("APPROVE")
	}
	fmt.Printf("%s %s\n", verbLine, ui.TitleStyle.Render(a.ActionSummary))
	fmt.Printf("  judge=%s (%s) project=%s risk=%s confidence=%s action_hash=%s\n",
		identity.User, identity.Role, a.ProjectID, a.RiskLevel, a.ConfidenceTier, shortHash16(a.ActionHash))

	d := safety.ApprovalDecision{
		ApprovalID: id,
		Decision:   safety.DecisionRepair,
		Approved:   approved,
		ActionHash: a.ActionHash, // bound to the exact recorded action
		ProjectID:  a.ProjectID,
		SessionID:  a.SessionID,
		DecidedBy:  identity.User + " (cli)",
		Reason:     approvalsReason,
	}
	res, err := store.Decide(d)
	if err != nil {
		return err
	}
	// Judge feedback: verify, authorize, record.
	fmt.Printf("Approval verified for %s.\n", res.ApprovalID)
	fmt.Printf("Authorized action: %s.\n", res.ActionSummary)
	fmt.Println("Action hash verified.")
	fmt.Println("Safety checks passed.")
	if approved {
		fmt.Println("Decision recorded: APPROVED. The paused repair pipeline revalidates and executes the approved action; health results are reported on completion.")
	} else {
		fmt.Println("Decision recorded: REJECTED. The proposed modification was NOT executed.")
	}
	if !approved && approvalsReason != "" {
		fmt.Println(ui.MutedStyle.Render("  reason: " + approvalsReason))
	}
	return nil
}

func shortHash16(h string) string {
	if len(h) > 16 {
		return h[:16] + "..."
	}
	return h
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
