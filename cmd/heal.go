package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
	"github.com/SUBHA22-CODER/ORQEN/internal/ui"
)

var healTries int

var healCmd = &cobra.Command{
	Use:   "heal [project]",
	Short: "Run the deterministic + LLM closed-loop repair",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runHeal,
}

func init() {
	healCmd.Flags().IntVar(&healTries, "tries", 3, "maximum repair attempts")
}

func runHeal(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	mgr, lock, err := resolveProject(name)
	if err != nil {
		return err
	}
	if err := requireDocker(ctx); err != nil {
		return err
	}
	if err := writeProjectEnv(mgr, lock); err != nil {
		return err
	}
	printEnvStatus(mgr, lock)
	var client domain.LLMClient
	if c, lerr := llmFor(true); lerr == nil {
		client = c
	} else {
		logf("%s", ui.WarningLine("LLM unavailable ("+lerr.Error()+"); deterministic repair only"))
	}
	h, err := healerFor(mgr, lock, client)
	if err != nil {
		return err
	}
	if h.Safety != nil {
		// --yes keeps its existing meaning: auto-approve destructive actions.
		// Otherwise, interactive sessions get a plain-language approval prompt;
		// non-interactive runs refuse risky repairs.
		if flagYes {
			setApprovalMode(h.Safety, "auto")
		} else {
			setApprovalMode(h.Safety, "prompt")
		}
	}
	h.MaxTries = healTries
	res, herr := h.Heal(ctx, "", "")
	if herr != nil {
		printFailureReport(mgr, lock, res, herr)
		return herr
	}
	clearFailureLog(mgr)
	logf("%s", ui.SuccessLine(res.Summary))
	waitForHealthy(ctx, mgr, lock, 60*time.Second)
	showStatus(ctx, mgr, lock)
	printURLs(ctx, mgr, lock)
	return nil
}
