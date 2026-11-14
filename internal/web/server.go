package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/SUBHA22-CODER/ORQEN/internal/agent"
	"github.com/SUBHA22-CODER/ORQEN/internal/safety"
)

//go:embed assets/*
var assetsFS embed.FS

// Server is the HTTP control plane.
type Server struct {
	backend Backend
	store   *ChatStore
	mux     *http.ServeMux
	// Version is reported by /api/health. Callers should set it from the
	// build-injected cmd.Version instead of hardcoding a release string.
	Version string
}

// New wires routes and assets.
func New(backend Backend, store *ChatStore) *Server {
	s := &Server{backend: backend, store: store, mux: http.NewServeMux(), Version: "dev"}
	s.mux.HandleFunc("/api/projects", s.handleProjects)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/api/chats", s.handleChats)
	s.mux.HandleFunc("/api/chats/", s.handleChat)
	s.mux.HandleFunc("/api/chat", s.handleChatTurn)
	s.mux.HandleFunc("/api/approvals", s.handleApprovals)
	s.mux.HandleFunc("/api/approvals/", s.handleApprovalByID)
	s.mux.HandleFunc("/api/approvals/stream", s.handleApprovalStream)
	s.mux.HandleFunc("/api/approvals/demo", s.handleApprovalDemo)
	s.mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version})
	})
	sub, err := fs.Sub(assetsFS, "assets")
	if err == nil {
		fileServer := http.FileServer(http.FS(sub))
		s.mux.Handle("/assets/", http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			fileServer.ServeHTTP(w, r)
		})))
	}
	s.mux.HandleFunc("/", s.handleIndex)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	projects, err := s.backend.Projects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cfg, err := s.backend.Config(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		chats, err := s.store.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, chats)
	case http.MethodPost:
		var body struct {
			Project   string `json:"project"`
			ProjectID string `json:"project_id"`
		}
		_ = readJSON(r, &body)
		chat, err := s.store.Create(body.Project, body.ProjectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, chat)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/chats/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		chat, err := s.store.Get(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, chat)
	case http.MethodDelete:
		if err := s.store.Delete(id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

type sseEvent struct {
	Type              string        `json:"type"`
	Chat              *ChatSummary  `json:"chat,omitempty"`
	Event             *agent.Event  `json:"event,omitempty"`
	Message           *Message      `json:"message,omitempty"`
	NeedsConfirmation bool          `json:"needs_confirmation,omitempty"`
	Confirmation      *Confirmation `json:"confirmation,omitempty"`
	Error             string        `json:"error,omitempty"`
}

func (s *Server) handleChatTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req ChatRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("message is empty"))
		return
	}

	var chat *Chat
	var err error
	if req.ChatID == "" {
		chat, err = s.store.Create(req.Project, "")
	} else {
		chat, err = s.store.Get(req.ChatID)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if req.Project == "" {
		req.Project = chat.Project
	}
	if !req.Retry {
		chat, err = s.store.Append(chat.ID, "user", req.Text, nil, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(ev sseEvent) bool {
		data, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	summary := summarize(chat)
	send(sseEvent{Type: "start", Chat: &summary})

	events := make([]agent.Event, 0, 16)
	var confirmation *Confirmation
	emit := func(ev agent.Event) {
		events = append(events, ev)
		if ev.Type == "permission_denied" {
			confirmation = &Confirmation{Tool: ev.Tool, Destructive: ev.Destructive, Args: ev.Args}
		}
		send(sseEvent{Type: "event", Event: &ev})
	}

	out, berr := s.backend.Send(r.Context(), req, emit)
	if out == nil {
		out = &Outcome{}
	}
	final := strings.TrimSpace(out.Final)
	if berr != nil && final == "" {
		final = "Error: " + berr.Error()
	}
	if final == "" {
		final = "No response."
	}
	if confirmation == nil && out.NeedsConfirmation && out.Confirmation != nil {
		confirmation = out.Confirmation
	}

	chat, err = s.store.Append(chat.ID, "assistant", final, out.Intent, events)
	if err != nil {
		send(sseEvent{Type: "error", Error: err.Error()})
		return
	}
	if out.Project != "" && (out.Project != chat.Project || out.ProjectID != chat.ProjectID) {
		if updated, uerr := s.store.UpdateProject(chat.ID, out.Project, out.ProjectID); uerr == nil {
			chat = updated
		}
	}
	chatSummary := summarize(chat)
	var last *Message
	if len(chat.Messages) > 0 {
		m := chat.Messages[len(chat.Messages)-1]
		last = &m
	}
	send(sseEvent{
		Type: "done", Chat: &chatSummary, Message: last,
		NeedsConfirmation: confirmation != nil, Confirmation: confirmation,
	})
}

func summarize(c *Chat) ChatSummary {
	return ChatSummary{
		ID: c.ID, Title: c.Title, Project: c.Project, ProjectID: c.ProjectID,
		UpdatedAt: c.UpdatedAt, MessageCount: len(c.Messages),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sameOrigin is the CSRF guard for state-changing endpoints: cross-origin
// browser requests are rejected. Requests without an Origin header (curl,
// tests, the loopback CLI) pass — the control plane is loopback-bound by
// default, and every decision is audited with a decided_by trail.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host
}

// handleApprovals lists approval requests; ?project=<id> filters, and
// expired PENDING requests are swept first so the list is never stale.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	approvals, err := s.backend.ListApprovals(r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, approvals)
}

// handleApprovalByID routes /api/approvals/{id}[/approve|/reject].
func (s *Server) handleApprovalByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/approvals/")
	id, action, _ := strings.Cut(rest, "/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		a, err := s.backend.GetApproval(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	case "approve", "reject":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
			return
		}
		var body struct {
			ProjectID  string `json:"project_id"`
			SessionID  string `json:"session_id"`
			ActionHash string `json:"action_hash"`
			Decision   string `json:"decision"` // repair | investigation
			Reason     string `json:"reason"`
		}
		if err := readJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if body.Decision == "" {
			body.Decision = string(safety.DecisionRepair)
		}
		d := safety.ApprovalDecision{
			ApprovalID: id,
			Decision:   safety.DecisionKind(body.Decision),
			Approved:   action == "approve",
			ActionHash: body.ActionHash,
			ProjectID:  body.ProjectID,
			SessionID:  body.SessionID,
			DecidedBy:  "judge-ui",
			Reason:     body.Reason,
		}
		a, err := s.backend.DecideApproval(d)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	default:
		http.NotFound(w, r)
	}
}

// handleApprovalDemo triggers a simulated judge-demo event against the
// isolated fixture project. Responses and records are clearly labeled
// SIMULATED; production data and real infrastructure are never touched.
func (s *Server) handleApprovalDemo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, fmt.Errorf("cross-origin request rejected"))
		return
	}
	var body struct {
		Event string `json:"event"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.backend.TriggerApprovalDemo(strings.TrimSpace(body.Event))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report["simulated"] = true
	writeJSON(w, http.StatusOK, report)
}

// handleApprovalStream streams approval lifecycle events over SSE so the
// judge dashboard updates live.
func (s *Server) handleApprovalStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	events, cancel := s.backend.SubscribeApprovals(32)
	defer cancel()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST, DELETE")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	return dec.Decode(dst)
}
