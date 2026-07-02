package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type State struct {
	mu         sync.RWMutex
	path       string
	SentEvents map[string]bool `json:"sent_events"` // event IDs that have been sent
	LastSentAt int64          `json:"last_sent_at"` // unix timestamp
}

func Load(dataDir, profileID string) (*State, error) {
	dir := filepath.Join(dataDir, profileID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.json")

	s := &State{path: path, SentEvents: make(map[string]bool)}
	if data, err := os.ReadFile(path); err == nil {
		// Use a temporary struct to avoid overwriting path
		var loaded struct {
			SentEvents map[string]bool `json:"sent_events"`
			LastSentAt int64           `json:"last_sent_at"`
		}
		if err := json.Unmarshal(data, &loaded); err == nil {
			s.SentEvents = loaded.SentEvents
			s.LastSentAt = loaded.LastSentAt
		}
	}
	if s.SentEvents == nil {
		s.SentEvents = make(map[string]bool)
	}
	return s, nil
}

func (s *State) MarkSent(eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SentEvents[eventID] = true
	s.LastSentAt = time.Now().Unix()
	return s.flush()
}

func (s *State) HasSent(eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.SentEvents[eventID]
}

func (s *State) FilterNew(eventIDs []string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var new []string
	for _, id := range eventIDs {
		if !s.SentEvents[id] {
			new = append(new, id)
		}
	}
	return new
}

func (s *State) flush() error {
	if s.path == "" {
		return nil // skip if path not set
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}
