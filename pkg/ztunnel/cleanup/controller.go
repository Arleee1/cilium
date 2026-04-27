// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/cilium/hive/cell"

	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/netns"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/ztunnel/config"
	"github.com/cilium/cilium/pkg/ztunnel/iptables"
)

// CleanupController handles cleanup of ztunnel iptables rules when the feature
// is disabled after previously being enabled.
type CleanupController struct {
	logger     *slog.Logger
	stateStore *CleanupStateStore
	config     config.Config
}

// NewCleanupController creates a new CleanupController.
func NewCleanupController(
	logger *slog.Logger,
	cfg config.Config,
) *CleanupController {
	stateStore := NewCleanupStateStore(cfg.CleanupStatePath)
	return &CleanupController{
		logger:     logger,
		stateStore: stateStore,
		config:     cfg,
	}
}

// Start implements cell.HookInterface and runs the cleanup controller startup logic.
func (c *CleanupController) Start(ctx cell.HookContext) error {
	c.logger.Info("Starting ztunnel cleanup controller")

	// Check if ztunnel feature is disabled but state file exists.
	// This indicates the feature was enabled before and we need to clean up.
	if !c.config.EnableZTunnel && c.stateStore.Exists() {
		c.logger.Info("Ztunnel feature disabled but state file exists, running cleanup")
		if err := c.CleanupOnFeatureDisable(ctx); err != nil {
			c.logger.Error("Failed to cleanup on feature disable", logfields.Error, err)
		}
		return nil
	}

	if !c.config.EnableZTunnel {
		c.logger.Debug("Ztunnel not enabled, cleanup controller not starting")
		return nil
	}

	// Load existing state (for crash recovery tracking)
	if err := c.stateStore.Load(); err != nil {
		c.logger.Warn("Failed to load cleanup state, starting fresh", logfields.Error, err)
	}

	c.logger.Info("Ztunnel cleanup controller started")
	return nil
}

// Stop implements cell.HookInterface.
func (c *CleanupController) Stop(ctx cell.HookContext) error {
	c.logger.Info("Stopping ztunnel cleanup controller")
	return nil
}

// CleanupOnFeatureDisable cleans up all ztunnel rules when the feature is disabled.
func (c *CleanupController) CleanupOnFeatureDisable(ctx context.Context) error {
	c.logger.Info("Running cleanup for ztunnel feature disable")

	// Load persisted state
	if err := c.stateStore.Load(); err != nil {
		c.logger.Warn("Failed to load state for cleanup", logfields.Error, err)
	}

	var errs []error
	states := c.stateStore.GetAll()

	// Clean up all enrolled pods
	for _, state := range states {
		if ctx.Err() != nil {
			break
		}

		c.logger.Info("Cleaning up pod for feature disable",
			"podUID", state.PodUID,
			"namespace", state.Namespace,
			"podName", state.PodName,
		)

		if err := c.cleanupPod(state); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				c.logger.Warn("Failed to cleanup pod",
					"podUID", state.PodUID,
					logfields.Error, err,
				)
				errs = append(errs, err)
			}
		}
	}

	// Delete the state file
	if err := c.stateStore.Delete(); err != nil {
		c.logger.Warn("Failed to delete state file", logfields.Error, err)
		errs = append(errs, err)
	}

	c.logger.Info("Feature disable cleanup completed",
		"cleanedPods", len(states),
		"errors", len(errs),
	)

	return errors.Join(errs...)
}

// GetStateStore returns the cleanup state store for integration with other components.
func (c *CleanupController) GetStateStore() *CleanupStateStore {
	return c.stateStore
}

// cleanupPod removes ztunnel rules from a specific pod's network namespace.
func (c *CleanupController) cleanupPod(state EnrolledPodState) error {
	ns, err := netns.OpenPinned(state.NetnsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Netns already gone, nothing to clean up
			c.logger.Info("Pod netns already cleaned up",
				"podUID", state.PodUID,
				"netnsPath", state.NetnsPath,
			)
			return nil
		}
		return fmt.Errorf("failed to open netns: %w", err)
	}
	defer ns.Close()

	err = ns.Do(func() error {
		return iptables.CleanupWithContinue(c.logger, option.Config.EnableIPv4, option.Config.EnableIPv6)
	})

	if err != nil {
		return fmt.Errorf("failed to cleanup iptables rules: %w", err)
	}

	c.logger.Info("Successfully cleaned up pod",
		"podUID", state.PodUID,
		"namespace", state.Namespace,
		"podName", state.PodName,
	)

	return nil
}

var _ cell.HookInterface = &CleanupController{}
