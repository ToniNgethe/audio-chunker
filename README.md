# Audio Chunker

A lightweight Go web service that accepts a video upload, slices it into audio chunks, optionally transcribes each chunk, and exposes every artefact (audio files, Base64 dumps, transcripts, logs) for easy review and copy/paste into tools such as ChatGPT or Gemini.

## Features

- Upload any video file supported by `ffmpeg`.
- Convert the audio track to mono 16 kHz PCM and split it into time-based chunks.
- Generate Base64 text dumps for every chunk so you can copy audio into text-only workflows.
- Serve chunk files directly for playback or download in the browser.
- Optional automatic transcription via an external `whisper.cpp` (or compatible) binary.
- Persist job metadata, logs, and outputs under `data/jobs/<job-id>/` for later access.

## Prerequisites

- Go 1.20+ (tested with Go 1.23).
- `ffmpeg` available on your `$PATH`, or set `FFMPEG_BIN` to the full executable path.
- (Optional) A transcription binary. The server expects a `whisper.cpp`-style CLI when `WHISPER_BIN` is defined. You can also add extra launch parameters via `WHISPER_ARGS`.

> **Transcription setup tip:** If you're using `whisper.cpp`, build the CLI and set:
>
> ```bash
> export WHISPER_BIN=/path/to/whisper
> export WHISPER_ARGS="-m /path/to/models/ggml-base.en.bin"
> ```
>
> The server will invoke `whisper` with `-f`, `-otxt`, and `-of` flags for each chunk and surface the resulting `.txt` files.

## Running the server

```bash
# Install dependencies (ffmpeg must already be installed locally)
go run ./cmd/server -addr :8080
```

Flags:

- `-addr` – HTTP address to listen on (default `:8080`).
- `-data` – Root directory for output artefacts (default `./data`).
- `-chunk` – Default chunk duration in seconds (default `300`).
- `-no-base64` – Disable Base64 dump generation if you only need the audio files.

Environment variables:

- `FFMPEG_BIN` – Override the ffmpeg executable name/path.
- `WHISPER_BIN` – Path to a transcription binary (enables the “Transcribe” checkbox in the UI).
- `WHISPER_ARGS` – Additional arguments (split on spaces) passed to the transcription command before the per-chunk parameters.

## Workflow

1. Open the UI at `http://localhost:8080`.
2. Upload a video and choose the chunk duration.
3. (Optional) Tick “Attempt transcription” if `WHISPER_BIN` is configured.
4. Wait for processing to finish. The job page lists every chunk with an inline audio player, download links, Base64 dumps, and transcript previews.
5. Copy Base64 dumps or transcript text into your preferred analysis tool.

Generated artefacts live under `data/jobs/<job-id>/`:

- `original/` – Uploaded source video.
- `chunks/` – WAV files per chunk.
- `base64/` – Text files containing Base64-encoded audio (omit or disable with `-no-base64`).
- `transcripts/` – Transcription text files (when enabled).
- `job.json` – Metadata driving the UI.

Processing logs are visible on each job page and stored alongside the metadata to aid debugging.

## Development

- Build: `go build ./...`
- Format: `gofmt -w $(find . -name '*.go')`
- The UI templates live under `web/templates/`.

If you encounter permission errors during processing, verify that `ffmpeg` is installed and executable by the server process.
