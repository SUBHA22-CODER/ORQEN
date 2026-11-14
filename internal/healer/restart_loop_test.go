package healer

import (
	"strings"
	"testing"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
)

func TestContainerHealth_DistinguishesCleanExitFromCrashLoop(t *testing.T) {
	// Case 1: Clean exit 0 in restart loop
	psClean := []domain.Container{
		{
			Service:  "orqen",
			State:    "restarting",
			ExitCode: 0,
		},
	}
	ready, fatal, restarting, detail := containerHealth(psClean)
	if ready {
		t.Error("ready should be false while restarting")
	}
	if fatal {
		t.Error("fatal should initially be false during grace period for clean exit")
	}
	if !restarting {
		t.Error("restarting must be true")
	}
	if !strings.Contains(detail, "exit 0") {
		t.Errorf("detail should note exit 0, got %q", detail)
	}

	// Case 2: Non-zero exit code crash loop
	psCrash := []domain.Container{
		{
			Service:  "api",
			State:    "restarting",
			ExitCode: 1,
		},
	}
	ready, fatal, restarting, detail = containerHealth(psCrash)
	if !fatal || !restarting {
		t.Errorf("non-zero exit in restarting should be flagged fatal immediately: fatal=%v restarting=%v", fatal, restarting)
	}
	if !strings.Contains(detail, "exit 1") {
		t.Errorf("detail should note exit 1, got %q", detail)
	}

	// Case 3: OOM killed container
	psOOM := []domain.Container{
		{
			Service:   "worker",
			State:     "exited",
			ExitCode:  137,
			OOMKilled: true,
		},
	}
	ready, fatal, restarting, detail = containerHealth(psOOM)
	if !fatal || ready {
		t.Errorf("OOM killed container must be fatal: fatal=%v ready=%v", fatal, ready)
	}
	if !strings.Contains(detail, "OOM") {
		t.Errorf("detail should mention OOM, got %q", detail)
	}
}
