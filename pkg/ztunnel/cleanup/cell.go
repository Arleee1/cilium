// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cleanup

import (
	"log/slog"

	"github.com/cilium/hive/cell"

	"github.com/cilium/cilium/pkg/ztunnel/config"
)

// Cell provides the cleanup controller for ztunnel iptables rules.
var Cell = cell.Module(
	"ztunnel-cleanup",
	"Cleanup controller for ztunnel iptables rules",

	cell.Provide(newCleanupController),
)

type cleanupParams struct {
	cell.In

	Lifecycle cell.Lifecycle
	Logger    *slog.Logger
	Config    config.Config
}

// CleanupControllerOut provides the CleanupController for other cells to use.
type CleanupControllerOut struct {
	cell.Out

	CleanupController *CleanupController
}

func newCleanupController(p cleanupParams) CleanupControllerOut {
	// Always create the controller, but it will check EnableZTunnel internally.
	// This allows it to handle the "feature disabled but state exists" case.
	controller := NewCleanupController(
		p.Logger,
		p.Config,
	)

	p.Lifecycle.Append(controller)

	return CleanupControllerOut{
		CleanupController: controller,
	}
}
