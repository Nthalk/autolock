package main

import (
	"encoding/json"
	"os"
	"time"
)

// stateRetention drops state entries whose target is older than this on load,
// so the file doesn't grow without bound.
const stateRetention = 90 * 24 * time.Hour

// State records which actions have already fired, so re-runs (cron at any
// cadence) never perform the same action twice. The key encodes the
// reservation, action, target time, and the exact call — so a changed date or
// code re-fires.
type State struct {
	path string
	done map[string]string // key -> target RFC3339
}

// LoadState reads the state file (a missing file is an empty state) and prunes
// entries older than stateRetention.
func LoadState(path string) (*State, error) {
	s := &State{path: path, done: map[string]string{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.done); err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-stateRetention)
	for k, ts := range s.done {
		if t, err := time.Parse(time.RFC3339, ts); err == nil && t.Before(cutoff) {
			delete(s.done, k)
		}
	}
	return s, nil
}

func (s *State) Has(key string) bool {
	_, ok := s.done[key]
	return ok
}

// Mark records key as done and persists the file. No-op persistence when the
// state path is empty (state disabled).
func (s *State) Mark(key string, target time.Time) error {
	s.done[key] = target.Format(time.RFC3339)
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.done, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
