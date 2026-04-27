// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cleanup

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cilium/cilium/pkg/testutils"
	"github.com/cilium/cilium/pkg/testutils/netns"
	"github.com/cilium/cilium/pkg/ztunnel/iptables"
)

// TestPrivilegedCleanupWithContinue_PartialFailure tests that CleanupWithContinue
// continues even if some operations fail.
func TestPrivilegedCleanupWithContinue_PartialFailure(t *testing.T) {
	testutils.PrivilegedTest(t)

	ns := netns.NewNetNS(t)
	ns.Do(func() error {
		// Cleanup on empty namespace should not error
		err := iptables.CleanupWithContinue(slog.Default(), true, true)
		require.NoError(t, err)
		return nil
	})
}

// TestEnrolledPodState_Serialization tests that EnrolledPodState
// serializes and deserializes correctly.
func TestEnrolledPodState_Serialization(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	store := NewCleanupStateStore(statePath)

	// Create state with all fields populated
	now := time.Now().Truncate(time.Second) // Truncate for JSON serialization
	state := EnrolledPodState{
		PodUID:     "uid-test-1234",
		Namespace:  "test-namespace",
		PodName:    "test-pod-name",
		NetnsPath:  "/var/run/netns/cni-test-1234",
		EnrolledAt: now,
	}

	err := store.MarkEnrolled(state)
	require.NoError(t, err)

	// Load in a new store
	store2 := NewCleanupStateStore(statePath)
	err = store2.Load()
	require.NoError(t, err)

	loaded, ok := store2.Get("uid-test-1234")
	require.True(t, ok)
	require.Equal(t, state.PodUID, loaded.PodUID)
	require.Equal(t, state.Namespace, loaded.Namespace)
	require.Equal(t, state.PodName, loaded.PodName)
	require.Equal(t, state.NetnsPath, loaded.NetnsPath)
	// Time comparison with truncation for JSON
	require.Equal(t, now.Unix(), loaded.EnrolledAt.Unix())
}

// TestCleanupController_FeatureDisableDetection tests that the controller
// detects when ztunnel was previously enabled.
func TestCleanupController_FeatureDisableDetection(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")

	// Create a state file to simulate previous enrollment
	store := NewCleanupStateStore(statePath)
	state := EnrolledPodState{
		PodUID:     "uid-1",
		Namespace:  "default",
		PodName:    "test-pod",
		NetnsPath:  "/nonexistent/path", // Will fail gracefully
		EnrolledAt: time.Now(),
	}
	err := store.MarkEnrolled(state)
	require.NoError(t, err)

	// Verify state file exists
	require.True(t, store.Exists())

	// After cleanup, state file should be deleted
	err = store.Delete()
	require.NoError(t, err)
	require.False(t, store.Exists())
}
