// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package config

import (
	"fmt"

	"github.com/spf13/pflag"
)

const (
	DefaultCleanupStatePath = "/var/run/cilium/ztunnel-enrolled.json"
)

var DefaultConfig = Config{
	EnableZTunnel:                  false,
	EndpointEventChannelBufferSize: 1,
	CleanupStatePath:               DefaultCleanupStatePath,
}

// Config is a shared config for all ZTunnel module's cells.
// Note: The operator reads EnableZTunnel directly from the ConfigMap,
// while the agent uses this Config struct for dependency injection.
type Config struct {
	EnableZTunnel                  bool   `mapstructure:"enable-ztunnel"`
	EndpointEventChannelBufferSize int    `mapstructure:"ztunnel-endpoint-event-channel-buffer-size"`
	CleanupStatePath               string `mapstructure:"ztunnel-cleanup-state-path"`
}

func (c Config) Flags(flags *pflag.FlagSet) {
	flags.Bool("enable-ztunnel", false, "Use zTunnel as Cilium's encryption infrastructure")
	flags.Int("ztunnel-endpoint-event-channel-buffer-size", 1, "Buffer size for the ztunnel endpoint event channel")
	flags.MarkHidden("ztunnel-endpoint-event-channel-buffer-size")
	flags.String("ztunnel-cleanup-state-path", DefaultCleanupStatePath, "Path to store ztunnel enrolled pod state for cleanup on feature disable")
}

func (c Config) Validate() error {
	if c.EndpointEventChannelBufferSize < 0 {
		return fmt.Errorf("ztunnel-endpoint-event-channel-buffer-size must be non-negative, got %d", c.EndpointEventChannelBufferSize)
	}
	return nil
}
