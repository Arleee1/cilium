// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cleanup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupStateStore_LoadSave(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Test initial load with no file
	err := store.Load()
	require.NoError(t, err)
	require.Empty(t, store.GetAll())

	// Add some state
	state1 := EnrolledPodState{
		PodUID:     "uid-1",
		Namespace:  "default",
		PodName:    "pod-1",
		NetnsPath:  "/var/run/netns/pod1",
		EnrolledAt: time.Now(),
	}
	state2 := EnrolledPodState{
		PodUID:     "uid-2",
		Namespace:  "kube-system",
		PodName:    "pod-2",
		NetnsPath:  "/var/run/netns/pod2",
		EnrolledAt: time.Now(),
	}

	err = store.MarkEnrolled(state1)
	require.NoError(t, err)
	err = store.MarkEnrolled(state2)
	require.NoError(t, err)

	// Verify state was persisted
	require.Equal(t, 2, store.Count())

	// Create new store and load
	store2 := NewCleanupStateStore(statePath)
	err = store2.Load()
	require.NoError(t, err)
	require.Equal(t, 2, store2.Count())

	// Verify specific state
	loaded, ok := store2.Get("uid-1")
	require.True(t, ok)
	require.Equal(t, "default", loaded.Namespace)
	require.Equal(t, "pod-1", loaded.PodName)
}

func TestCleanupStateStore_MarkDisenrolled(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Add state
	state := EnrolledPodState{
		PodUID:     "uid-1",
		Namespace:  "default",
		PodName:    "pod-1",
		NetnsPath:  "/var/run/netns/pod1",
		EnrolledAt: time.Now(),
	}
	err := store.MarkEnrolled(state)
	require.NoError(t, err)
	require.Equal(t, 1, store.Count())

	// Remove state
	err = store.MarkDisenrolled("uid-1")
	require.NoError(t, err)
	require.Equal(t, 0, store.Count())

	// Verify removal persisted
	store2 := NewCleanupStateStore(statePath)
	err = store2.Load()
	require.NoError(t, err)
	require.Equal(t, 0, store2.Count())
}

func TestCleanupStateStore_Delete(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Add state
	state := EnrolledPodState{
		PodUID:     "uid-1",
		Namespace:  "default",
		PodName:    "pod-1",
		NetnsPath:  "/var/run/netns/pod1",
		EnrolledAt: time.Now(),
	}
	err := store.MarkEnrolled(state)
	require.NoError(t, err)

	// Verify file exists
	require.True(t, store.Exists())

	// Delete
	err = store.Delete()
	require.NoError(t, err)
	require.False(t, store.Exists())
	require.Equal(t, 0, store.Count())

	// Delete again should not error
	err = store.Delete()
	require.NoError(t, err)
}

func TestCleanupStateStore_Clear(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Add state
	state := EnrolledPodState{
		PodUID:     "uid-1",
		Namespace:  "default",
		PodName:    "pod-1",
		NetnsPath:  "/var/run/netns/pod1",
		EnrolledAt: time.Now(),
	}
	err := store.MarkEnrolled(state)
	require.NoError(t, err)

	// Clear
	err = store.Clear()
	require.NoError(t, err)
	require.Equal(t, 0, store.Count())

	// File should still exist but be empty
	require.True(t, store.Exists())

	// Load from new store
	store2 := NewCleanupStateStore(statePath)
	err = store2.Load()
	require.NoError(t, err)
	require.Equal(t, 0, store2.Count())
}

func TestCleanupStateStore_Atomicity(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Add initial state
	state := EnrolledPodState{
		PodUID:     "uid-1",
		Namespace:  "default",
		PodName:    "pod-1",
		NetnsPath:  "/var/run/netns/pod1",
		EnrolledAt: time.Now(),
	}
	err := store.MarkEnrolled(state)
	require.NoError(t, err)

	// Verify no temp file is left behind
	files, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	require.Len(t, files, 1) // Only the state file should exist
	require.Equal(t, "state.json", files[0].Name())
}

func TestCleanupStateStore_CorruptFile(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	// Write corrupt data
	err := os.WriteFile(statePath, []byte("not json"), 0644)
	require.NoError(t, err)

	store := NewCleanupStateStore(statePath)
	err = store.Load()
	// Should not return a fatal error, but log a warning and start fresh
	require.Error(t, err) // Error is returned for caller to log
	require.Equal(t, 0, store.Count())
}

func TestCleanupStateStore_GetAll(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Add multiple states
	for i := 0; i < 5; i++ {
		state := EnrolledPodState{
			PodUID:     string(rune('a' + i)),
			Namespace:  "default",
			PodName:    string(rune('a' + i)),
			NetnsPath:  "/var/run/netns/" + string(rune('a'+i)),
			EnrolledAt: time.Now(),
		}
		err := store.MarkEnrolled(state)
		require.NoError(t, err)
	}

	all := store.GetAll()
	require.Len(t, all, 5)
}
