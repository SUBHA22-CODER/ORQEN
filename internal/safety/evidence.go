// Package safety is ORQEN's Repair Safety Engine. It gates every healing
// decision on deterministic, timestamped evidence so that transient faults do
// not trigger modification, LLM diagnoses are cross-checked against observed
// reality, risky or destructive repairs require approval, repeated failures
// trip a circuit breaker, and every decision is auditable.
//
// The package imports only domain and the standard library: all container and
// filesystem I/O arrives through seams, so every rule is unit-testable without
// Docker. Import direction is healer -> safety; safety receives analysis
// results (fingerprint, category) as plain data instead of importing healer.
package safety

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
)

// Kind enumerates the deterministic evidence types the engine reasons over.
type Kind string

const (
	KindContainer  Kind = "container_state"  // compose ps row
	KindHTTP       Kind = "http_probe"       // readiness probe result
	KindLog        Kind = "log_tail"         // recent container output
	KindDependency Kind = "dependency_state" // backing service observed state
	KindResource   Kind = "resource"         // CPU/memory pressure reading
)

// Source marks where an observation came from. Deterministic sources outrank
// model-derived text when weighing confidence.
type Source string

const (
	SourceDocker    Source = "docker"    // compose ps / stats
	SourceProbe     Source = "probe"     // httpcheck
	SourceLog       Source = "log"       // parsed output text
	SourceModel     Source = "model"     // LLM assertion (never proof)
	SourceHeuristic Source = "heuristic" // derived rule, e.g. restart loop
)

// Evidence is one timestamped observation with a stable signature when the
// observation indicates a problem ("" when healthy).
type Evidence struct {
	ID        string    `json:"id"` // stable id, e.g. "container:api"
	Kind      Kind      `json:"kind"`
	Source    Source    `json:"source"`
	Service   string    `json:"service,omitempty"`
	At        time.Time `json:"at"`
	Healthy   bool      `json:"healthy"`
	Detail    string    `json:"detail,omitempty"`
	Signature string    `json:"signature,omitempty"` // problem fingerprint; "" when healthy
}

// Weight is the base evidentiary quality of a kind, used by the confidence
// engine. Deterministic machine state outranks parsed log text.
func (e Evidence) Weight() float64 {
	switch e.Kind {
	case KindContainer:
		return 0.9
	case KindHTTP:
		return 0.85
	case KindDependency:
		return 0.8
	case KindLog:
		return 0.7
	case KindResource:
		return 0.5
	default:
		return 0.3
	}
}

// String renders a one-line human-readable form for audit and explanations.
func (e Evidence) String() string {
	status := "ok"
	if !e.Healthy {
		status = "PROBLEM"
	}
	detail := e.Detail
	if len(detail) > 120 {
		detail = detail[:117] + "…"
	}
	if e.Signature != "" {
		return fmt.Sprintf("%s[%s] %s: %s (%s)", e.Kind, e.Source, label(e), status, e.Signature)
	}
	return fmt.Sprintf("%s[%s] %s: %s", e.Kind, e.Source, label(e), status)
}

func label(e Evidence) string {
	if e.Service != "" {
		return e.Service
	}
	return string(e.ID)
}

// Set is an ordered collection of evidence with helper queries.
type Set struct {
	Items []Evidence `json:"items"`
	At    time.Time  `json:"at"` // collection timestamp
}

// Add appends evidence preserving order.
func (s *Set) Add(e Evidence) { s.Items = append(s.Items, e) }

// Problems returns only observations with a non-empty signature.
func (s *Set) Problems() []Evidence {
	var out []Evidence
	for _, e := range s.Items {
		if e.Signature != "" {
			out = append(out, e)
		}
	}
	return out
}

// Signatures returns the distinct problem signatures in first-seen order.
func (s *Set) Signatures() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range s.Problems() {
		if !seen[e.Signature] {
			seen[e.Signature] = true
			out = append(out, e.Signature)
		}
	}
	return out
}

