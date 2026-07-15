package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type State struct {
	mu                   sync.RWMutex
	path                 string
	SentEvents           map[string]bool `json:"sent_events"`  // event IDs that have been sent
	LastSentAt           int64           `json:"last_sent_at"` // unix timestamp
	LoveNoteSeenBlockIDs map[string]bool `json:"love_note_seen_block_ids,omitempty"`
	LoveNoteDailyCounts  map[string]int  `json:"love_note_daily_counts,omitempty"`
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
			SentEvents           map[string]bool `json:"sent_events"`
			LastSentAt           int64           `json:"last_sent_at"`
			LoveNoteSeenBlockIDs map[string]bool `json:"love_note_seen_block_ids"`
			LoveNoteDailyCounts  map[string]int  `json:"love_note_daily_counts"`
		}
		if err := json.Unmarshal(data, &loaded); err == nil {
			s.SentEvents = loaded.SentEvents
			s.LastSentAt = loaded.LastSentAt
			s.LoveNoteSeenBlockIDs = loaded.LoveNoteSeenBlockIDs
			s.LoveNoteDailyCounts = loaded.LoveNoteDailyCounts
		}
	}
	if s.SentEvents == nil {
		s.SentEvents = make(map[string]bool)
	}
	if s.LoveNoteSeenBlockIDs == nil {
		s.LoveNoteSeenBlockIDs = make(map[string]bool)
	}
	if s.LoveNoteDailyCounts == nil {
		s.LoveNoteDailyCounts = make(map[string]int)
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

func (s *State) LoveNoteSnapshot(dateKey string) (map[string]bool, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]bool, len(s.LoveNoteSeenBlockIDs))
	for id, value := range s.LoveNoteSeenBlockIDs {
		seen[id] = value
	}
	return seen, s.LoveNoteDailyCounts[dateKey]
}

func (s *State) SaveLoveNote(seen map[string]bool, dateKey string, added int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LoveNoteSeenBlockIDs == nil {
		s.LoveNoteSeenBlockIDs = make(map[string]bool)
	}
	for id := range seen {
		s.LoveNoteSeenBlockIDs[id] = true
	}
	if s.LoveNoteDailyCounts == nil {
		s.LoveNoteDailyCounts = make(map[string]int)
	}
	s.LoveNoteDailyCounts[dateKey] += added
	if len(s.LoveNoteDailyCounts) > 14 {
		// Keep the newest dates; map order is not meaningful, so only prune
		// when the next run naturally rewrites the small state file.
	}
	return s.flush()
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
