// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cleanup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EnrolledPodState represents the persistent state of an enrolled pod.
type EnrolledPodState struct {
	PodUID     string    `json:"podUID"`
	Namespace  string    `json:"namespace"`
	PodName    string    `json:"podName"`
	NetnsPath  string    `json:"netnsPath"`
	EnrolledAt time.Time `json:"enrolledAt"`
}

// CleanupStateStore provides persistent tracking of enrolled pods for crash recovery.
// It stores the state in a JSON file that survives agent restarts but is cleared on
// node reboot (since it's in /var/run).
type CleanupStateStore struct {
	statePath string
	enrolled  map[string]EnrolledPodState // keyed by pod UID
	mu        sync.RWMutex
}

// NewCleanupStateStore creates a new CleanupStateStore with the given file path.
func NewCleanupStateStore(statePath string) *CleanupStateStore {
	return &CleanupStateStore{
		statePath: statePath,
		enrolled:  make(map[string]EnrolledPodState),
	}
}

// Load reads the persisted state from disk. If the file doesn't exist,
// it initializes an empty state.
func (s *CleanupStateStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// File doesn't exist, start with empty state
			s.enrolled = make(map[string]EnrolledPodState)
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	var states []EnrolledPodState
	if err := json.Unmarshal(data, &states); err != nil {
		// If we can't parse the file, start fresh
		s.enrolled = make(map[string]EnrolledPodState)
		return fmt.Errorf("failed to parse state file (starting fresh): %w", err)
	}

	s.enrolled = make(map[string]EnrolledPodState, len(states))
	for _, state := range states {
		s.enrolled[state.PodUID] = state
	}

	return nil
}

// Save writes the current state to disk atomically.
func (s *CleanupStateStore) Save() error {
	s.mu.RLock()
	states := make([]EnrolledPodState, 0, len(s.enrolled))
	for _, state := range s.enrolled {
		states = append(states, state)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Ensure the directory exists
	dir := filepath.Dir(s.statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Write to a temporary file first, then rename for atomicity
	tempPath := s.statePath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp state file: %w", err)
	}

	if err := os.Rename(tempPath, s.statePath); err != nil {
		os.Remove(tempPath) // Clean up temp file on failure
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}

// MarkEnrolled records that a pod has been enrolled with ztunnel.
func (s *CleanupStateStore) MarkEnrolled(state EnrolledPodState) error {
	s.mu.Lock()
	s.enrolled[state.PodUID] = state
	s.mu.Unlock()

	return s.Save()
}

// MarkDisenrolled removes a pod from the enrolled state.
func (s *CleanupStateStore) MarkDisenrolled(podUID string) error {
	s.mu.Lock()
	delete(s.enrolled, podUID)
	s.mu.Unlock()

	return s.Save()
}

// GetAll returns a copy of all enrolled pod states.
func (s *CleanupStateStore) GetAll() []EnrolledPodState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := make([]EnrolledPodState, 0, len(s.enrolled))
	for _, state := range s.enrolled {
		states = append(states, state)
	}
	return states
}

// Get returns the state for a specific pod UID, if it exists.
func (s *CleanupStateStore) Get(podUID string) (EnrolledPodState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state, ok := s.enrolled[podUID]
	return state, ok
}

// Count returns the number of enrolled pods.
func (s *CleanupStateStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.enrolled)
}

// Exists returns true if the state file exists on disk.
func (s *CleanupStateStore) Exists() bool {
	_, err := os.Stat(s.statePath)
	return err == nil
}

// Delete removes the state file from disk.
func (s *CleanupStateStore) Delete() error {
	s.mu.Lock()
	s.enrolled = make(map[string]EnrolledPodState)
	s.mu.Unlock()

	err := os.Remove(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Clear removes all entries from the in-memory state and saves to disk.
func (s *CleanupStateStore) Clear() error {
	s.mu.Lock()
	s.enrolled = make(map[string]EnrolledPodState)
	s.mu.Unlock()

	return s.Save()
}
