package main

import (
	"context"
	"deep-research/pkg/agent"
	"deep-research/pkg/llm"
	"deep-research/pkg/search"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

// ResearchJob represents an active research job
type ResearchJob struct {
	ID        string               `json:"id"`
	Topic     string               `json:"topic"`
	Status    string               `json:"status"` // "idle", "planning", "awaiting_approval", "running", "complete", "error", "cancelled"
	Progress  agent.ProgressEvent  `json:"progress"`
	Plan      *agent.ResearchPlan  `json:"plan,omitempty"`
	Result    *agent.ResearchResult `json:"result,omitempty"`
	Error     string               `json:"error,omitempty"`
	StartedAt time.Time            `json:"startedAt"`
	Config    ResearchRequest      `json:"config"`
}

// ResearchRequest is the JSON body for starting research
type ResearchRequest struct {
	Topic       string `json:"topic"`
	Loops       int    `json:"loops"`
	Parallel    int    `json:"parallel"`
	ContextLen  int    `json:"contextLen"`
	DeepMode    bool   `json:"deepMode"`
	ResultLinks bool   `json:"resultLinks"`
	MinResults  int    `json:"minResults"`
	DelayMs     int    `json:"delayMs"`
	SimpleMode  bool   `json:"simpleMode"`
	MaxPages    int    `json:"maxPages"`
}

// ReviseRequest is the JSON body for revising a plan
type ReviseRequest struct {
	Feedback string `json:"feedback"`
}

// Server holds the HTTP server state
type Server struct {
	lmURL       string
	searxURL    string
	currentJob  *ResearchJob
	mu          sync.RWMutex
	sseClients  map[chan agent.ProgressEvent]bool
	sseMu       sync.Mutex
	cancelFunc  context.CancelFunc
	researcher  *agent.DeepResearcher
}

func main() {
	// Detect WSL and set appropriate LM Studio URL
	defaultLMURL := "http://localhost:1234/v1"
	if isWSL() {
		wslHost := getWSLHost()
		if wslHost != "" {
			defaultLMURL = fmt.Sprintf("http://%s:1234/v1", wslHost)
		}
	}

	// Parse command line flags (override defaults)
	var lmURL, searxURL, port string
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--lm-url":
			if i+1 < len(os.Args) {
				lmURL = os.Args[i+1]
				i++
			}
		case "--searxng-url":
			if i+1 < len(os.Args) {
				searxURL = os.Args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(os.Args) {
				port = os.Args[i+1]
				i++
			}
		}
	}

	// Fall back to env vars, then defaults
	if lmURL == "" {
		lmURL = getEnv("LM_URL", defaultLMURL)
	}
	if searxURL == "" {
		searxURL = getEnv("SEARX_URL", "http://localhost:8080")
	}
	if port == "" {
		port = getEnv("PORT", "8081")
	}

	server := &Server{
		lmURL:      lmURL,
		searxURL:   searxURL,
		currentJob: &ResearchJob{Status: "idle"},
		sseClients: make(map[chan agent.ProgressEvent]bool),
	}

	// API routes
	http.HandleFunc("/api/research", server.handleResearch)
	http.HandleFunc("/api/approve", server.handleApprove)
	http.HandleFunc("/api/revise", server.handleRevise)
	http.HandleFunc("/api/cancel", server.handleCancel)
	http.HandleFunc("/api/status", server.handleStatus)
	http.HandleFunc("/api/progress", server.handleProgress)
	http.HandleFunc("/api/results", server.handleResults)

	// Serve embedded web files
	webContent, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(webContent)))

	fmt.Printf("🚀 Deep Research Web UI\n")
	fmt.Printf("   LM Studio: %s\n", lmURL)
	fmt.Printf("   SearXNG:   %s\n", searxURL)
	fmt.Printf("   Web UI:    http://localhost:%s\n", port)
	fmt.Println("\nOpen your browser to start researching!")

	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// handleResearch creates a plan and returns it for approval
func (s *Server) handleResearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if already running
	s.mu.RLock()
	status := s.currentJob.Status
	s.mu.RUnlock()
	if status == "planning" || status == "running" || status == "awaiting_approval" {
		http.Error(w, "Research already in progress", http.StatusConflict)
		return
	}

	// Parse request
	var req ResearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Topic == "" {
		http.Error(w, "Topic is required", http.StatusBadRequest)
		return
	}

	// Set defaults
	if req.Loops <= 0 {
		req.Loops = 5
	}
	if req.Parallel <= 0 {
		req.Parallel = 5
	}
	if req.ContextLen <= 0 {
		req.ContextLen = 32768
	}
	if req.MinResults <= 0 {
		req.MinResults = 20
	}
	if req.DelayMs <= 0 {
		req.DelayMs = 500
	}

	// Create job
	job := &ResearchJob{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Topic:     req.Topic,
		Status:    "planning",
		StartedAt: time.Now(),
		Config:    req,
	}

	s.mu.Lock()
	s.currentJob = job
	s.mu.Unlock()

	// Create plan synchronously and return for approval
	s.createPlan(req)

	// Return current job with plan
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.currentJob)
}

