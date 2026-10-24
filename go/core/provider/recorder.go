package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const transcriptSeparator = "\n### SSE\n"

// Recorder persists one file per completion call: the JSON request body,
// a separator, then the raw SSE stream exactly as received. These files are
// the golden-test corpus for the streaming parser.
type Recorder struct {
	Dir    string
	Prefix string
	mu     sync.Mutex
	n      int
	err    error
}

func NewRecorder(dir, prefix string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Recorder{Dir: dir, Prefix: prefix}, nil
}

func (r *Recorder) nextPath() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		r.n++
		path := filepath.Join(r.Dir, fmt.Sprintf("%s_%02d.transcript.txt", r.Prefix, r.n))
		// Reserve across Recorder instances/restarts, not just goroutines sharing r.
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
}

func (r *Recorder) Save(requestBody, rawSSE []byte) (string, error) {
	path, err := r.nextPath()
	if err != nil {
		r.noteError(err)
		return "", err
	}
	if err := writeTranscript(path, requestBody, rawSSE); err != nil {
		r.noteError(err)
		return "", err
	}
	return path, nil
}

func writeTranscript(path string, requestBody, rawResponse []byte) error {
	var buf []byte
	buf = append(buf, "### REQUEST\n"...)
	buf = append(buf, requestBody...)
	buf = append(buf, transcriptSeparator...)
	buf = append(buf, rawResponse...)
	return writeRecorderFile(path, buf)
}

// CompletionLifecycle describes one client-side HTTP Do invocation, not a
// server/backend receipt or a count of redirects/transport retries. "started"
// is persisted immediately before Do; an interrupted process may leave it in
// that state. A finished error never fabricates bytes that were not observed.
// URLs, headers and error strings are deliberately excluded: they can contain
// credentials. The ordinary transcript remains the raw-observation corpus.
type CompletionLifecycle struct {
	Version       int        `json:"version"`
	Scope         string     `json:"scope"`
	State         string     `json:"state"`
	RequestID     string     `json:"request_id,omitempty"`
	Streaming     bool       `json:"streaming"`
	StartedAt     time.Time  `json:"started_at"`
	StartRecorded bool       `json:"start_recorded"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	HTTPStatus    int        `json:"http_status,omitempty"`
	ResponseBytes int        `json:"response_bytes"`
	Outcome       string     `json:"outcome,omitempty"`
	ErrorKind     string     `json:"error_kind,omitempty"`
}

type completionRecording struct {
	path        string
	requestBody []byte
	lifecycle   CompletionLifecycle
	recorder    *Recorder
}

func (r *Recorder) beginCompletion(requestBody []byte, requestID string, streaming bool) *completionRecording {
	if r == nil {
		return nil
	}
	path, err := r.nextPath()
	if err != nil {
		r.noteError(err)
		return nil
	}
	record := &completionRecording{
		path: path, requestBody: requestBody, recorder: r,
		lifecycle: CompletionLifecycle{
			Version: 1, Scope: "client_http_do_invocation", State: "started",
			RequestID: requestID, Streaming: streaming, StartedAt: time.Now().UTC(),
		},
	}
	// Recording is diagnostic and best effort, never a reason to change model
	// execution. Finish retries these writes if storage is temporarily unavailable.
	if err := writeTranscript(record.path, requestBody, nil); err != nil {
		r.noteError(err)
	} else {
		record.lifecycle.StartRecorded = true
	}
	if err := record.saveLifecycle(); err != nil {
		record.lifecycle.StartRecorded = false
		r.noteError(err)
	}
	return record
}

func (r *completionRecording) finish(rawResponse []byte, status int, completionErr error, ctxErr error) (string, error) {
	if r == nil {
		return "", nil
	}
	finishedAt := time.Now().UTC()
	r.lifecycle.State = "finished"
	r.lifecycle.FinishedAt = &finishedAt
	r.lifecycle.HTTPStatus = status
	r.lifecycle.ResponseBytes = len(rawResponse)
	r.lifecycle.Outcome = "returned_without_error"
	if completionErr != nil {
		r.lifecycle.Outcome = "returned_error"
		switch {
		case errors.Is(completionErr, ErrGenerationStall):
			r.lifecycle.ErrorKind = "generation_stall"
		case errors.Is(completionErr, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded):
			r.lifecycle.ErrorKind = "deadline_exceeded"
		case errors.Is(completionErr, context.Canceled) || errors.Is(ctxErr, context.Canceled):
			r.lifecycle.ErrorKind = "context_canceled"
		case status == 0:
			r.lifecycle.ErrorKind = "transport"
		case status != 200:
			r.lifecycle.ErrorKind = "http_status"
		default:
			r.lifecycle.ErrorKind = "completion"
		}
	}
	if err := writeTranscript(r.path, r.requestBody, rawResponse); err != nil {
		r.recorder.noteError(err)
		return "", err
	}
	if err := r.saveLifecycle(); err != nil {
		r.recorder.noteError(err)
		return "", err
	}
	return r.path, nil
}

// Err returns the first diagnostic persistence failure. It is sticky: later
// successful calls do not make a recorder with missing evidence complete.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Recorder) noteError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	first := r.err == nil
	if first {
		r.err = err
	}
	r.mu.Unlock()
	if first {
		log.Printf("provider recorder incomplete: %v", err)
	}
}

func (r *completionRecording) saveLifecycle() error {
	body, err := json.MarshalIndent(r.lifecycle, "", "  ")
	if err != nil {
		return err
	}
	return writeRecorderFile(r.path+".lifecycle.json", append(body, '\n'))
}

// Atomic replacement prevents an observer seeing a partially rewritten record.
// Private permissions also keep request/response diagnostics local to the user.
func writeRecorderFile(path string, body []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".provider-record-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// SplitTranscript separates a recorded transcript back into request body and
// raw SSE bytes.
func SplitTranscript(data []byte) (request, sse []byte, err error) {
	idx := -1
	sep := []byte(transcriptSeparator)
	for i := 0; i+len(sep) <= len(data); i++ {
		if string(data[i:i+len(sep)]) == string(sep) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, nil, fmt.Errorf("transcript missing %q separator", transcriptSeparator)
	}
	req := data[:idx]
	if len(req) >= len("### REQUEST\n") {
		req = req[len("### REQUEST\n"):]
	}
	return req, data[idx+len(sep):], nil
}
