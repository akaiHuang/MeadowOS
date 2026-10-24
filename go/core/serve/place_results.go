package serve

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	agentcore "github.com/hey-meadow/meadow-code/go/core/agent"
	"github.com/hey-meadow/meadow-code/go/core/tools"
)

const maxPlaceRecords = 50

type placeRecord struct {
	RunID  string `json:"runId"`
	CallID string `json:"callId"`
	// RecordedAt is the turn's receipt time. Provider ObservedAt can be older
	// when a cached result is observed again in a later conversation turn.
	RecordedAt time.Time `json:"recordedAt"`
	tools.PlaceCollection
}

func clonePlaceRecords(records []placeRecord) []placeRecord {
	result := make([]placeRecord, 0, len(records))
	for _, record := range records {
		record.PlaceCollection = *tools.ClonePlaceCollection(&record.PlaceCollection)
		if record.RecordedAt.IsZero() {
			record.RecordedAt, _ = time.Parse(time.RFC3339Nano, record.ObservedAt)
		}
		result = append(result, record)
	}
	return result
}

func normalizedPlaceRecords(records []placeRecord) []placeRecord {
	result := []placeRecord{}
	for _, record := range records {
		if strings.TrimSpace(record.RunID) == "" || strings.TrimSpace(record.CallID) == "" || len(record.RunID) > 512 || len(record.CallID) > 512 || tools.ValidatePlaceCollection(&record.PlaceCollection) != nil {
			continue
		}
		record.PlaceCollection = *tools.ClonePlaceCollection(&record.PlaceCollection)
		if record.RecordedAt.IsZero() {
			record.RecordedAt, _ = time.Parse(time.RFC3339Nano, record.ObservedAt)
		}
		index := -1
		for i := range result {
			if result[i].RunID == record.RunID && result[i].CallID == record.CallID {
				index = i
				break
			}
		}
		if index >= 0 {
			result[index] = record
		} else {
			result = append(result, record)
		}
	}
	if len(result) > maxPlaceRecords {
		result = result[len(result)-maxPlaceRecords:]
	}
	return result
}

func (sess *serveSession) placeSnapshot() []placeRecord {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return clonePlaceRecords(sess.placeResults)
}

func (sess *serveSession) hasPlaceResults() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, record := range sess.placeResults {
		if len(record.Places) > 0 {
			return true
		}
	}
	return false
}

func (sess *serveSession) recordPlaceResults(execution agentcore.ToolExecution) error {
	if !execution.Executed || execution.Status != agentcore.ToolOK || execution.Err != nil || execution.Places == nil {
		return nil
	}
	if err := tools.ValidatePlaceCollection(execution.Places); err != nil {
		return fmt.Errorf("invalid place observation: %w", err)
	}
	sess.mu.Lock()
	if strings.TrimSpace(sess.activeRunID) == "" || strings.TrimSpace(execution.CallID) == "" {
		sess.mu.Unlock()
		return fmt.Errorf("place observation requires run and tool-call ownership")
	}
	previous := sess.placeResults
	record := placeRecord{RunID: sess.activeRunID, CallID: execution.CallID, RecordedAt: time.Now().UTC(), PlaceCollection: *tools.ClonePlaceCollection(execution.Places)}
	for _, existing := range previous {
		if existing.RunID == record.RunID && existing.CallID == record.CallID {
			record.RecordedAt = existing.RecordedAt
			break
		}
	}
	sess.placeResults = normalizedPlaceRecords(append(clonePlaceRecords(previous), record))
	if err := sess.persistWorkCenterLocked(); err != nil {
		sess.placeResults = previous
		sess.mu.Unlock()
		sess.broadcast("work_center_error", map[string]any{"sessionId": sess.id, "operation": "persist_places", "message": err.Error()})
		return err
	}
	// Keep persistence and notification ordered across concurrent tool finishes.
	sess.broadcastLocked("place_results", map[string]any{"sessionId": sess.id, "places": clonePlaceRecords(sess.placeResults)})
	sess.mu.Unlock()
	return nil
}

func (s *Server) handlePlaces(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "unknown session")
		return
	}
	writeJSON(w, map[string]any{"places": sess.placeSnapshot()})
}
