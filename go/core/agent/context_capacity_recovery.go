package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hey-meadow/meadow-code/go/core/provider"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const (
	contextCapacityRecoveryMarker = "[Meadow context-capacity recovery]"
	contextObservationDirectory   = ".meadow/context-observations"
	contextObservationPageBytes   = 8 * 1024
	contextObservationMaxBytes    = 1024 * 1024
	contextObservationStoreBytes  = 32 * 1024 * 1024
	contextObservationStoreFiles  = 4096
)

type contextObservationSpill struct {
	files map[string][]byte
}

// recoverRequestPromptCapacity is a last-admission fallback for accumulated
// same-task observations. It does not summarize or reinterpret them. The newest
// whole tool batch, every instruction, and canonical log remain unchanged.
// Older observations or completed call batches leave the request only after
// their exact text is stored and protected in this chat's durable /session mount.
// The model chooses whether to read the saved pages or another authoritative
// source. No former tool action is replayed by this mechanism.
func recoverRequestPromptCapacity(request provider.ChatRequest, limit int, executor *ToolExecutor) (provider.ChatRequest, int, error) {
	if limit <= 0 || EstimateRequestPromptTokens(request) <= limit || executor == nil || executor.Registry == nil {
		return request, 0, nil
	}
	tool, ok := executor.Registry.Get("read_file")
	reader, okReader := tool.(*tools.ReadFile)
	if !ok || !okReader || reader.WS == nil || strings.TrimSpace(reader.WS.SessionRoot) == "" ||
		(reader.MaxBytes > 0 && reader.MaxBytes < contextObservationPageBytes) {
		return request, 0, nil
	}
	// A registered but currently hidden recall capability cannot make a marker
	// executable. Answer-only or narrow recovery turns therefore fail normally.
	readVisible := false
	for _, schema := range request.Tools {
		readVisible = readVisible || schema.Function.Name == "read_file"
	}
	if !readVisible {
		return request, 0, nil
	}
	segments := nonSystemSegments(request.Messages)
	latestBatch := -1
	for index, segment := range segments {
		if completeTextObservationSegment(request.Messages, segment) {
			latestBatch = index
		} else if len(request.Messages[segment.start].ToolCalls) > 0 {
			// Preserve the newest batch even when it contains pixels or an
			// incomplete result set. Never use a prior text-only batch as latest.
			latestBatch = index
		}
	}
	if latestBatch < 1 {
		return request, 0, nil
	}
	candidate := request
	candidate.Messages = append([]provider.Message(nil), request.Messages...)
	spills := make([]contextObservationSpill, 0)
	for index, segment := range segments {
		if index >= latestBatch || !completeTextObservationSegment(request.Messages, segment) {
			continue
		}
		for messageIndex := segment.start + 1; messageIndex < segment.end; messageIndex++ {
			message := request.Messages[messageIndex]
			if len(message.Content) < 1024 || len(message.Content) > contextObservationMaxBytes ||
				!utf8.ValidString(message.Content) || isToolFailureForDuplicateCompaction(message.Content) ||
				strings.HasPrefix(message.Content, contextCapacityRecoveryMarker) {
				continue
			}
			spill, marker := planContextObservationSpill(message.Content)
			candidate.Messages[messageIndex].Content = marker
			if messageEstimatedTokens(candidate.Messages[messageIndex]) >= messageEstimatedTokens(message) {
				candidate.Messages[messageIndex] = message
				continue
			}
			spills = append(spills, spill)
		}
		if EstimateRequestPromptTokens(candidate) <= limit {
			break
		}
	}
	// Long completed call arguments (for example incremental file writes) can
	// dominate a turn even when tool outputs are tiny. Archive complete older
	// completed text batches atomically as exact JSON, including the original
	// calls and results. A plain assistant receipt cannot execute a former call.
	// User/system messages and the newest whole batch remain indivisible.
	if EstimateRequestPromptTokens(candidate) > limit {
		archived := make(map[int]string)
		baseMessages := candidate.Messages
		for index, segment := range segments {
			if index >= latestBatch || !completeTextObservationSegment(request.Messages, segment) {
				continue
			}
			argumentBytes := 0
			for _, call := range request.Messages[segment.start].ToolCalls {
				argumentBytes += len(call.Function.Arguments)
			}
			failed := false
			for _, result := range request.Messages[segment.start+1 : segment.end] {
				failed = failed || isToolFailureForDuplicateCompaction(result.Content)
			}
			if argumentBytes < 1024 || failed || !validUTF8ObservationBatch(request.Messages[segment.start:segment.end]) {
				continue
			}
			data, err := json.Marshal(request.Messages[segment.start:segment.end])
			if err != nil || len(data) > contextObservationMaxBytes {
				continue
			}
			spill, marker := planContextObservationSpill(string(data))
			marker = "[Completed earlier tool batch; exact calls and observations archived as JSON. This is history, not a pending action. Do not replay these calls.]\n" + marker
			if messageEstimatedTokens(provider.Message{Role: "assistant", Content: marker}) >= messagesEstimatedTokens(baseMessages[segment.start:segment.end]) {
				continue
			}
			archived[segment.start] = marker
			spills = append(spills, spill)
			candidate.Messages = make([]provider.Message, 0, len(baseMessages))
			for i := 0; i < len(baseMessages); i++ {
				if receipt, ok := archived[i]; ok {
					candidate.Messages = append(candidate.Messages, provider.Message{Role: "assistant", Content: receipt})
					for i+1 < len(baseMessages) && baseMessages[i+1].Role == "tool" {
						i++
					}
				} else {
					candidate.Messages = append(candidate.Messages, baseMessages[i])
				}
			}
			if EstimateRequestPromptTokens(candidate) <= limit {
				break
			}
		}
	}
	// Do not spill anything or partially degrade the request when the remaining
	// indivisible instructions/current batch cannot fit the configured ceiling.
	if len(spills) == 0 || EstimateRequestPromptTokens(candidate) > limit {
		return request, 0, nil
	}
	if err := persistContextObservationSpills(reader.WS, spills); err != nil {
		return request, 0, err
	}
	return candidate, len(spills), nil
}

