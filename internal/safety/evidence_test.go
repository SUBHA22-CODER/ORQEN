package safety

import (
	"context"
	"testing"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
)

// fakeStack is a scripted ComposeController for evidence tests.
type fakeStack struct {
	ps    [][]domain.Container
	psIdx int
	logs  map[string]string
	stats []domain.ContainerStat
}

func (f *fakeStack) nextPs() []domain.Container {
	if f.psIdx < len(f.ps) {
		p := f.ps[f.psIdx]
		f.psIdx++
		return p
	}
	if len(f.ps) == 0 {
		return nil
	}
	return f.ps[len(f.ps)-1]
}

func (f *fakeStack) Ps(ctx context.Context) ([]domain.Container, error) { return f.nextPs(), nil }
func (f *fakeStack) Logs(ctx context.Context, service string, tail int) (string, error) {
	return f.logs[service], nil
}
func (f *fakeStack) Stats(ctx context.Context) ([]domain.ContainerStat, error) { return f.stats, nil }
func (f *fakeStack) Build(ctx context.Context, service string) (string, error) { return "", nil }
func (f *fakeStack) Up(ctx context.Context, extra ...string) (string, error)   { return "", nil }
func (f *fakeStack) Down(ctx context.Context, volumes bool) (string, error)    { return "", nil }
func (f *fakeStack) Restart(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (f *fakeStack) Stop(ctx context.Context, service string) (string, error)  { return "", nil }
func (f *fakeStack) Start(ctx context.Context, service string) (string, error) { return "", nil }
func (f *fakeStack) Recreate(ctx context.Context, service string) (string, error) {
	return "", nil
}
func (f *fakeStack) Available() bool { return true }

type fakeProber struct {
	results []domain.Result
	idx     int
}

func (p *fakeProber) Check(ctx context.Context, url string) domain.Result {
	if p.idx < len(p.results) {
		r := p.results[p.idx]
		p.idx++
		return r
	}
	if len(p.results) == 0 {
		return domain.Result{URL: url, Status: "healthy", Code: 200}
	}
	return p.results[len(p.results)-1]
}

func TestCollectContainerStates(t *testing.T) {
	stack := &fakeStack{
		ps: [][]domain.Container{{
			{Service: "api", State: "running", Health: "healthy"},
			{Service: "db", State: "running", Health: "healthy"},
		}},
	}
	c := &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}}
	set, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Problems()) != 0 {
		t.Fatalf("expected healthy stack, got %v", set.Signatures())
	}
	// db is not an app service: it must appear as dependency evidence too.
	foundDep := false
	for _, e := range set.Items {
		if e.Kind == KindDependency && e.Service == "db" {
			foundDep = true
			if !e.Healthy {
				t.Fatalf("dependency db should be healthy: %+v", e)
			}
		}
	}
	if !foundDep {
		t.Fatal("expected dependency evidence for db")
	}
	if len(set.Kinds()) < 2 {
		t.Fatalf("expected container+dependency kinds, got %v", set.Kinds())
	}
}

func TestCollectStoppedStackIsProblemNotHealthy(t *testing.T) {
	stack := &fakeStack{ps: [][]domain.Container{nil}}
	c := &Collector{Compose: stack}
	set, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sigs := set.Signatures()
	if len(sigs) != 1 || sigs[0] != "stack_absent" {
		t.Fatalf("expected stack_absent signature, got %v", sigs)
	}
}

func TestCollectUnhealthyAndRestarting(t *testing.T) {
	stack := &fakeStack{
		ps: [][]domain.Container{{
			{Service: "api", State: "restarting", ExitCode: 1},
			{Service: "web", State: "running", Health: "unhealthy"},
		}},
		logs: map[string]string{"api": "npm ERR! connection refused\n"},
	}
	c := &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}, {Name: "web"}}}
	set, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sigs := set.Signatures()
	found := map[string]bool{}
	for _, s := range sigs {
		found[s] = true
	}
	if !found["restart_loop:api:exit=1"] || !found["container_unhealthy:web"] {
		t.Fatalf("missing expected signatures: %v", sigs)
	}
	// api was unhealthy, so its log tail should be evidence with a signature.
	foundLog := false
	for _, e := range set.Items {
		if e.Kind == KindLog && e.Service == "api" && e.Signature == "log_connection_refused:api" {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatal("expected log evidence with log_connection_refused:api")
	}
}