// createPlan generates the research plan
func (s *Server) createPlan(req ResearchRequest) {
	// Setup LLM client
	llmClient := llm.NewClient(llm.Config{
		BaseURL:       s.lmURL,
		APIKey:        "lm-studio",
		Model:         "local-model",
		Temperature:   0.0,
		ContextLength: req.ContextLen,
		Timeout:       5 * time.Minute,
	})

	// Setup search client
	searcher := search.NewSearXNGClient(s.searxURL)

	// Setup agent with progress callback
	researcher := agent.NewDeepResearcher(llmClient, searcher, agent.Config{
		MaxLoops:      req.Loops,
		ParallelQuery: req.Parallel,
		DeepMode:      req.DeepMode,
		ResultLinks:   req.ResultLinks,
		SimpleMode:    req.SimpleMode,
		MinResults:    req.MinResults,
		DelayMs:       req.DelayMs,
		MaxPages:      req.MaxPages,
		ContextLength: req.ContextLen,
		OnProgress:    s.onProgress,
	})

	// Store researcher for later use
	s.mu.Lock()
	s.researcher = researcher
	s.mu.Unlock()

	// Emit planning event
	s.onProgress(agent.ProgressEvent{
		Phase:   "planning",
		Message: "Creating research plan...",
		Percent: 2,
	})

	// Create plan
	var plan agent.ResearchPlan
	var err error
	if req.SimpleMode {
		plan, err = researcher.CreatePlan(req.Topic, "")
	} else {
		plan, err = researcher.CreatePlanExhaustive(req.Topic, "")
	}

	if err != nil {
		s.setError(fmt.Sprintf("Failed to create plan: %v", err))
		return
	}

	// Update job with plan and wait for approval
	s.mu.Lock()
	s.currentJob.Plan = &plan
	s.currentJob.Status = "awaiting_approval"
	s.mu.Unlock()

	s.onProgress(agent.ProgressEvent{
		Phase:   "awaiting_approval",
		Message: fmt.Sprintf("Plan ready with %d search queries. Awaiting approval.", len(plan.SearchQueries)),
		Percent: 5,
	})
}

// handleApprove starts research execution after plan approval
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	status := s.currentJob.Status
	plan := s.currentJob.Plan
	researcher := s.researcher
	req := s.currentJob.Config
	topic := s.currentJob.Topic
	s.mu.RUnlock()

	if status != "awaiting_approval" {
		http.Error(w, "No plan awaiting approval", http.StatusBadRequest)
		return
	}

	if plan == nil || researcher == nil {
		http.Error(w, "Plan not found", http.StatusInternalServerError)
		return
	}

	// Update status to running
	s.mu.Lock()
	s.currentJob.Status = "running"
	s.mu.Unlock()

	// Create cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelFunc = cancel
	s.mu.Unlock()

	// Start research in background
	go s.executeResearch(ctx, researcher, topic, *plan, req.SimpleMode)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "running",
	})
}

// handleRevise regenerates the plan with user feedback
func (s *Server) handleRevise(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	status := s.currentJob.Status
	req := s.currentJob.Config
	s.mu.RUnlock()

	if status != "awaiting_approval" {
		http.Error(w, "No plan awaiting revision", http.StatusBadRequest)
		return
	}

	// Parse revision feedback
	var reviseReq ReviseRequest
	if err := json.NewDecoder(r.Body).Decode(&reviseReq); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Update status back to planning
	s.mu.Lock()
	s.currentJob.Status = "planning"
	s.currentJob.Plan = nil
	s.mu.Unlock()

	// Regenerate plan with feedback
	s.createPlanWithFeedback(req, reviseReq.Feedback)

	// Return updated job
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.currentJob)
}

// createPlanWithFeedback generates a new plan incorporating user feedback
func (s *Server) createPlanWithFeedback(req ResearchRequest, feedback string) {
	researcher := s.researcher
	if researcher == nil {
		s.setError("Researcher not initialized")
		return
	}

	// Emit planning event
	s.onProgress(agent.ProgressEvent{
		Phase:   "planning",
		Message: "Revising research plan with your feedback...",
		Percent: 2,
	})

	// Create plan with feedback as hint
	var plan agent.ResearchPlan
	var err error
	if req.SimpleMode {
		plan, err = researcher.CreatePlan(req.Topic, feedback)
	} else {
		plan, err = researcher.CreatePlanExhaustive(req.Topic, feedback)
	}

	if err != nil {
		s.setError(fmt.Sprintf("Failed to revise plan: %v", err))
		return
	}

	// Update job with new plan
	s.mu.Lock()
	s.currentJob.Plan = &plan
	s.currentJob.Status = "awaiting_approval"
	s.mu.Unlock()

	s.onProgress(agent.ProgressEvent{
		Phase:   "awaiting_approval",
		Message: fmt.Sprintf("Revised plan ready with %d search queries. Awaiting approval.", len(plan.SearchQueries)),
		Percent: 5,
	})
}

