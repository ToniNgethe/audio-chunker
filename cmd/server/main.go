package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"audi/internal/model"
	"audi/internal/processor"
	"audi/internal/storage"
)

// server coordinates job metadata, templates, and processing workers.
type server struct {
	jobsDir      string
	templates    *template.Template
	processor    *processor.Processor
	defaultChunk int
	makeBase64   bool
	mu           sync.Mutex
	jobsInFlight map[string]*model.Job
}

// templateData exposes job-related state to HTML templates.
type templateData struct {
	Jobs           []*model.Job
	Job            *model.Job
	WhisperActive  bool
	Base64Enabled  bool
	DefaultChunk   int
	Error          string
	ChunkValue     int
	ChunkUnit      string
	ChunkUnits     []chunkUnitOption
	HumanChunk     string
	TotalDuration  float64
	ChunkWarning   string
	Flash          string
	DeleteDisabled bool
	DeleteReason   string
	HasDuration    bool
}

type chunkUnitOption struct {
	Label      string
	Value      string
	Multiplier int
}

var chunkUnits = []chunkUnitOption{
	{Label: "Seconds", Value: "seconds", Multiplier: 1},
	{Label: "Minutes", Value: "minutes", Multiplier: 60},
	{Label: "Hours", Value: "hours", Multiplier: 3600},
}

// main wires configuration, templates, and HTTP handlers before serving traffic.
func main() {
	rand.Seed(time.Now().UnixNano())

	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data", "data", "root directory for generated files")
	defaultChunk := flag.Int("chunk", 300, "default chunk length in seconds")
	disableBase64 := flag.Bool("no-base64", false, "disable generation of base64 dumps")
	flag.Parse()

	jobsDir := filepath.Join(*dataDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		log.Fatalf("unable to create jobs directory: %v", err)
	}

	funcMap := template.FuncMap{
		"formatSeconds": formatSeconds,
		"uppercase":     strings.ToUpper,
		"add": func(a, b float64) float64 {
			return a + b
		},
		"formatDurationHuman": formatDurationHuman,
	}

	tmpl, err := template.New("app").Funcs(funcMap).ParseGlob(filepath.Join("web", "templates", "*.gohtml"))
	if err != nil {
		log.Fatalf("parsing templates: %v", err)
	}

	whisperArgs := strings.Fields(os.Getenv("WHISPER_ARGS"))

	srv := &server{
		jobsDir:      jobsDir,
		templates:    tmpl,
		defaultChunk: *defaultChunk,
		makeBase64:   !*disableBase64,
		processor: &processor.Processor{
			FFmpegBin:   os.Getenv("FFMPEG_BIN"),
			WhisperBin:  os.Getenv("WHISPER_BIN"),
			WhisperArgs: whisperArgs,
		},
		jobsInFlight: make(map[string]*model.Job),
	}

	// Register HTTP endpoints for the dashboard, uploads, and per-job assets.
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/upload", srv.handleUpload)
	mux.HandleFunc("/jobs/", srv.handleJobDetail)

	fileServer := http.FileServer(http.Dir(*dataDir))
	mux.Handle("/files/", http.StripPrefix("/files/", fileServer))

	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

// handleIndex renders the landing page with upload form and job list.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	jobs, err := storage.ListJobs(s.jobsDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list jobs: %v", err), http.StatusInternalServerError)
		return
	}

	value, unit := secondsToValueUnit(s.defaultChunk)
	flash := r.URL.Query().Get("flash")
	errorMsg := r.URL.Query().Get("error")
	data := templateData{
		Jobs:          jobs,
		WhisperActive: s.processor.WhisperBin != "",
		Base64Enabled: s.makeBase64,
		DefaultChunk:  s.defaultChunk,
		ChunkValue:    value,
		ChunkUnit:     unit,
		ChunkUnits:    chunkUnits,
		HumanChunk:    formatDurationHuman(s.defaultChunk),
		Flash:         flash,
		Error:         errorMsg,
	}
	if err := s.templates.ExecuteTemplate(w, "index.gohtml", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleUpload accepts the multipart video upload and enqueues processing.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseMultipartForm(512 << 20); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse form: %v", err), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		http.Error(w, "video field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	chunkDuration := resolveChunkDuration(r, s.defaultChunk)

	transcribe := r.FormValue("transcribe") == "on"

	jobID := newJobID()
	jobDir := storage.JobDir(s.jobsDir, jobID)

	if err := storage.EnsureJobSubdirs(jobDir, "original", "chunks", "base64", "transcripts"); err != nil {
		http.Error(w, fmt.Sprintf("failed to prepare job directories: %v", err), http.StatusInternalServerError)
		return
	}

	originalPath := filepath.Join(jobDir, "original", header.Filename)
	out, err := os.Create(originalPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create file: %v", err), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		http.Error(w, fmt.Sprintf("failed to save upload: %v", err), http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		http.Error(w, fmt.Sprintf("failed to finalise upload: %v", err), http.StatusInternalServerError)
		return
	}

	job := &model.Job{
		ID:                     jobID,
		OriginalFileName:       header.Filename,
		OriginalVideoPath:      filepath.ToSlash(filepath.Join("original", header.Filename)),
		CreatedAt:              time.Now(),
		ChunkDurationSeconds:   chunkDuration,
		TranscriptionRequested: transcribe,
		Status:                 model.JobStatusPending,
	}

	if err := storage.SaveJob(jobDir, job); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist job metadata: %v", err), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.jobsInFlight[jobID] = job
	s.mu.Unlock()

	go s.processJob(job, jobDir, originalPath, processor.Options{
		ChunkDurationSeconds: chunkDuration,
		MakeBase64:           s.makeBase64,
		Transcribe:           transcribe,
	})

	http.Redirect(w, r, "/jobs/"+jobID, http.StatusSeeOther)
}

