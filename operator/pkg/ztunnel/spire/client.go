// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/spf13/pflag"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"

	ztunnel "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
)

const parentID = "/ztunnel"

// DefaultSpireEntryConfig returns the ztunnel SPIRE entry configuration.
func DefaultSpireEntryConfig() SpireEntryConfig {
	return SpireEntryConfig{
		ParentID:      parentID,
		PathFunc:      ztunnel.SpiffeIDPathFunc,
		SelectorsFunc: ztunnel.SpiffeIDSelectorsFunc,
	}
}

// Cell is the cell for the SPIRE client.
var Cell = cell.Module(
	"spire-client",
	"Spire Server API Client",
	cell.Config(defaultClientConfig),
	cell.Provide(DefaultSpireEntryConfig),
	cell.Provide(NewClient),
)

// ClientConfig contains the configuration for the SPIRE client.
type ClientConfig struct {
	SpireAgentSocketPath         string        `mapstructure:"mesh-auth-spire-agent-socket"`
	SpireServerAddress           string        `mapstructure:"mesh-auth-spire-server-address"`
	SpireServerConnectionTimeout time.Duration `mapstructure:"mesh-auth-spire-server-connection-timeout"`
	SpiffeTrustDomain            string        `mapstructure:"mesh-auth-spiffe-trust-domain"`
	// HAEnabled installs static registration intent for independent SPIRE stores.
	HAEnabled        bool   `mapstructure:"mesh-auth-spire-ha"`
	BootstrapEntries string `mapstructure:"mesh-auth-spire-bootstrap-entries"`
	CRDParentID      string `mapstructure:"mesh-auth-spire-crd-parent-id"`
	CRDClassName     string `mapstructure:"mesh-auth-spire-crd-class"`
}

var defaultClientConfig = ClientConfig{
	SpireAgentSocketPath:         "/run/spire/sockets/agent/agent.sock",
	SpireServerAddress:           "spire-server.spire.svc:8081",
	SpireServerConnectionTimeout: 10 * time.Second,
	SpiffeTrustDomain:            "spiffe.cilium",
}

// Flags adds the flags used by ClientConfig.
func (cfg ClientConfig) Flags(flags *pflag.FlagSet) {
	flags.String("mesh-auth-spire-agent-socket",
		cfg.SpireAgentSocketPath,
		"The path for the SPIRE admin agent Unix socket.")
	flags.String("mesh-auth-spire-server-address",
		cfg.SpireServerAddress,
		"SPIRE server endpoint.")
	flags.Duration("mesh-auth-spire-server-connection-timeout",
		cfg.SpireServerConnectionTimeout,
		"SPIRE server connection timeout.")
	flags.String("mesh-auth-spiffe-trust-domain",
		cfg.SpiffeTrustDomain,
		"The trust domain for the SPIFFE identity.")
	flags.Bool("mesh-auth-spire-ha",
		cfg.HAEnabled,
		"Install the SPIRE ClusterStaticEntry CRD and required bootstrap entries on elected-leader startup, then register enrolled ServiceAccounts using static entries (requires ztunnel with the SPIRE CA).")
	flags.String("mesh-auth-spire-bootstrap-entries", cfg.BootstrapEntries,
		"JSON array containing the two required bootstrap entries (name and spec): the SPIRE agent node alias and ztunnel. Required with mesh-auth-spire-ha.")
	flags.String("mesh-auth-spire-crd-parent-id", cfg.CRDParentID,
		"Optional workload parent SPIFFE ID; must match the configured SPIRE agent bootstrap alias (requires mesh-auth-spire-ha).")
	flags.String("mesh-auth-spire-crd-class", cfg.CRDClassName,
		"Optional controller class for operator-produced ClusterStaticEntries; empty leaves the class unset on new entries (requires the CRD backend).")
}

type params struct {
	cell.In

	Logger        *slog.Logger
	K8sClient     k8sClient.Clientset
	Lifecycle     cell.Lifecycle
	ClientConfig  ClientConfig
	EntryConfig   SpireEntryConfig
	ZtunnelConfig ztunnel.Config
}

// SpireEntryConfig contains the configuration for ztunnel SPIRE entry generation.
type SpireEntryConfig struct {
	ParentID      string
	PathFunc      func(string) string
	SelectorsFunc func(string) []*types.Selector
}

func (cfg SpireEntryConfig) entry(trustDomain, id string) *types.Entry {
	return &types.Entry{
		SpiffeId: &types.SPIFFEID{
			TrustDomain: trustDomain,
			Path:        cfg.PathFunc(id),
		},
		ParentId: &types.SPIFFEID{
			TrustDomain: trustDomain,
			Path:        cfg.ParentID,
		},
		Selectors: cfg.SelectorsFunc(id),
	}
}

// SpireClient registers individual identities with the selected backend.
// Successful mutations acknowledge backend acceptance, not convergence of all
// SPIRE servers. Initialized reports backend initialization only.
//
// List is intentionally excluded: SPIRE entry IDs and Kubernetes resource names
// are not interchangeable with logical ServiceAccount identities.
type SpireClient interface {
	Upsert(context.Context, string) error
	UpsertBatch(context.Context, []string) error
	Delete(context.Context, string) error
	DeleteBatch(context.Context, []string) error
	Initialized() <-chan struct{}
}

// ServiceAccountPruner is an optional capability for ownership-scoped cleanup.
// An empty namespace selects all owned registrations. keep must consult current,
// initialized inventories; it is called immediately before each deletion.
type ServiceAccountPruner interface {
	Prune(ctx context.Context, namespace string, keep func(id string) bool) error
}

// NewClient creates a SPIRE registration client for ztunnel namespace enrollment.
func NewClient(params params) (SpireClient, error) {
	if !params.ClientConfig.HAEnabled && (params.ClientConfig.CRDParentID != "" || params.ClientConfig.CRDClassName != "" || params.ClientConfig.BootstrapEntries != "") {
		return nil, fmt.Errorf("SPIRE bootstrap entries, CRD parent and class overrides require mesh-auth-spire-ha")
	}
	if params.ClientConfig.HAEnabled && !params.ZtunnelConfig.UseSpireCA() {
		return nil, fmt.Errorf("mesh-auth-spire-ha requires enable-ztunnel and ztunnel-ca-type=spire")
	}
	if !params.ZtunnelConfig.UseSpireCA() {
		return nil, nil
	}

	if params.ClientConfig.HAEnabled {
		client, err := newCRDClient(params)
		if err != nil {
			return nil, err
		}
		return client, nil
	}

	return newEntryClient(params), nil
}
