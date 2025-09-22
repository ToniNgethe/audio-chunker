package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"audi/internal/model"
)

const jobFileName = "job.json"

// SaveJob serialises job metadata atomically into job.json.
func SaveJob(jobDir string, job *model.Job) error {
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fmt.Errorf("creating job directory: %w", err)
	}

	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling job data: %w", err)
	}

	tmp := filepath.Join(jobDir, jobFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing job temp file: %w", err)
	}

	if err := os.Rename(tmp, filepath.Join(jobDir, jobFileName)); err != nil {
		return fmt.Errorf("persisting job file: %w", err)
	}

	return nil
}

// LoadJob reads job.json from disk and restores a Job structure.
func LoadJob(jobDir string) (*model.Job, error) {
	data, err := os.ReadFile(filepath.Join(jobDir, jobFileName))
	if err != nil {
		return nil, fmt.Errorf("reading job file: %w", err)
	}

	var job model.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshalling job file: %w", err)
	}

	return &job, nil
}

// ListJobs walks all job directories and returns their metadata, newest first.
func ListJobs(jobsRoot string) ([]*model.Job, error) {
	entries, err := os.ReadDir(jobsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading jobs directory: %w", err)
	}

	jobs := make([]*model.Job, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobDir := filepath.Join(jobsRoot, entry.Name())
		job, err := LoadJob(jobDir)
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})

	return jobs, nil
}

// JobDir resolves the absolute path for a job inside the data root.
func JobDir(jobsRoot, jobID string) string {
	return filepath.Join(jobsRoot, jobID)
}

// EnsureJobSubdirs makes sure the expected per-job subdirectories exist.
func EnsureJobSubdirs(jobDir string, names ...string) error {
	for _, name := range names {
		dirPath := filepath.Join(jobDir, name)
		if err := os.MkdirAll(dirPath, 0o755); err != nil {
			return fmt.Errorf("creating subdir %s: %w", name, err)
		}
	}
	return nil
}
