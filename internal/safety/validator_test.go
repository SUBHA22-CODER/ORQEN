package safety

import (
	"strings"
	"testing"
	"time"
)

func evidenceSet(items ...Evidence) Set {
	return Set{At: time.Now().UTC(), Items: items}
}

func TestCrossCheckRejectsFalseDownClaim(t *testing.T) {
	// Evidence: postgres is running and healthy.
	ev := evidenceSet(Evidence{
		ID: "container:postgres", Kind: KindContainer, Source: SourceDocker,
		Service: "postgres", Healthy: true, Detail: "state=running health=healthy",
	})
	v := Validator{}
	d := Diagnosis{Raw: "postgres is not running; restart it", Claims: []Claim{
		{About: "postgres", Statement: "postgres is not running", Asserted: "false"},
	}}
	conflicts := v.CrossCheck(d, ev)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(conflicts), conflicts)
	}
	if !strings.Contains(conflicts[0].Explanation, "down/broken") {
		t.Fatalf("unexpected explanation: %s", conflicts[0].Explanation)
	}
	if !strings.Contains(conflicts[0].Explanation, "healthy") {
		t.Fatalf("explanation must cite the contradicting evidence: %s", conflicts[0].Explanation)
	}
}

func TestCrossCheckRejectsFalseHealthyClaim(t *testing.T) {
	// Evidence: api is crash-looping.
	ev := evidenceSet(Evidence{
		ID: "container:api", Kind: KindContainer, Source: SourceDocker,
		Service: "api", Healthy: false, Detail: "state=restarting exit=1",
		Signature: "restart_loop:api:exit=1",
	})
	v := Validator{}
	d := Diagnosis{Claims: []Claim{
		{About: "api", Statement: "the api is healthy, nothing is wrong", Asserted: "true"},
	}}
	if conflicts := v.CrossCheck(d, ev); len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
}

func TestCrossCheckUnverifiedClaimIsNotConflict(t *testing.T) {
	// No evidence exists about redis: the claim stays unverified, which must
	// NOT be counted as a contradiction (manufacturing conflicts would be its
	// own false positive).
	ev := evidenceSet(Evidence{
		ID: "container:api", Kind: KindContainer, Source: SourceDocker,
		Service: "api", Healthy: true,
	})
	v := Validator{}
	d := Diagnosis{Claims: []Claim{
		{About: "redis", Statement: "redis is down", Asserted: "false"},
	}}
	if conflicts := v.CrossCheck(d, ev); len(conflicts) != 0 {
		t.Fatalf("unverified claim must not conflict, got %d", len(conflicts))
	}
}

func TestCrossCheckStrongestEvidenceWins(t *testing.T) {
	// Weakest (log) says broken, strongest (container state) says healthy:
	// the conflict decision must use the container state.
	ev := evidenceSet(
		Evidence{
			ID: "log:api", Kind: KindLog, Source: SourceLog, Service: "api",
			Healthy: false, Signature: "log_exit_1:api",
		},
		Evidence{
			ID: "container:api", Kind: KindContainer, Source: SourceDocker, Service: "api",
			Healthy: true, Detail: "running healthy",
		},
	)
	v := Validator{}
	// Claim "healthy": the weak log evidence would contradict it, but the
	// strongest (container state) evidence agrees — so no conflict.
	d := Diagnosis{Claims: []Claim{
		{About: "api", Statement: "api is healthy", Asserted: "true"},
	}}
	if conflicts := v.CrossCheck(d, ev); len(conflicts) != 0 {
		t.Fatalf("strongest (container) evidence says healthy: expected no conflict, got %v", conflicts)
	}
}

func TestExtractClaims(t *testing.T) {
	text := "The postgres container is not running. The api service is healthy but misconfigured. Consider checking DNS."
	claims := ExtractClaims(text)
	if len(claims) != 2 {
		t.Fatalf("expected 2 claims, got %d: %+v", len(claims), claims)
	}
	if claims[0].About != "postgres" || claims[0].Asserted != "false" {
		t.Fatalf("claim 0 wrong: %+v", claims[0])
	}
	if claims[1].About != "api" || claims[1].Asserted != "true" {
		t.Fatalf("claim 1 wrong: %+v", claims[1])
	}
}

func TestExtractClaimsIgnoresNonStateSentences(t *testing.T) {
	text := "The build may lack dependency X. We should investigate the logs further."
	if claims := ExtractClaims(text); len(claims) != 0 {
		t.Fatalf("expected no claims from speculative text, got %+v", claims)
	}
}

func TestConflictsSummary(t *testing.T) {
	s := ConflictsSummary([]Conflict{{Explanation: "b"}, {Explanation: "a"}})
	if !strings.Contains(s, "- a") || !strings.Contains(s, "- b") {
		t.Fatalf("summary missing entries: %q", s)
	}
	if ConflictsSummary(nil) != "" {
		t.Fatal("empty summary expected for no conflicts")
	}
}
