package model

import "time"

// JobStatus represents the lifecycle stage of a processing job.
type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusProcessing JobStatus = "processing"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
)

// Chunk captures metadata for a single audio slice derived from the upload.
type Chunk struct {
	Index             int     `json:"index"`
	StartSeconds      float64 `json:"startSeconds"`
	DurationSeconds   float64 `json:"durationSeconds"`
	AudioFile         string  `json:"audioFile"`
	Base64File        string  `json:"base64File,omitempty"`
	TranscriptFile    string  `json:"transcriptFile,omitempty"`
	TranscriptPreview string  `json:"transcriptPreview,omitempty"`
}

// Job persists everything the UI needs to render the processing results.
type Job struct {
	ID                     string     `json:"id"`
	OriginalFileName       string     `json:"originalFileName"`
	OriginalVideoPath      string     `json:"originalVideoPath"`
	CreatedAt              time.Time  `json:"createdAt"`
	CompletedAt            *time.Time `json:"completedAt,omitempty"`
	ChunkDurationSeconds   int        `json:"chunkDurationSeconds"`
	TranscriptionRequested bool       `json:"transcriptionRequested"`
	Status                 JobStatus  `json:"status"`
	ErrorMessage           string     `json:"errorMessage,omitempty"`
	Chunks                 []Chunk    `json:"chunks"`
	ProcessingLog          string     `json:"processingLog,omitempty"`
}

// IsDone reports whether the job reached a terminal state.
func (j *Job) IsDone() bool {
	return j.Status == JobStatusCompleted || j.Status == JobStatusFailed
}