// Kinds returns the distinct evidence kinds present, sorted for determinism.
func (s *Set) Kinds() []string {
	set := map[string]bool{}
	for _, e := range s.Items {
		set[string(e.Kind)] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ByService returns evidence for one service.
func (s *Set) ByService(name string) []Evidence {
	var out []Evidence
	for _, e := range s.Items {
		if e.Service == name {
			out = append(out, e)
		}
	}
	return out
}

// Prober is the HTTP readiness seam (satisfied by httpcheck.Check wrappers).
type Prober interface {
	Check(ctx context.Context, url string) domain.Result
}

// Collector gathers deterministic evidence through the ComposeController seam.
// A nil seam means "skip that source": evidence quality then drops, which the
// confidence engine treats as insufficiency rather than guessing.
type Collector struct {
	Compose  domain.ComposeController
	Prober   Prober
	Services []*domain.Service
	PortMap  map[string]int
	// Now overrides time.Now in tests.
	Now func() time.Time
	// LogTail caps log lines fetched per service (default 200).
	LogTail int
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// Collect observes the whole stack once: container states, per-service HTTP
// readiness, infra dependency state, resource pressure, and log tails for
// services that look unhealthy (plus workers, whose logs are their only
// signal). Every observation is timestamped and carries a signature when it
// indicates a problem.
func (c *Collector) Collect(ctx context.Context) (Set, error) {
	set := Set{At: c.now()}
	if c.Compose == nil {
		return set, domain.E(domain.KindValidation, "safety.Collector.Collect", "nil compose controller", nil)
	}
	ps, err := c.Compose.Ps(ctx)
	if err != nil {
		return set, err
	}
	// A runtime heal on a stopped stack must not read as "healthy": no
	// containers at all is itself a problem signature.
	if len(ps) == 0 {
		set.Add(Evidence{
			ID: "container:stack", Kind: KindContainer, Source: SourceDocker,
			At: set.At, Detail: "no containers found", Signature: "stack_absent",
		})
		return set, nil
	}
	appNames := map[string]bool{}
	for _, svc := range c.Services {
		if svc != nil {
			appNames[svc.Name] = true
		}
	}

	var unhealthy []string
	for _, ctr := range ps {
		e := Evidence{
			ID:      "container:" + ctr.Service,
			Kind:    KindContainer,
			Source:  SourceDocker,
			Service: ctr.Service,
			At:      set.At,
		}
		health := strings.ToLower(ctr.Health)
		switch {
		case ctr.State == "running" && (health == "" || health == "none" || health == "healthy"):
			e.Healthy = true
			e.Detail = fmt.Sprintf("state=%s health=%s", ctr.State, orNone(health))
		case ctr.State == "running":
			// Docker-native healthcheck reports unhealthy: strong persistent signal.
			e.Detail = fmt.Sprintf("state=%s health=%s", ctr.State, health)
			e.Signature = "container_unhealthy:" + ctr.Service
			unhealthy = append(unhealthy, ctr.Service)
		case ctr.State == "restarting":
			e.Detail = fmt.Sprintf("state=restarting exit=%d", ctr.ExitCode)
			e.Signature = fmt.Sprintf("restart_loop:%s:exit=%d", ctr.Service, ctr.ExitCode)
			unhealthy = append(unhealthy, ctr.Service)
		case ctr.State == "exited" || ctr.State == "dead":
			e.Detail = fmt.Sprintf("state=%s exit=%d", ctr.State, ctr.ExitCode)
			e.Signature = fmt.Sprintf("container_exited:%s:exit=%d", ctr.Service, ctr.ExitCode)
			unhealthy = append(unhealthy, ctr.Service)
		default:
			e.Detail = "state=" + ctr.State
			e.Signature = "container_" + ctr.State + ":" + ctr.Service
			unhealthy = append(unhealthy, ctr.Service)
		}
		set.Add(e)

		// Dependency observation: any container that is not an app service acts
		// as backing infrastructure for the stack; its state matters to apps.
		if !appNames[ctr.Service] {
			d := e
			d.ID = "dependency:" + ctr.Service
			d.Kind = KindDependency
			set.Add(d)
		}
	}

	// HTTP readiness for app services with a host port.
	if c.Prober != nil {
		for _, svc := range c.Services {
			if svc == nil || svc.IsWorker {
				continue
			}
			port := c.PortMap[svc.Name]
			if port == 0 {
				port = svc.HostPort
			}
			if port == 0 {
				continue
			}
			r := c.Prober.Check(ctx, fmt.Sprintf("http://localhost:%d/", port))
			e := Evidence{
				ID:      "http:" + svc.Name,
				Kind:    KindHTTP,
				Source:  SourceProbe,
				Service: svc.Name,
				At:      set.At,
				Detail:  fmt.Sprintf("%s (code=%d)", r.Status, r.Code),
			}
			if r.Healthy() {
				e.Healthy = true
			} else {
				e.Signature = "http_" + r.Status + ":" + svc.Name
				e.Detail = fmt.Sprintf("%s code=%d err=%s", r.Status, r.Code, r.Err)
				unhealthy = append(unhealthy, svc.Name)
			}
			set.Add(e)
		}
	}

	// Resource pressure: saturation is a distinct problem class.
	if stats, serr := c.Compose.Stats(ctx); serr == nil {
		for _, st := range stats {
			if st.CPUPerc >= 98 || st.MemPerc >= 98 {
				set.Add(Evidence{
					ID:        "resource:" + st.Name,
					Kind:      KindResource,
					Source:    SourceDocker,
					Service:   st.Name,
					At:        set.At,
					Detail:    fmt.Sprintf("cpu=%.1f%% mem=%.1f%% (%s)", st.CPUPerc, st.MemPerc, st.MemUsage),
					Signature: fmt.Sprintf("resource_saturation:%s:cpu=%.0f,mem=%.0f", st.Name, st.CPUPerc, st.MemPerc),
				})
			}
		}
	}

	// Log tails only for services that already look unhealthy (cheap and
	// focused) plus workers.
	tail := c.LogTail
	if tail <= 0 {
		tail = 200
	}
	need := map[string]bool{}
	for _, s := range unhealthy {
		need[s] = true
	}
	for _, svc := range c.Services {
		if svc != nil && svc.IsWorker {
			need[svc.Name] = true
		}
	}
	names := make([]string, 0, len(need))
	for n := range need {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		logs, lerr := c.Compose.Logs(ctx, name, tail)
		if lerr != nil || strings.TrimSpace(logs) == "" {
			continue
		}
		sig := LogSignature(logs)
		e := Evidence{
			ID:      "log:" + name,
			Kind:    KindLog,
			Source:  SourceLog,
			Service: name,
			At:      set.At,
			Healthy: sig == "",
			Detail:  Tail(logs, 8),
		}
		if sig != "" {
			e.Signature = sig + ":" + name
		}
		set.Add(e)
	}
	return set, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// logMarkers lists deterministic log problem markers ordered from least to
// most specific: LogSignature scans the list from the end, so specific
// failures (panic, heap OOM) outrank generic ones (exit codes, generic OOM).
// A log becomes a signature only when it matches one of these patterns;
// arbitrary scary strings never create a signature, which keeps the engine
// robust against false positives from log noise.
var logMarkers = []struct {
	pattern string
	sig     string
}{
	{"connection refused", "log_connection_refused"},
	{"could not connect", "log_connection_refused"},
	{"econnrefused", "log_connection_refused"},
	{"getaddrinfo", "log_dns_failure"},
	{"name or service not known", "log_dns_failure"},
	{"no route to host", "log_network_unreachable"},
	{"network is unreachable", "log_network_unreachable"},
	{"process exited with", "log_exit"},
	{"exit code 1", "log_exit_1"},
	{"health check failed", "log_healthcheck_failed"},
	{"healthcheck failed", "log_healthcheck_failed"},
	{"out of memory", "log_oom"},
	{"oom-killed", "log_oom"},
	{"exit code 137", "log_oom"},
	{"javascript heap out of memory", "log_heap_oom"},
	{"segfault", "log_segfault"},
	{"fatal error:", "log_fatal"},
	{"uncaught exception", "log_fatal"},
	{"panic:", "log_panic"},
}

// LogSignature scans output text for a deterministic problem marker and
// returns its signature name ("" when the log looks clean). The scan runs
// from the end of the marker list, so the most specific failure class wins.
func LogSignature(log string) string {
	low := strings.ToLower(log)
	for i := len(logMarkers) - 1; i >= 0; i-- {
		if strings.Contains(low, logMarkers[i].pattern) {
			return logMarkers[i].sig
		}
	}
	return ""
}

// Tail returns the last n lines of s.
func Tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// OneLine collapses a multi-line explanation into a single log-friendly line.
func OneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

// Sampler re-observes the stack to separate transient faults from persistent
// ones. Transient means: a problem signature seen in the first observation is
// absent from the final observation. Persistent means: at least one problem
// signature survives every observation. Evolved means the signature changed:
// the stack moved to a different failure, which is itself persistent trouble.
type Sampler struct {
	Collector *Collector
	// Samples is the number of observations (default 3; minimum 2).
	Samples int
	// Interval is the wait between observations (default 1s; tests use ms).
	Interval time.Duration
	// Sleep allows tests to stub the wait.
	Sleep func(d time.Duration)
}

// Transience classifies whether an initially observed problem persisted.
type Transience int

const (
	// TransientUnknown means too few observations were taken, or the stack was
	// never observed as unhealthy in the first place.
	TransientUnknown Transience = iota
	// TransientFault means the signature disappeared without modification.
	TransientFault
	// PersistentFault means the signature survived every re-observation.
	PersistentFault
	// EvolvedFault means the signature changed across observations.
	EvolvedFault
)

// String renders the classification for logs and audit records.
func (t Transience) String() string {
	switch t {
	case TransientFault:
		return "transient"
	case PersistentFault:
		return "persistent"
	case EvolvedFault:
		return "evolved"
	default:
		return "unknown"
	}
}

// Sample completes the observation series and classifies the signature set.
// initial is the pre-collected first observation; it counts as sample one.
func (s *Sampler) Sample(ctx context.Context, initial Set) (Set, Transience, error) {
	last := initial
	n := s.Samples
	if n < 2 {
		n = 2
	}
	interval := s.Interval
	if interval <= 0 {
		interval = time.Second
	}
	for i := 1; i < n; i++ {
		if err := s.wait(ctx, interval); err != nil {
			return last, TransientUnknown, err
		}
		next, err := s.Collector.Collect(ctx)
		if err != nil {
			return last, TransientUnknown, err
		}
		last = next
	}
	firstSigs := map[string]bool{}
	for _, sig := range initial.Signatures() {
		firstSigs[sig] = true
	}
	lastSigs := map[string]bool{}
	for _, sig := range last.Signatures() {
		lastSigs[sig] = true
	}
	switch {
	case len(firstSigs) == 0 && len(lastSigs) == 0:
		return last, TransientUnknown, nil // never sick in the first place
	case len(lastSigs) == 0:
		return last, TransientFault, nil // healed without modification
	case sameSet(firstSigs, lastSigs):
		return last, PersistentFault, nil
	default:
		return last, EvolvedFault, nil
	}
}

func (s *Sampler) wait(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		s.Sleep(d)
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
