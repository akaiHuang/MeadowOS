package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/session"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	maxBackgroundTasks = 100
	maxResearchSources = 200
	maxWorkspaceTasks  = 500
)

type backgroundTaskRecord struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"sessionId,omitempty"`
	Kind       string     `json:"kind"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	StopReason string     `json:"stopReason,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type researchSourceRecord struct {
	ID                string    `json:"id"`
	Title             string    `json:"title"`
	URL               string    `json:"url"`
	Tool              string    `json:"tool"`
	CallID            string    `json:"callId,omitempty"`
	RunID             string    `json:"runId,omitempty"`
	ObservedAt        time.Time `json:"observedAt"`
	Snippet           string    `json:"snippet,omitempty"`
	SnippetTruncated  bool      `json:"snippetTruncated,omitempty"`
	ContentKind       string    `json:"contentKind,omitempty"`
	ThumbnailURL      string    `json:"thumbnailURL,omitempty"`
	FetchedAt         string    `json:"fetchedAt,omitempty"`
	SourceVersion     string    `json:"sourceVersion,omitempty"`
	FetchError        string    `json:"fetchError,omitempty"`
	InputTruncated    bool      `json:"inputTruncated,omitempty"`
	EvidenceTruncated bool      `json:"evidenceTruncated,omitempty"`
}

type workCenterDiskState struct {
	Version int                    `json:"version"`
	Tasks   []backgroundTaskRecord `json:"tasks"`
	Sources []researchSourceRecord `json:"sources"`
	Places  []placeRecord          `json:"places,omitempty"`
}

func (sess *serveSession) workCenterPath() string {
	key := sess.queueKey
	if key == "" {
		key = sess.id
	}
	return filepath.Join(
		session.PrimaryDir(), "projects", session.ProjectDirName(sess.cwd),
		"desktop", session.ProjectDirName(key)+".json",
	)
}

func (sess *serveSession) loadWorkCenter() error {
	b, err := os.ReadFile(sess.workCenterPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Decode place receipts separately: one damaged coordinate must not make
	// the user's other maps, source receipts, or background tasks disappear.
	var state struct {
		Tasks   []backgroundTaskRecord `json:"tasks"`
		Sources []researchSourceRecord `json:"sources"`
		Places  []json.RawMessage      `json:"places"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return err
	}
	queued := make(map[string]struct{}, len(sess.queuedPrompts))
	for _, item := range sess.queuedPrompts {
		if item != nil {
			queued[item.ID] = struct{}{}
		}
	}
	normalized := false
	for _, task := range state.Tasks {
		if strings.TrimSpace(task.ID) == "" {
			continue
		}
		if task.Status == "running" || task.Status == "cancelling" {
			if _, ok := queued[task.ID]; ok {
				task.Status = "queued"
				task.StartedAt = nil
				task.FinishedAt = nil
			} else {
				// Queue removal is persisted before the terminal task record. If
				// the process dies between those writes, do not resurrect a ghost
				// job that has no executable prompt body.
				finished := time.Now().UTC()
				task.Status = "interrupted"
				task.StopReason = "process_interrupted"
				task.FinishedAt = &finished
			}
			normalized = true
		}
		sess.backgroundTasks = append(sess.backgroundTasks, task)
	}
	for _, source := range state.Sources {
		if strings.TrimSpace(source.ID) == "" || strings.TrimSpace(source.URL) == "" {
			continue
		}
		metadata, ok := tools.NormalizeEvidenceSource(source.evidenceSource())
		if !ok {
			continue
		}
		source.setEvidenceMetadata(metadata)
		sess.researchSources = append(sess.researchSources, source)
	}
	placeRecords := make([]placeRecord, 0, len(state.Places))
	for _, raw := range state.Places {
		var record placeRecord
		if json.Unmarshal(raw, &record) == nil {
			placeRecords = append(placeRecords, record)
		}
	}
	sess.placeResults = normalizedPlaceRecords(placeRecords)
	terminalTasks := make(map[string]struct{}, len(sess.backgroundTasks))
	for _, task := range sess.backgroundTasks {
		if backgroundTaskTerminal(task.Status) {
			terminalTasks[task.ID] = struct{}{}
		}
	}
	if len(terminalTasks) > 0 && len(sess.queuedPrompts) > 0 {
		kept := sess.queuedPrompts[:0]
		for _, item := range sess.queuedPrompts {
			if item == nil {
				continue
			}
			if _, terminal := terminalTasks[item.ID]; terminal {
				normalized = true
				continue
			}
			kept = append(kept, item)
		}
		sess.queuedPrompts = kept
		if normalized {
			if err := sess.persistQueueLocked(); err != nil {
				return err
			}
		}
	}
	if normalized {
		return sess.persistWorkCenterLocked()
	}
	return nil
}

