package agenttrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// DirectoryObserver writes one event JSONL file and one report JSON file per
// run. It is safe to share across serve sessions.
type DirectoryObserver struct {
	mu             sync.Mutex
	dir            string
	files          map[string]*os.File
	encoders       map[string]*json.Encoder
	lastReportPath string
	firstErr       error
}

func NewDirectoryObserver(dir string) (*DirectoryObserver, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &DirectoryObserver{
		dir:      dir,
		files:    map[string]*os.File{},
		encoders: map[string]*json.Encoder{},
	}, nil
}

func (o *DirectoryObserver) Observe(event Event) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	encoder, ok := o.encoders[event.RunID]
	if !ok {
		path := o.EventPath(event.RunID)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			o.rememberErr(err)
			return
		}
		o.files[event.RunID] = file
		encoder = json.NewEncoder(file)
		o.encoders[event.RunID] = encoder
	}
	if err := encoder.Encode(event); err != nil {
		o.rememberErr(err)
	}
}

func (o *DirectoryObserver) ObserveReport(report Report) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		path := o.ReportPath(report.RunID)
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, append(data, '\n'), 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
		if err == nil {
			o.lastReportPath = path
		}
	}
	if err != nil {
		o.rememberErr(err)
	}
	if file := o.files[report.RunID]; file != nil {
		if closeErr := file.Close(); closeErr != nil {
			o.rememberErr(closeErr)
		}
		delete(o.files, report.RunID)
		delete(o.encoders, report.RunID)
	}
}

func (o *DirectoryObserver) EventPath(runID string) string {
	return filepath.Join(o.dir, runID+".events.jsonl")
}

func (o *DirectoryObserver) ReportPath(runID string) string {
	return filepath.Join(o.dir, runID+".report.json")
}

func (o *DirectoryObserver) LastReportPath() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastReportPath
}

func (o *DirectoryObserver) Err() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.firstErr
}

func (o *DirectoryObserver) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for runID, file := range o.files {
		if err := file.Close(); err != nil {
			o.rememberErr(err)
		}
		delete(o.files, runID)
		delete(o.encoders, runID)
	}
	return o.firstErr
}

func (o *DirectoryObserver) rememberErr(err error) {
	if err != nil && o.firstErr == nil {
		o.firstErr = err
	}
}