func TestCollectHTTPProbe(t *testing.T) {
	stack := &fakeStack{
		ps: [][]domain.Container{{{Service: "api", State: "running", Health: "healthy"}}},
	}
	prober := &fakeProber{results: []domain.Result{{Service: "api", Status: "application_error", Code: 500}}}
	c := &Collector{Compose: stack, Prober: prober, Services: []*domain.Service{{Name: "api"}}, PortMap: map[string]int{"api": 8000}}
	set, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(set.Signatures(), "http_application_error:api") {
		t.Fatalf("expected http signature, got %v", set.Signatures())
	}
}

func TestCollectResourceSaturation(t *testing.T) {
	stack := &fakeStack{
		ps:    [][]domain.Container{{{Service: "api", State: "running", Health: "healthy"}}},
		stats: []domain.ContainerStat{{Name: "api", CPUPerc: 99.5, MemPerc: 40}},
	}
	c := &Collector{Compose: stack, Services: []*domain.Service{{Name: "api"}}}
	set, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(set.Signatures(), "resource_saturation:api:cpu=100,mem=40") {
		t.Fatalf("expected resource signature, got %v", set.Signatures())
	}
}

func TestLogSignatureDeterministicMarkers(t *testing.T) {
	cases := map[string]string{
		"error: connection refused to 10.0.0.5:5432": "log_connection_refused",
		"FATAL: panic: runtime error":                "log_panic",
		"JavaScript heap out of memory":              "log_heap_oom",
		"getaddrinfo ENOTFOUND db.local":             "log_dns_failure",
		"healthcheck failed after 3 tries":           "log_healthcheck_failed",
	}
	for log, want := range cases {
		if got := LogSignature(log); got != want {
			t.Errorf("LogSignature(%q)=%q want %q", log, got, want)
		}
	}
	for _, clean := range []string{"", "server listening on :8000", "GET / 200 OK\nGET /health 200"} {
		if got := LogSignature(clean); got != "" {
			t.Errorf("LogSignature(%q)=%q want empty", clean, got)
		}
	}
}

// TestLogSpecificityOrdering: more specific failures outrank generic exits.
func TestLogSpecificityOrdering(t *testing.T) {
	got := LogSignature("exit code 1\nout of memory killed")
	if got != "log_oom" {
		t.Fatalf("expected log_oom to outrank exit, got %q", got)
	}
}

func TestSamplerTransientVsPersistent(t *testing.T) {
	// Transient: restarting in sample 1, healthy afterwards.
	tr := &Collector{Compose: &fakeStack{
		ps: [][]domain.Container{
			{{Service: "api", State: "restarting", ExitCode: 1}},
			{{Service: "api", State: "running", Health: "healthy"}},
			{{Service: "api", State: "running", Health: "healthy"}},
		},
	}, Services: []*domain.Service{{Name: "api"}}}
	s := &Sampler{Collector: tr, Samples: 3, Sleep: func(time.Duration) {}}
	first, _ := tr.Collect(context.Background())
	_, class, err := s.Sample(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if class != TransientFault {
		t.Fatalf("expected transient, got %s", class)
	}

	// Persistent: restarting across all samples.
	pm := &Collector{Compose: &fakeStack{
		ps: [][]domain.Container{
			{{Service: "api", State: "restarting", ExitCode: 1}},
			{{Service: "api", State: "restarting", ExitCode: 1}},
			{{Service: "api", State: "restarting", ExitCode: 1}},
		},
	}, Services: []*domain.Service{{Name: "api"}}}
	s2 := &Sampler{Collector: pm, Samples: 3, Sleep: func(time.Duration) {}}
	first2, _ := pm.Collect(context.Background())
	_, class2, err := s2.Sample(context.Background(), first2)
	if err != nil {
		t.Fatal(err)
	}
	if class2 != PersistentFault {
		t.Fatalf("expected persistent, got %s", class2)
	}
}

func TestSamplerContextCancelled(t *testing.T) {
	pm := &Collector{Compose: &fakeStack{
		ps: [][]domain.Container{{{Service: "api", State: "restarting", ExitCode: 1}}},
	}, Services: []*domain.Service{{Name: "api"}}}
	s := &Sampler{Collector: pm, Samples: 3} // real wait: ctx cancels first tick
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first, _ := pm.Collect(context.Background())
	if _, _, err := s.Sample(ctx, first); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
