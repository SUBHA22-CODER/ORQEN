package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/SUBHA22-CODER/ORQEN/internal/config"
	"github.com/SUBHA22-CODER/ORQEN/internal/docker"
	"github.com/SUBHA22-CODER/ORQEN/internal/domain"
	"github.com/SUBHA22-CODER/ORQEN/internal/envvar"
	"github.com/SUBHA22-CODER/ORQEN/internal/healer"
	"github.com/SUBHA22-CODER/ORQEN/internal/httpcheck"
	"github.com/SUBHA22-CODER/ORQEN/internal/infra"
	"github.com/SUBHA22-CODER/ORQEN/internal/llm"
	"github.com/SUBHA22-CODER/ORQEN/internal/safefs"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
	"github.com/SUBHA22-CODER/ORQEN/internal/state"
	"github.com/SUBHA22-CODER/ORQEN/internal/ui"
)

func logf(format string, args ...any) {
	if flagQuiet {
		return
	}
	fmt.Printf(format+"\n", args...)
}

func verbosef(format string, args ...any) {
	if !flagVerbose || flagQuiet {
		return
	}
	fmt.Printf(ui.MutedStyle.Render(format)+"\n", args...)
}

func resolveProject(name string) (*state.Manager, *domain.Lock, error) {
	return state.Resolve(name)
}

func requireDocker(ctx context.Context) error {
	if !docker.Available() {
		return domain.E(domain.KindDocker, "doctor", "docker is not installed or not on PATH", nil)
	}
	if !docker.DaemonRunning(ctx) {
		return domain.E(domain.KindDocker, "doctor", "the Docker daemon is not running", nil)
	}
	return nil
}

func composeFor(mgr *state.Manager) *docker.Compose {
	return docker.New(mgr.ComposePath(), mgr.Dir, "orqen-"+mgr.ID)
}

// projectFor reconstructs the domain.Project from persisted state.
func projectFor(mgr *state.Manager, lock *domain.Lock) *domain.Project {
	return &domain.Project{
		ID:            lock.ProjectID,
		Name:          lock.Project,
		SourcePath:    lock.RepoPath,
		WorkDir:       lock.WorkDir,
		Services:      lock.Services,
		InfraServices: lock.Infra,
		Env:           lock.Env,
		Intel:         lock.Intel,
		NetworkName:   lock.NetworkName,
		CreatedAt:     lock.LastInit,
		UpdatedAt:     lock.LastUp,
	}
}

// llmFor resolves an LLM client, returning ErrNotConfigured when unavailable.
func llmFor(jsonMode bool) (domain.LLMClient, error) {
	resolved, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	return llm.NewFromResolved(resolved, jsonMode)
}

func healerFor(mgr *state.Manager, lock *domain.Lock, client domain.LLMClient) (*healer.Healer, error) {
	workJail, err := safefs.NewJail(mgr.Dir)
	if err != nil {
		return nil, err
	}
	var srcJail *safefs.Jail
	if lock.RepoPath != "" {
		if j, err := safefs.NewJail(lock.RepoPath); err == nil {
			srcJail = j
		}
	}
	compose := composeFor(mgr)
	gate := &safety.Gate{
		Collector: &safety.Collector{
			Compose:  compose,
			Prober:   probeFor(),
			Services: lock.Services,
			PortMap:  lock.PortMap,
		},
		Breaker: safety.NewBreaker(mgr.BreakerPath(), 0, 0),
		Audit:   safety.NewAudit(mgr.SafetyAuditPath()),
		Sandbox: &safety.Sandbox{Runner: docker.NewSandboxRunner()},
		Logf: func(format string, args ...any) {
			verbosef("  safety: "+format, args...)
		},
	}
	// Judge mode (active while `orqen serve` runs): risky repairs pause and
	// await a structured approval from the web control plane instead of being
	// refused outright. CLI sessions keep --yes / prompt behavior.
	if judgeApprovals {
		coord := judgeCoordinator(mgr.ID)
		coord.Emit = func(eventType string, a safety.Approval) {
			verbosef("  approval %s: %s [%s] risk=%s confidence=%.2f", eventType, a.ApprovalID, a.ActionType, a.RiskLevel, a.ConfidenceScore)
		}
		gate.Coordinator = coord
	}
	return &healer.Healer{
		Compose:       compose,
		LLM:           client,
		WorkJail:      workJail,
		SourceJail:    srcJail,
		Services:      lock.Services,
		PortMap:       lock.PortMap,
		MaxTries:      3,
		Provenance:    healer.NewProvenance(mgr.RepairsPath()),
		HealthTimeout: 90 * time.Second,
		PollInterval:  2 * time.Second,
		Logf: func(format string, args ...any) {
			if !flagQuiet {
				fmt.Println(ui.MutedStyle.Render(fmt.Sprintf(format, args...)))
			}
		},
		HTTPCheck: func(ctx context.Context, url string) domain.Result {
			return httpcheck.Check(ctx, url, 4*time.Second)
		},
		Safety: gate,
	}, nil
}