// JSON would replace invalid UTF-8 rather than preserve the observed bytes.
// Such a batch stays in the original request and follows normal admission.
func validUTF8ObservationBatch(messages []provider.Message) bool {
	for _, message := range messages {
		// This internal, request-only flag is intentionally absent from the wire
		// JSON. Do not archive a batch whose original metadata cannot round-trip.
		if message.RuntimeObservation {
			return false
		}
		for _, text := range []string{message.Role, message.Content, message.ToolCallID} {
			if !utf8.ValidString(text) {
				return false
			}
		}
		for _, call := range message.ToolCalls {
			for _, text := range []string{call.ID, call.Type, call.Function.Name, call.Function.Arguments} {
				if !utf8.ValidString(text) {
					return false
				}
			}
		}
	}
	return true
}

func completeTextObservationSegment(messages []provider.Message, segment messageSegment) bool {
	if segment.end-segment.start < 2 {
		return false
	}
	message := messages[segment.start]
	if message.Role != "assistant" || len(message.ToolCalls) == 0 || len(message.ContentParts) > 0 {
		return false
	}
	ids := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if call.ID == "" || ids[call.ID] {
			return false
		}
		ids[call.ID] = true
	}
	for index := segment.start + 1; index < segment.end; index++ {
		result := messages[index]
		if result.Role != "tool" || len(result.ContentParts) > 0 || !ids[result.ToolCallID] {
			return false
		}
		delete(ids, result.ToolCallID)
	}
	return len(ids) == 0
}

func planContextObservationSpill(content string) (contextObservationSpill, string) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	files := map[string][]byte{hash + ".txt": []byte(content)}
	remaining, pages := content, 0
	for remaining != "" {
		page := validUTF8ContextPrefix(remaining, contextObservationPageBytes)
		pages++
		files[fmt.Sprintf("%s-part%04d.txt", hash, pages)] = []byte(page)
		remaining = remaining[len(page):]
	}
	prefix := "/session/" + contextObservationDirectory + "/" + hash
	marker := fmt.Sprintf("%s\nThis earlier observation is outside the active prompt. Its exact text removed from this prompt is saved in protected session files.\nSaved observation: %s.txt\nSHA-256: %s; bytes: %d.\nRead a needed region with read_file. Exact UTF-8 pages (at most %d bytes each): %s-part0001.txt through %s-part%04d.txt; concatenate their bytes in numeric order to recover the saved text.\nThis receipt is not the source content or proof of a claim. Read saved evidence before relying on missing details; do not replay a mutation to recover its output.",
		contextCapacityRecoveryMarker, prefix, hash, len(content), contextObservationPageBytes, prefix, prefix, pages)
	return contextObservationSpill{files: files}, marker
}