func backgroundTaskTerminal(status string) bool {
	return status != "" && status != "queued" && status != "guiding" && status != "running" && status != "cancelling"
}

func (sess *serveSession) persistWorkCenterLocked() error {
	state := workCenterDiskState{
		Version: 1,
		Tasks:   append([]backgroundTaskRecord(nil), sess.backgroundTasks...),
		Sources: append([]researchSourceRecord(nil), sess.researchSources...),
		Places:  clonePlaceRecords(sess.placeResults),
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := sess.workCenterPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (sess *serveSession) taskSnapshot() []backgroundTaskRecord {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	tasks := append([]backgroundTaskRecord(nil), sess.backgroundTasks...)
	for i := range tasks {
		tasks[i].SessionID = sess.id
	}
	return tasks
}

func (sess *serveSession) sourceSnapshot() []researchSourceRecord {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return append([]researchSourceRecord(nil), sess.researchSources...)
}

func (sess *serveSession) recordBackgroundTask(update backgroundTaskRecord) error {
	if strings.TrimSpace(update.ID) == "" {
		return nil
	}
	now := time.Now().UTC()
	sess.mu.Lock()
	previous := append([]backgroundTaskRecord(nil), sess.backgroundTasks...)
	index := -1
	for i := range sess.backgroundTasks {
		if sess.backgroundTasks[i].ID == update.ID {
			index = i
			break
		}
	}
	if index < 0 {
		if update.CreatedAt.IsZero() {
			update.CreatedAt = now
		}
		sess.backgroundTasks = append(sess.backgroundTasks, update)
		index = len(sess.backgroundTasks) - 1
	} else {
		current := sess.backgroundTasks[index]
		if update.Kind != "" {
			current.Kind = update.Kind
		}
		if update.Title != "" {
			current.Title = update.Title
		}
		// Task state is monotonic at the terminal boundary. Concurrent cancel
		// delivery can publish an in-flight "cancelling" update just after the
		// runtime has already recorded "cancelled"; never resurrect completed,
		// cancelled, or failed work as active.
		if update.Status != "" && !(backgroundTaskTerminal(current.Status) && !backgroundTaskTerminal(update.Status)) {
			current.Status = update.Status
		}
		if update.StopReason != "" {
			current.StopReason = update.StopReason
		}
		if update.StartedAt != nil {
			current.StartedAt = update.StartedAt
		}
		if update.FinishedAt != nil {
			current.FinishedAt = update.FinishedAt
		}
		update = current
		sess.backgroundTasks[index] = current
	}
	if len(sess.backgroundTasks) > maxBackgroundTasks {
		sess.backgroundTasks = append([]backgroundTaskRecord(nil), sess.backgroundTasks[len(sess.backgroundTasks)-maxBackgroundTasks:]...)
	}
	if err := sess.persistWorkCenterLocked(); err != nil {
		sess.backgroundTasks = previous
		sess.mu.Unlock()
		sess.broadcast("work_center_error", map[string]any{
			"sessionId": sess.id, "operation": "persist_task", "message": err.Error(),
		})
		return err
	}
	tasks := append([]backgroundTaskRecord(nil), sess.backgroundTasks...)
	sess.mu.Unlock()
	sess.broadcast("background_tasks", map[string]any{"sessionId": sess.id, "items": tasks})
	return nil
}

func (sess *serveSession) recordResearchSources(execution agentcore.ToolExecution) error {
	if execution.Status != agentcore.ToolOK || len(execution.EvidenceSources) == 0 {
		return nil
	}
	now := time.Now().UTC()
	changed := false
	sess.mu.Lock()
	previous := append([]researchSourceRecord(nil), sess.researchSources...)
	for _, source := range execution.EvidenceSources {
		source, valid := tools.NormalizeEvidenceSource(source)
		if !valid {
			continue
		}
		url := source.URL
		index := -1
		for i := range sess.researchSources {
			if sess.researchSources[i].URL == url && sess.researchSources[i].RunID == sess.activeRunID {
				index = i
				break
			}
		}
		if index >= 0 {
			current := &sess.researchSources[index]
			before := *current
			// A later opened page can enrich an earlier search hit. Preserve
			// receipt fields as one unit so versions and excerpts never mix.
			if current.mergeEvidenceMetadata(source) {
				current.Tool, current.CallID, current.ObservedAt = execution.Tool, execution.CallID, now
			}
			changed = changed || before != *current
			continue
		}
		record := researchSourceRecord{
			ID: session.NewUUID(), Title: strings.TrimSpace(source.Title), URL: url,
			Tool: execution.Tool, CallID: execution.CallID, RunID: sess.activeRunID, ObservedAt: now,
		}
		record.setEvidenceMetadata(source)
		sess.researchSources = append(sess.researchSources, record)
		changed = true
	}
	if !changed {
		sess.mu.Unlock()
		return nil
	}
	if len(sess.researchSources) > maxResearchSources {
		sess.researchSources = append([]researchSourceRecord(nil), sess.researchSources[len(sess.researchSources)-maxResearchSources:]...)
	}
	if err := sess.persistWorkCenterLocked(); err != nil {
		sess.researchSources = previous
		sess.mu.Unlock()
		sess.broadcast("work_center_error", map[string]any{
			"sessionId": sess.id, "operation": "persist_source", "message": err.Error(),
		})
		return err
	}
	sources := append([]researchSourceRecord(nil), sess.researchSources...)
	sess.mu.Unlock()
	sess.broadcast("research_sources", map[string]any{"sessionId": sess.id, "items": sources})
	return nil
}

func (source researchSourceRecord) evidenceSource() tools.EvidenceSource {
	return tools.EvidenceSource{
		Title: source.Title, URL: source.URL, Snippet: source.Snippet,
		SnippetTruncated: source.SnippetTruncated,
		ContentKind:      source.ContentKind, ThumbnailURL: source.ThumbnailURL,
		FetchedAt: source.FetchedAt, SourceVersion: source.SourceVersion,
		FetchError: source.FetchError, InputTruncated: source.InputTruncated,
		EvidenceTruncated: source.EvidenceTruncated,
	}
}

func (source *researchSourceRecord) setEvidenceMetadata(metadata tools.EvidenceSource) {
	source.Title, source.URL = metadata.Title, metadata.URL
	source.Snippet, source.ContentKind = metadata.Snippet, metadata.ContentKind
	source.SnippetTruncated = metadata.SnippetTruncated
	source.ThumbnailURL, source.FetchedAt = metadata.ThumbnailURL, metadata.FetchedAt
	source.SourceVersion = metadata.SourceVersion
	source.FetchError = metadata.FetchError
	source.InputTruncated, source.EvidenceTruncated = metadata.InputTruncated, metadata.EvidenceTruncated
}

func (source *researchSourceRecord) mergeEvidenceMetadata(metadata tools.EvidenceSource) bool {
	if source.Title == "" {
		source.Title = metadata.Title
	}
	if metadata.Snippet != "" || metadata.ContentKind != "" || metadata.ThumbnailURL != "" ||
		metadata.FetchedAt != "" || metadata.SourceVersion != "" || metadata.FetchError != "" ||
		metadata.InputTruncated || metadata.EvidenceTruncated {
		if metadata.Title == "" {
			metadata.Title = source.Title
		}
		source.setEvidenceMetadata(metadata)
		return true
	}
	return false
}

func presentationEvidenceSources(execution agentcore.ToolExecution) []tools.EvidenceSource {
	if execution.Status != agentcore.ToolOK {
		return nil
	}
	var sources []tools.EvidenceSource
	for _, source := range execution.EvidenceSources {
		if metadata, ok := tools.NormalizeEvidenceSource(source); ok {
			sources = append(sources, metadata)
		}
	}
	return sources
}

func (s *Server) handleBackgroundTasks(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	writeJSON(w, map[string]any{"sessionId": sess.id, "items": sess.taskSnapshot()})
}

func (s *Server) handleAllBackgroundTasks(w http.ResponseWriter, r *http.Request) {
	cwd := strings.TrimSpace(r.URL.Query().Get("cwd"))
	if cwd == "" {
		cwd = s.WorkspaceCwd
	}
	items, warnings := persistedWorkspaceTasks(cwd)
	seen := make(map[string]int, len(items))
	for index, task := range items {
		seen[task.ID] = index
	}
	s.mu.Lock()
	sessions := make([]*serveSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.cwd == cwd {
			sessions = append(sessions, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		for _, task := range sess.taskSnapshot() {
			if index, duplicate := seen[task.ID]; duplicate {
				// A live session is authoritative over the disk snapshot and carries
				// the routable session id required for cancel.
				items[index] = task
				continue
			}
			seen[task.ID] = len(items)
			items = append(items, task)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	if len(items) > maxWorkspaceTasks {
		items = append([]backgroundTaskRecord(nil), items[len(items)-maxWorkspaceTasks:]...)
	}
	response := map[string]any{"cwd": cwd, "items": items}
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}
	writeJSON(w, response)
}

// persistedWorkspaceTasks makes the task center useful immediately after an
// app restart, before the user reopens every individual chat. Disk-only tasks
// intentionally carry no live SessionID, so the Desktop will display them but
// cannot route a cancel request to the wrong conversation.
func persistedWorkspaceTasks(cwd string) ([]backgroundTaskRecord, []string) {
	projectDir := filepath.Join(session.PrimaryDir(), "projects", session.ProjectDirName(cwd))
	desktopDir := filepath.Join(projectDir, "desktop")
	entries, err := os.ReadDir(desktopDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("read work center: %v", err)}
	}

	items := make([]backgroundTaskRecord, 0)
	warnings := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(desktopDir, entry.Name())
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("read %s: %v", entry.Name(), readErr))
			continue
		}
		var state workCenterDiskState
		if unmarshalErr := json.Unmarshal(b, &state); unmarshalErr != nil {
			warnings = append(warnings, fmt.Sprintf("parse %s: %v", entry.Name(), unmarshalErr))
			continue
		}
		queued := persistedQueueIDs(filepath.Join(projectDir, "queues", entry.Name()))
		fallbackFinished := time.Now().UTC()
		if info, infoErr := entry.Info(); infoErr == nil {
			fallbackFinished = info.ModTime().UTC()
		}
		for _, task := range state.Tasks {
			if strings.TrimSpace(task.ID) == "" {
				continue
			}
			task.SessionID = ""
			if task.Status == "running" || task.Status == "cancelling" {
				if _, ok := queued[task.ID]; ok {
					task.Status = "queued"
					task.StartedAt = nil
					task.FinishedAt = nil
				} else {
					task.Status = "interrupted"
					task.StopReason = "process_interrupted"
					task.FinishedAt = &fallbackFinished
				}
			}
			items = append(items, task)
		}
	}
	return items, warnings
}

func persistedQueueIDs(path string) map[string]struct{} {
	ids := map[string]struct{}{}
	b, err := os.ReadFile(path)
	if err != nil {
		return ids
	}
	var state queueDiskState
	if json.Unmarshal(b, &state) != nil {
		return ids
	}
	for _, item := range state.Items {
		if item != nil && strings.TrimSpace(item.ID) != "" {
			ids[item.ID] = struct{}{}
		}
	}
	return ids
}

func (s *Server) handleResearchSources(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	writeJSON(w, map[string]any{"sessionId": sess.id, "items": sess.sourceSnapshot()})
}