// probeFor returns the HTTP prober seam used by the safety collector.
func probeFor() safety.Prober {
	return proberFunc(func(ctx context.Context, url string) domain.Result {
		return httpcheck.Check(ctx, url, 4*time.Second)
	})
}

type proberFunc func(ctx context.Context, url string) domain.Result

func (f proberFunc) Check(ctx context.Context, url string) domain.Result { return f(ctx, url) }

// setApprovalMode configures how the gate asks for approval of risky repairs.
// "auto" approves everything (--yes); "prompt" asks in plain language when a
// terminal is attached; any other value (the default for non-interactive
// callers such as chat and the web console) leaves the approver nil, which
// refuses risky repairs — the safe default.
func setApprovalMode(g *safety.Gate, mode string) {
	switch mode {
	case "auto":
		g.Approver = func(context.Context, safety.ApprovalRequest) bool { return true }
	case "prompt":
		if term.IsTerminal(int(os.Stdin.Fd())) {
			g.Approver = promptApprover
		}
	}
}

// promptApprover asks the user in plain language; no debugging knowledge is
// required to answer.
func promptApprover(_ context.Context, req safety.ApprovalRequest) bool {
	fmt.Println()
	fmt.Println(ui.WarnStyle.Render("ORQEN wants to apply a repair that is harder to undo:"))
	if req.Summary != "" {
		fmt.Println("  Fix: " + req.Summary)
	}
	fmt.Println("  Risk: " + req.Explanation)
	if len(req.Files) > 0 {
		fmt.Println("  Files: " + strings.Join(req.Files, ", "))
	}
	fmt.Print("Apply this repair? [y/N]: ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// renderEnv rebuilds the project .env from the lock, infra bindings, service
// env, and dashboard overrides. Existing values win over regenerated defaults.
func renderEnv(mgr *state.Manager, lock *domain.Lock) (map[string]string, error) {
	existing, _ := mgr.LoadEnv()
	merged, err := envvar.MergeEnvironment(existing, lock.Env, infra.GenerateEnvBindings(lock.Infra))
	if err != nil {
		return nil, err
	}
	for _, svc := range lock.Services {
		for k, v := range svc.Env {
			merged[k] = v
		}
	}
	overrides, _ := mgr.LoadOverrides()
	for k, v := range overrides {
		merged[k] = v
	}
	return merged, nil
}

func writeProjectEnv(mgr *state.Manager, lock *domain.Lock) error {
	merged, err := renderEnv(mgr, lock)
	if err != nil {
		return err
	}
	return mgr.SaveEnv(merged)
}

type urlPair struct {
	Service string
	URL     string
}

func projectURLs(lock *domain.Lock) []urlPair {
	var out []urlPair
	for _, svc := range lock.Services {
		if svc.IsWorker {
			continue
		}
		host := lock.PortMap[svc.Name]
		if host == 0 {
			host = svc.HostPort
		}
		if host == 0 {
			continue
		}
		out = append(out, urlPair{Service: svc.Name, URL: fmt.Sprintf("http://localhost:%d", host)})
	}
	return out
}

// printURLs prints live endpoints, but only for containers that are actually
// running. Stopped services are summarized honestly instead of advertised as
// live, so a stopped stack never claims working URLs.
func printURLs(ctx context.Context, mgr *state.Manager, lock *domain.Lock) {
	pairs := projectURLs(lock)
	if len(pairs) == 0 {
		return
	}
	ps, err := composeFor(mgr).Ps(ctx)
	if err != nil {
		logf("%s", ui.WarningLine("could not query container state; endpoints not shown ("+err.Error()+")"))
		return
	}
	live, stopped := partitionURLs(pairs, ps)
	if len(live) > 0 {
		logf("")
		logf("%s", ui.Section("live endpoints"))
		for _, p := range live {
			logf("  %-16s %s", p.Service, ui.URL(p.URL))
		}
	}
	if len(stopped) > 0 {
		names := make([]string, 0, len(stopped))
		for _, p := range stopped {
			names = append(names, p.Service)
		}
		logf("%s", ui.MutedStyle.Render(fmt.Sprintf("  ○ not running: %s — start with `orqen up %s`", strings.Join(names, ", "), mgr.ID)))
	}
}

// partitionURLs splits endpoint candidates into live and stopped based on
// container state; services missing from ps count as stopped.
func partitionURLs(pairs []urlPair, ps []domain.Container) (live, stopped []urlPair) {
	byService := map[string]domain.Container{}
	for _, c := range ps {
		byService[c.Service] = c
	}
	for _, p := range pairs {
		if c, ok := byService[p.Service]; ok && c.State == "running" {
			live = append(live, p)
		} else {
			stopped = append(stopped, p)
		}
	}
	return live, stopped
}

// findContainer returns the container row for a service name.
func findContainer(ps []domain.Container, name string) (domain.Container, bool) {
	for _, c := range ps {
		if c.Service == name {
			return c, true
		}
	}
	return domain.Container{}, false
}

// containerLabel is the short health label used in progress lines.
func containerLabel(c domain.Container) string {
	switch {
	case c.State == "":
		return "stopped"
	case c.State != "running":
		return c.State
	case strings.EqualFold(c.Health, "healthy"):
		return "healthy"
	case strings.EqualFold(c.Health, "unhealthy"):
		return "unhealthy"
	case strings.EqualFold(c.Health, "starting"):
		return "starting"
	default:
		return "running"
	}
}

// waitForHealthy polls compose until every non-worker app service is running
// and its healthcheck (when one is defined) reports healthy, so the status
// table and endpoints printed right after a start reflect reality instead of
// containers that are still "starting". It never fails; on timeout it warns.
func waitForHealthy(ctx context.Context, mgr *state.Manager, lock *domain.Lock, timeout time.Duration) {
	watch := map[string]bool{}
	for _, svc := range lock.Services {
		if svc.IsWorker {
			continue
		}
		if lock.PortMap[svc.Name] > 0 || svc.HostPort > 0 {
			watch[svc.Name] = true
		}
	}
	if len(watch) == 0 {
		return
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	compose := composeFor(mgr)
	deadline := time.Now().Add(timeout)
	last := map[string]string{}
	started := time.Now()
	for {
		ps, err := compose.Ps(ctx)
		if err != nil {
			return
		}
		settled := true
		for name := range watch {
			label := "stopped"
			if c, ok := findContainer(ps, name); ok {
				label = containerLabel(c)
			}
			if last[name] != label {
				last[name] = label
				elapsed := fmt.Sprintf("%.0fs", time.Since(started).Seconds())
				if label == "healthy" || label == "running" {
					logf("%s", ui.SuccessLine(fmt.Sprintf("health check %s: %s (%s)", name, label, elapsed)))
				} else {
					logf("%s", ui.MutedStyle.Render(fmt.Sprintf("  ● health check %s: %s (%s)", name, label, elapsed)))
				}
			}
			if label != "healthy" && label != "running" {
				settled = false
			}
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			logf("%s", ui.WarningLine("health checks still settling after "+timeout.String()+"; showing current state"))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func confirm(prompt string) bool {
	if flagYes {
		return true
	}
	fmt.Print(ui.WarnStyle.Render("? ") + prompt + " [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
func checkDaemon(ctx context.Context) error {
	if !docker.DaemonRunning(ctx) {
		return domain.E(domain.KindDocker, "doctor", "the Docker daemon is not running", nil)
	}
	return nil
}

func missingEnvKeys(mgr *state.Manager, lock *domain.Lock) []string {
	if lock == nil {
		return nil
	}
	merged, err := renderEnv(mgr, lock)
	if err != nil {
		return nil
	}
	var missing []string
	for _, ev := range lock.Env {
		if strings.HasSuffix(ev.Key, "_") {
			continue
		}
		// Respect the Required flag: only variables required by the application
		// can be considered missing.
		if !ev.Required {
			continue
		}
		// If already set, not missing.
		if strings.TrimSpace(merged[ev.Key]) != "" {
			continue
		}
		// GitHub PAT is optional for public repositories.
		if ev.Key == "GITHUB_PAT" || ev.Key == "GH_TOKEN" {
			continue
		}
		// Skip alternative, unused LLM provider keys when another provider
		// is selected or configured.
		if isAlternativeProviderKey(ev, merged, lock.Env) {
			continue
		}
		missing = append(missing, ev.Key)
	}
	sort.Strings(missing)
	return missing
}

func isAlternativeProviderKey(ev *domain.EnvVar, merged map[string]string, allEnv []*domain.EnvVar) bool {
	providerKeys := map[string]string{
		"GEMINI_API_KEY":    "gemini",
		"XAI_API_KEY":       "grok",
		"OPENAI_API_KEY":    "openai",
		"ANTHROPIC_API_KEY": "anthropic",
		"GROQ_API_KEY":      "groq",
	}
	provider, isProvKey := providerKeys[ev.Key]
	if !isProvKey {
		return false
	}

	if strings.TrimSpace(merged[ev.Key]) != "" {
		return false
	}

	// Identify active/selected provider from repo env or ORQEN config.
	selected := ""
	for _, selKey := range []string{"ORQEN_LLM_PROVIDER", "LLM_PROVIDER", "AI_PROVIDER", "PROVIDER"} {
		if v := strings.TrimSpace(merged[selKey]); v != "" {
			selected = config.NormalizeProvider(v)
			break
		}
	}
	if selected == "" {
		if r, err := config.Resolve(); err == nil && r != nil {
			selected = config.NormalizeProvider(r.Provider)
		}
	}

	// If a different provider is selected and its key is provided, this one is an alternative.
	if selected != "" && selected != provider {
		switch selected {
		case "gemini":
			if strings.TrimSpace(merged["GEMINI_API_KEY"]) != "" {
				return true
			}
		case "grok":
			if strings.TrimSpace(merged["XAI_API_KEY"]) != "" {
				return true
			}
		case "openai":
			if strings.TrimSpace(merged["OPENAI_API_KEY"]) != "" {
				return true
			}
		}
	}

	// If any other LLM provider key is satisfied and this key came from an example env file
	// (not direct source code usage), it is an unused alternative from a template.
	if ev.Source == domain.EnvSourceExample {
		for otherKey := range providerKeys {
			if otherKey != ev.Key && strings.TrimSpace(merged[otherKey]) != "" {
				return true
			}
		}
		if selected != "" && selected != provider {
			return true
		}
	}

	return false
}

func urlStrings(lock *domain.Lock) []string {
	pairs := projectURLs(lock)
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.URL)
	}
	return out
}

// printEnvStatus surfaces unresolved configuration right after init/update so a
// developer knows exactly what to set.
func printEnvStatus(mgr *state.Manager, lock *domain.Lock) {
	missing := missingEnvKeys(mgr, lock)
	if len(missing) == 0 {
		return
	}
	shown := missing
	extra := ""
	if len(shown) > 10 {
		extra = fmt.Sprintf(" (+%d more)", len(shown)-10)
		shown = shown[:10]
	}
	logf("%s", ui.WarningLine(fmt.Sprintf("%d variable(s) still unset: %s%s", len(missing), strings.Join(shown, ", "), extra)))
	logf("%s", ui.InfoLine(fmt.Sprintf("set them with: orqen env %s set KEY=VALUE", mgr.ID)))
}