// handleJobDetail serves the detailed view for a single job or its assets.
func (s *server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	jobID := parts[0]

	if len(parts) >= 2 {
		switch parts[1] {
		case "delete":
			s.handleJobDelete(w, r, jobID)
			return
		case "raw":
			s.serveJobAsset(w, r, jobID, parts[1:])
			return
		default:
			s.serveJobAsset(w, r, jobID, parts[1:])
			return
		}
	}

	job, err := storage.LoadJob(storage.JobDir(s.jobsDir, jobID))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load job: %v", err), http.StatusNotFound)
		return
	}

	data := templateData{
		Job:           job,
		WhisperActive: s.processor.WhisperBin != "",
		Base64Enabled: s.makeBase64,
		DefaultChunk:  s.defaultChunk,
		ChunkUnits:    chunkUnits,
		HumanChunk:    formatDurationHuman(job.ChunkDurationSeconds),
	}

	totalDuration := totalDurationSeconds(job.Chunks)
	data.TotalDuration = totalDuration
	data.ChunkWarning = buildChunkWarning(job, totalDuration)
	data.HasDuration = totalDuration > 0

	value, unit := secondsToValueUnit(job.ChunkDurationSeconds)
	data.ChunkValue = value
	data.ChunkUnit = unit
	s.mu.Lock()
	_, inFlight := s.jobsInFlight[jobID]
	s.mu.Unlock()
	data.DeleteDisabled = inFlight
	if inFlight {
		data.DeleteReason = "Job is currently processing. Wait for it to finish before deleting."
	}

	if err := s.templates.ExecuteTemplate(w, "job.gohtml", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveJobAsset safely exposes generated files under /jobs/{id}/raw/... .
func (s *server) serveJobAsset(w http.ResponseWriter, r *http.Request, jobID string, parts []string) {
	if len(parts) < 2 || parts[0] != "raw" {
		http.NotFound(w, r)
		return
	}

	assetPath := filepath.Join(parts[1:]...)
	clean := filepath.Clean(assetPath)
	if strings.Contains(clean, "..") {
		http.Error(w, "invalid asset path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(storage.JobDir(s.jobsDir, jobID), clean)
	http.ServeFile(w, r, fullPath)
}

// processJob runs the ffmpeg/whisper pipeline and persists the job state as it evolves.
func (s *server) processJob(job *model.Job, jobDir, originalPath string, opts processor.Options) {
	job.Status = model.JobStatusProcessing
	job.ErrorMessage = ""
	job.ProcessingLog = ""
	if err := storage.SaveJob(jobDir, job); err != nil {
		log.Printf("job %s: failed to update status: %v", job.ID, err)
	}

	ctx := context.Background()
	result, err := s.processor.Process(ctx, jobDir, originalPath, opts)
	if err != nil {
		job.Status = model.JobStatusFailed
		job.ErrorMessage = err.Error()
		job.Chunks = result.Chunks
		job.ProcessingLog = strings.Join(append(result.Logs, err.Error()), "\n---\n")
		completed := time.Now()
		job.CompletedAt = &completed
	} else {
		job.Status = model.JobStatusCompleted
		job.ErrorMessage = ""
		job.Chunks = result.Chunks
		job.ProcessingLog = strings.Join(result.Logs, "\n---\n")
		completed := time.Now()
		job.CompletedAt = &completed
	}

	if err := storage.SaveJob(jobDir, job); err != nil {
		log.Printf("job %s: failed to persist completion: %v", job.ID, err)
	}

	s.mu.Lock()
	delete(s.jobsInFlight, job.ID)
	s.mu.Unlock()
}

func (s *server) handleJobDelete(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	_, inFlight := s.jobsInFlight[jobID]
	s.mu.Unlock()
	if inFlight {
		http.Redirect(w, r, "/?error="+url.QueryEscape("Unable to delete while processing"), http.StatusSeeOther)
		return
	}

	jobDir := storage.JobDir(s.jobsDir, jobID)
	if err := os.RemoveAll(jobDir); err != nil {
		log.Printf("job %s: delete failed: %v", jobID, err)
		http.Redirect(w, r, "/?error="+url.QueryEscape("Failed to delete job"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/?flash="+url.QueryEscape("Job deleted"), http.StatusSeeOther)
}

func resolveChunkDuration(r *http.Request, fallback int) int {
	valueStr := strings.TrimSpace(r.FormValue("chunk_value"))
	unit := strings.TrimSpace(r.FormValue("chunk_unit"))
	if valueStr != "" {
		if val, err := strconv.Atoi(valueStr); err == nil && val > 0 {
			if mult := multiplierForUnit(unit); mult > 0 {
				seconds := val * mult
				if seconds > 0 {
					return seconds
				}
			}
		}
	}

	if legacy := strings.TrimSpace(r.FormValue("chunk_duration")); legacy != "" {
		if sec, err := strconv.Atoi(legacy); err == nil && sec > 0 {
			return sec
		}
	}

	return fallback
}

// newJobID generates a timestamped identifier that keeps jobs roughly ordered.
func newJobID() string {
	timestamp := time.Now().Format("20060102-150405")
	return fmt.Sprintf("%s-%04d", timestamp, rand.Intn(10000))
}

// formatSeconds converts raw seconds into mm:ss or hh:mm:ss for display.
func formatSeconds(v float64) string {
	if v < 0 {
		v = 0
	}
	total := int(math.Round(v))
	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func formatDurationHuman(seconds int) string {
	if seconds <= 0 {
		return "instant"
	}
	if seconds%3600 == 0 && seconds >= 3600 {
		hours := seconds / 3600
		unit := "hour"
		if hours != 1 {
			unit = "hours"
		}
		return fmt.Sprintf("%d %s", hours, unit)
	}
	if seconds%60 == 0 && seconds >= 60 {
		minutes := seconds / 60
		unit := "minute"
		if minutes != 1 {
			unit = "minutes"
		}
		return fmt.Sprintf("%d %s", minutes, unit)
	}
	unit := "second"
	if seconds != 1 {
		unit = "seconds"
	}
	return fmt.Sprintf("%d %s", seconds, unit)
}

func secondsToValueUnit(seconds int) (int, string) {
	if seconds%3600 == 0 && seconds >= 3600 {
		return seconds / 3600, "hours"
	}
	if seconds%60 == 0 && seconds >= 60 {
		return seconds / 60, "minutes"
	}
	if seconds <= 0 {
		seconds = 1
	}
	return seconds, "seconds"
}

func multiplierForUnit(unit string) int {
	for _, opt := range chunkUnits {
		if opt.Value == unit {
			return opt.Multiplier
		}
	}
	return 0
}

func totalDurationSeconds(chunks []model.Chunk) float64 {
	var total float64
	for _, chunk := range chunks {
		end := chunk.StartSeconds + chunk.DurationSeconds
		if end > total {
			total = end
		}
	}
	return total
}

func buildChunkWarning(job *model.Job, totalSeconds float64) string {
	chunkSeconds := job.ChunkDurationSeconds
	totalRounded := int(math.Round(totalSeconds))
	chunkCount := len(job.Chunks)

	if chunkCount == 0 || chunkSeconds <= 0 {
		return ""
	}

	humanChunk := formatDurationHuman(chunkSeconds)

	if totalRounded > 0 && chunkSeconds >= totalRounded {
		return fmt.Sprintf("Chunk duration was set to %s, which is longer than this %s recording, so only one chunk was generated. Choose a smaller duration on the upload form to create more splits.", humanChunk, formatSeconds(totalSeconds))
	}

	if chunkCount <= 2 && chunkSeconds >= 1800 {
		return fmt.Sprintf("Chunk duration was set to %s, so only %d chunk%s were needed. Try something like 5 minutes to produce shorter segments.", humanChunk, chunkCount, pluralSuffix(chunkCount))
	}

	return ""
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
