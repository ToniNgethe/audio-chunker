package processor

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"audi/internal/model"
)

// Processor wraps the external binaries used to transform uploaded media.
type Processor struct {
	FFmpegBin   string
	WhisperBin  string
	WhisperArgs []string
}

// Options tunes how audio chunks are generated and whether extras are produced.
type Options struct {
	ChunkDurationSeconds int
	MakeBase64           bool
	Transcribe           bool
}

// Result captures the generated chunks alongside the command output.
type Result struct {
	Chunks []model.Chunk
	Logs   []string
}

// Process runs ffmpeg (and optionally Whisper) to populate the job directory.
func (p *Processor) Process(ctx context.Context, jobDir, inputPath string, opts Options) (Result, error) {
	ffmpeg := p.FFmpegBin
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return Result{}, fmt.Errorf("ffmpeg binary not found: %w", err)
	}

	chunksDir := filepath.Join(jobDir, "chunks")
	base64Dir := filepath.Join(jobDir, "base64")
	transcriptsDir := filepath.Join(jobDir, "transcripts")

	for _, dir := range []string{chunksDir, base64Dir, transcriptsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Result{}, fmt.Errorf("creating processing directory: %w", err)
		}
	}

	chunkPattern := filepath.Join(chunksDir, "chunk_%03d.wav")
	args := []string{
		"-y",
		"-i", inputPath,
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "16000",
		"-ac", "1",
		"-f", "segment",
		"-segment_time", strconv.Itoa(opts.ChunkDurationSeconds),
		"-reset_timestamps", "1",
		chunkPattern,
	}

	logEntry, err := runCommand(ctx, ffmpeg, args...)
	logs := []string{logEntry}
	if err != nil {
		return Result{Logs: logs}, fmt.Errorf("running ffmpeg: %w", err)
	}

	globPattern := filepath.Join(chunksDir, "chunk_*.wav")
	chunkFiles, err := filepath.Glob(globPattern)
	if err != nil {
		return Result{Logs: logs}, fmt.Errorf("locating chunks: %w", err)
	}

	sort.Strings(chunkFiles)
	if len(chunkFiles) == 0 {
		return Result{Logs: logs}, errors.New("no audio chunks produced")
	}

	makeBase64 := opts.MakeBase64
	transcribe := opts.Transcribe && p.WhisperBin != ""

	chunks := make([]model.Chunk, 0, len(chunkFiles))

	for idx, chunkPath := range chunkFiles {
		select {
		case <-ctx.Done():
			return Result{Logs: logs}, ctx.Err()
		default:
		}

		duration, err := wavDuration(chunkPath)
		if err != nil {
			return Result{Logs: logs}, fmt.Errorf("determining chunk duration: %w", err)
		}

		chunk := model.Chunk{
			Index:           idx,
			StartSeconds:    float64(idx * opts.ChunkDurationSeconds),
			DurationSeconds: duration,
			AudioFile:       filepath.ToSlash(filepath.Join("chunks", filepath.Base(chunkPath))),
		}

		if makeBase64 {
			baseName := strings.TrimSuffix(filepath.Base(chunkPath), filepath.Ext(chunkPath)) + ".b64.txt"
			base64Path := filepath.Join(base64Dir, baseName)
			if err := writeBase64File(chunkPath, base64Path); err != nil {
				return Result{Logs: logs}, fmt.Errorf("creating base64 dump: %w", err)
			}
			chunk.Base64File = filepath.ToSlash(filepath.Join("base64", baseName))
		}

		if transcribe {
			transcriptPrefix := filepath.Join(transcriptsDir, strings.TrimSuffix(filepath.Base(chunkPath), filepath.Ext(chunkPath)))
			transcriptPath := transcriptPrefix + ".txt"

			args := append([]string{}, p.WhisperArgs...)
			args = append(args,
				"-f", chunkPath,
				"-otxt",
				"-of", transcriptPrefix,
			)
			transcribeLog, err := runCommand(ctx, p.WhisperBin, args...)
			logs = append(logs, transcribeLog)
			if err != nil {
				chunk.TranscriptPreview = fmt.Sprintf("transcription failed: %v", err)
			} else {
				preview, readErr := readPreview(transcriptPath, 400)
				if readErr != nil {
					chunk.TranscriptPreview = fmt.Sprintf("unable to read transcript: %v", readErr)
				} else {
					chunk.TranscriptPreview = preview
					chunk.TranscriptFile = filepath.ToSlash(filepath.Join("transcripts", filepath.Base(transcriptPath)))
				}
			}
		}

		chunks = append(chunks, chunk)
	}

	return Result{Chunks: chunks, Logs: logs}, nil
}

// runCommand executes an external binary and captures combined output.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}

// writeBase64File streams WAV bytes into a matching .b64.txt file.
func writeBase64File(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	encoder := base64.NewEncoder(base64.StdEncoding, out)
	if _, err := io.Copy(encoder, in); err != nil {
		_ = encoder.Close()
		return err
	}

	if err := encoder.Close(); err != nil {
		return err
	}

	return out.Close()
}

// wavDuration inspects a PCM WAV header to compute the clip length.
func wavDuration(path string) (float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return 0, err
	}

	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, errors.New("not a WAV file")
	}

	var sampleRate uint32
	var bitsPerSample uint16
	var channels uint16
	var dataSize uint32

	for {
		var chunkHeader [8]byte
		if _, err := io.ReadFull(file, chunkHeader[:]); err != nil {
			return 0, err
		}

		chunkID := string(chunkHeader[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])

		switch chunkID {
		case "fmt ":
			buf := make([]byte, chunkSize)
			if _, err := io.ReadFull(file, buf); err != nil {
				return 0, err
			}
			if len(buf) < 16 {
				return 0, errors.New("invalid fmt chunk")
			}
			channels = binary.LittleEndian.Uint16(buf[2:4])
			sampleRate = binary.LittleEndian.Uint32(buf[4:8])
			bitsPerSample = binary.LittleEndian.Uint16(buf[14:16])
		case "data":
			dataSize = chunkSize
		default:
			skip := int64(chunkSize)
			if skip%2 == 1 {
				skip++
			}
			if _, err := file.Seek(skip, io.SeekCurrent); err != nil {
				return 0, err
			}
		}

		if chunkID == "data" {
			break
		}
	}

	if sampleRate == 0 || channels == 0 || bitsPerSample == 0 {
		return 0, errors.New("missing audio format information")
	}

	bytesPerSample := (bitsPerSample / 8) * channels
	if bytesPerSample == 0 {
		return 0, errors.New("invalid bytes per sample")
	}

	duration := float64(dataSize) / float64(bytesPerSample) / float64(sampleRate)
	if math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 0, errors.New("invalid duration computed")
	}

	return duration, nil
}

// readPreview loads a short transcript prefix for display in the UI.
func readPreview(path string, limit int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) <= limit {
		return string(data), nil
	}
	return string(data[:limit]) + "...", nil
}
