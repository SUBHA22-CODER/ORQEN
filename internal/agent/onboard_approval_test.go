package agent

import (
	"context"
	"strings"
	"testing"
)

func TestOnboardingApproval_AllThreeStates(t *testing.T) {
	ctx := context.Background()

	// State 1: Approval Granted
	t.Run("ApprovalGranted", func(t *testing.T) {
		onboardCalled := false
		ag, err := New(Options{
			AutoApprove: false,
			Confirm: func(tool string, destructive bool, args map[string]any) bool {
				if tool == "onboard_repository" {
					return true
				}
				return false
			},
			Onboard: func(ctx context.Context, target string, maxServices int, noRun bool, progress func(string)) (*OnboardResult, error) {
				onboardCalled = true
				return &OnboardResult{Project: "test-proj"}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close()

		res, err := ag.RunIntent(ctx, "onboard https://github.com/acme/app", IntentContext{})
		if err != nil {
			t.Fatal(err)
		}
		if !onboardCalled {
			t.Fatal("expected Onboard callback to be invoked when approval is granted")
		}
		if res.State == StateFailed {
			t.Fatalf("expected success, got state=%s final=%q", res.State, res.Final)
		}
	})

	// State 2: Approval Denied
	t.Run("ApprovalDenied", func(t *testing.T) {
		onboardCalled := false
		deniedEventSeen := false
		ag, err := New(Options{
			AutoApprove: false,
			Confirm: func(tool string, destructive bool, args map[string]any) bool {
				return false // user clicks No / Deny
			},
			Onboard: func(ctx context.Context, target string, maxServices int, noRun bool, progress func(string)) (*OnboardResult, error) {
				onboardCalled = true
				return &OnboardResult{}, nil
			},
			EventHandler: func(ev Event) {
				if ev.Type == "permission_denied" && ev.Tool == "onboard_repository" {
					deniedEventSeen = true
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close()

		res, err := ag.RunIntent(ctx, "onboard https://github.com/acme/app", IntentContext{})
		if err != nil {
			t.Fatal(err)
		}
		if onboardCalled {
			t.Fatal("SECURITY VIOLATION: Onboard was called despite user denial!")
		}
		if !deniedEventSeen {
			t.Fatal("expected permission_denied event to be emitted on denial")
		}
		if res.State != StateFailed {
			t.Errorf("expected state failed on denial, got %s", res.State)
		}
		if !strings.Contains(res.Final, "Onboarding was not approved") || !strings.Contains(res.Final, "permission denied") {
			t.Errorf("expected informative denial message, got %q", res.Final)
		}
	})

	// State 3: Approval Callback Unavailable (nil Confirm)
	t.Run("ApprovalCallbackUnavailable", func(t *testing.T) {
		onboardCalled := false
		deniedEventSeen := false
		ag, err := New(Options{
			AutoApprove: false,
			Confirm:     nil, // no callback provided
			Onboard: func(ctx context.Context, target string, maxServices int, noRun bool, progress func(string)) (*OnboardResult, error) {
				onboardCalled = true
				return &OnboardResult{}, nil
			},
			EventHandler: func(ev Event) {
				if ev.Type == "permission_denied" && ev.Tool == "onboard_repository" {
					deniedEventSeen = true
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close()

		res, err := ag.RunIntent(ctx, "onboard https://github.com/acme/app", IntentContext{})
		if err != nil {
			t.Fatal(err)
		}
		if onboardCalled {
			t.Fatal("SECURITY VIOLATION: Onboard was called when confirm callback was unavailable!")
		}
		if !deniedEventSeen {
			t.Fatal("expected permission_denied event when confirmation is unavailable")
		}
		if res.State != StateFailed {
			t.Errorf("expected state failed, got %s", res.State)
		}
		if !strings.Contains(res.Final, "no confirmation callback is available") {
			t.Errorf("expected explanation about unavailable callback, got %q", res.Final)
		}
	})
}