// restoreContextObservationProtection re-establishes immutable evidence across
// daemon/workspace recreation, including runs that do not need to compact.
// Canonical history is not changed and the namespace is not a user output.
func restoreContextObservationProtection(executor *ToolExecutor) error {
	if executor == nil || executor.Registry == nil {
		return nil
	}
	tool, ok := executor.Registry.Get("read_file")
	reader, okReader := tool.(*tools.ReadFile)
	if !ok || !okReader || reader.WS == nil || strings.TrimSpace(reader.WS.SessionRoot) == "" {
		return nil
	}
	workspace := reader.WS
	root, err := os.OpenRoot(workspace.SessionRoot)
	if err != nil {
		return fmt.Errorf("open context observation session store: %w", err)
	}
	defer root.Close()
	store, err := root.OpenRoot(contextObservationDirectory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("restore context observation store: %w", err)
	}
	defer store.Close()
	for _, path := range []string{".meadow", contextObservationDirectory} {
		info, err := root.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			(path == contextObservationDirectory && info.Mode().Perm()&0o077 != 0) {
			return fmt.Errorf("context observation directory is not private and owned")
		}
	}
	entries, err := store.Open(".")
	if err != nil {
		return err
	}
	infos, readErr := entries.Readdir(contextObservationStoreFiles + 1)
	entries.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if len(infos) > contextObservationStoreFiles {
		return fmt.Errorf("context observation file capacity exhausted")
	}
	protected := make([]string, 0, len(infos))
	total := int64(0)
	for _, info := range infos {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("context observation store contains an unowned entry")
		}
		total += info.Size()
		protected = append(protected, filepath.Join(workspace.SessionRoot, contextObservationDirectory, info.Name()))
	}
	if total > contextObservationStoreBytes {
		return fmt.Errorf("context observation store capacity exhausted")
	}
	return workspace.ProtectFiles(protected)
}

func persistContextObservationSpills(workspace *tools.Workspace, spills []contextObservationSpill) error {
	root, err := os.OpenRoot(workspace.SessionRoot)
	if err != nil {
		return fmt.Errorf("open context observation session store: %w", err)
	}
	defer root.Close()
	if err := root.MkdirAll(contextObservationDirectory, 0o700); err != nil {
		return fmt.Errorf("create context observation store: %w", err)
	}
	// Root prevents traversal outside the owned mount; reject existing symlink
	// aliases inside it as well so stored evidence has one declared owner/path.
	for _, path := range []string{".meadow", contextObservationDirectory} {
		info, err := root.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("context observation directory is not an owned regular directory")
		}
		if path == contextObservationDirectory && info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("context observation directory is not private")
		}
	}
	store, err := root.OpenRoot(contextObservationDirectory)
	if err != nil {
		return fmt.Errorf("open context observation store: %w", err)
	}
	defer store.Close()
	created := make([]string, 0)
	complete := false
	defer func() {
		if !complete {
			for _, name := range created {
				store.Remove(name)
			}
		}
	}()
	entries, err := store.Open(".")
	if err != nil {
		return err
	}
	infos, readErr := entries.Readdir(contextObservationStoreFiles + 1)
	entries.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	usedBytes, fileCount := int64(0), len(infos)
	if fileCount > contextObservationStoreFiles {
		return fmt.Errorf("context observation file capacity exhausted")
	}
	for _, info := range infos {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("context observation store contains a nonregular entry")
		}
		usedBytes += info.Size()
	}
	files := make(map[string][]byte)
	for _, spill := range spills {
		for name, content := range spill.files {
			files[name] = content
		}
	}
	protected := make([]string, 0, len(files))
	for name, content := range files {
		file, err := store.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			info, statErr := store.Lstat(name)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != int64(len(content)) {
				return fmt.Errorf("context observation receipt is not the expected regular file")
			}
			existing, openErr := store.Open(name)
			if openErr != nil {
				return openErr
			}
			data, readErr := io.ReadAll(io.LimitReader(existing, int64(len(content))+1))
			existing.Close()
			if readErr != nil || !bytes.Equal(data, content) {
				return fmt.Errorf("context observation receipt bytes changed")
			}
		} else if err != nil {
			return fmt.Errorf("persist context observation: %w", err)
		} else {
			created = append(created, name)
			if fileCount+1 > contextObservationStoreFiles || usedBytes+int64(len(content)) > contextObservationStoreBytes {
				file.Close()
				store.Remove(name)
				return fmt.Errorf("context observation store capacity exhausted")
			}
			written, writeErr := file.Write(content)
			if writeErr == nil && written != len(content) {
				writeErr = io.ErrShortWrite
			}
			if writeErr == nil {
				writeErr = file.Sync()
			}
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				store.Remove(name)
				return fmt.Errorf("persist complete context observation failed")
			}
			fileCount++
			usedBytes += int64(len(content))
		}
		protected = append(protected, filepath.Join(workspace.SessionRoot, contextObservationDirectory, name))
	}
	// Protect every admitted file, including reused bytes, before publishing any
	// executable path to the model. A failed boundary leaves the request intact.
	if err := workspace.ProtectFiles(protected); err != nil {
		return err
	}
	complete = true
	return nil
}