// handleCancel cancels an ongoing research (triggers early report)
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	status := s.currentJob.Status
	cancelFunc := s.cancelFunc
	s.mu.RUnlock()

	if status == "running" && cancelFunc != nil {
		// Cancel the context - this will trigger early report writing
		cancelFunc()
		
		s.mu.Lock()
		s.currentJob.Status = "cancelled"
		s.mu.Unlock()

		s.onProgress(agent.ProgressEvent{
			Phase:   "cancelling",
			Message: "Cancelling search and generating partial report...",
			Percent: 85,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "cancelling",
		})
		return
	}

	if status == "awaiting_approval" || status == "planning" {
		// Just reset to idle
		s.mu.Lock()
		s.currentJob = &ResearchJob{Status: "idle"}
		s.researcher = nil
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "cancelled",
		})
		return
	}

	http.Error(w, "Nothing to cancel", http.StatusBadRequest)
}

// executeResearch runs the research with cancellation support
func (s *Server) executeResearch(ctx context.Context, researcher *agent.DeepResearcher, topic string, plan agent.ResearchPlan, simpleMode bool) {
	var result agent.ResearchResult
	var err error
	
	if simpleMode {
		result, err = researcher.Run(topic, plan)
	} else {
		result, err = researcher.RunExhaustiveWithContext(ctx, topic, plan)
	}

	if err != nil {
		// Check if it was a cancellation
		if ctx.Err() == context.Canceled {
			// Cancellation already handled, result should contain partial report
			s.mu.Lock()
			s.currentJob.Status = "complete"
			s.currentJob.Result = &result
			s.mu.Unlock()

			s.onProgress(agent.ProgressEvent{
				Phase:     "complete",
				Message:   fmt.Sprintf("Partial report generated with %d sources (search was cancelled).", len(result.Sources)),
				Percent:   100,
				URLsFound: len(result.Sources),
			})
			return
		}
		s.setError(fmt.Sprintf("Research failed: %v", err))
		return
	}

	// Complete
	s.mu.Lock()
	s.currentJob.Status = "complete"
	s.currentJob.Result = &result
	s.mu.Unlock()

	s.onProgress(agent.ProgressEvent{
		Phase:     "complete",
		Message:   fmt.Sprintf("Research complete! Found %d sources.", len(result.Sources)),
		Percent:   100,
		URLsFound: len(result.Sources),
	})
}

// onProgress handles progress events from the agent
func (s *Server) onProgress(event agent.ProgressEvent) {
	s.mu.Lock()
	s.currentJob.Progress = event
	s.mu.Unlock()

	// Broadcast to SSE clients
	s.sseMu.Lock()
	for ch := range s.sseClients {
		select {
		case ch <- event:
		default:
			// Client not keeping up, skip
		}
	}
	s.sseMu.Unlock()
}

// setError sets the job to error state
func (s *Server) setError(errMsg string) {
	s.mu.Lock()
	s.currentJob.Status = "error"
	s.currentJob.Error = errMsg
	s.mu.Unlock()

	s.onProgress(agent.ProgressEvent{
		Phase:   "error",
		Message: errMsg,
		Percent: 0,
	})
}

// handleStatus returns current job status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.currentJob)
}

// handleProgress provides SSE stream for real-time progress
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Create channel for this client
	ch := make(chan agent.ProgressEvent, 10)
	s.sseMu.Lock()
	s.sseClients[ch] = true
	s.sseMu.Unlock()

	// Remove on disconnect
	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, ch)
		s.sseMu.Unlock()
		close(ch)
	}()

	// Send current state immediately
	s.mu.RLock()
	currentProgress := s.currentJob.Progress
	s.mu.RUnlock()
	
	data, _ := json.Marshal(currentProgress)
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()

	// Stream updates
	for {
		select {
		case event := <-ch:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
			
			if event.Phase == "complete" || event.Phase == "error" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// handleResults returns the research results
func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.currentJob.Result == nil {
		http.Error(w, "No results available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.currentJob.Result)
}

// Helper functions

func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "microsoft")
}

func getWSLHost() string {
	// Method 1: Try default gateway (more reliable for WSL2)
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err == nil {
		fields := strings.Fields(string(out))
		for i, field := range fields {
			if field == "via" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	
	// Method 2: Fall back to resolv.conf nameserver
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "nameserver") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return ""
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
