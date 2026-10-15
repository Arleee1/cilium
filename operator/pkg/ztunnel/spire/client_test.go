// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"testing"

	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	ztunnel "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	"github.com/cilium/cilium/pkg/k8s/client"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client/testutils"
)

func TestClientBackendWiring(t *testing.T) {
	for _, tt := range []struct {
		name    string
		caType  ztunnel.CAType
		ztunnel bool
		crd     bool
		wantErr bool
	}{
		{name: "disabled", caType: ztunnel.CATypeInternal},
		{name: "disabled SPIRE", caType: ztunnel.CATypeSpire},
		{name: "internal ztunnel", caType: ztunnel.CATypeInternal, ztunnel: true},
		{name: "direct SPIRE ztunnel", caType: ztunnel.CATypeSpire, ztunnel: true},
		{name: "CRD SPIRE ztunnel", caType: ztunnel.CATypeSpire, ztunnel: true, crd: true},
		{name: "CRD requires ztunnel", caType: ztunnel.CATypeSpire, crd: true, wantErr: true},
		{name: "CRD requires SPIRE CA", caType: ztunnel.CATypeInternal, ztunnel: true, crd: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			log := hivetest.Logger(t)
			_, cs := k8sClient.NewFakeClientset(log)
			var registration SpireClient
			h := hive.New(
				Cell,
				cell.Config(ztunnel.DefaultConfig),
				cell.Provide(func() client.Clientset { return cs }),
				cell.Invoke(func(c SpireClient) {
					registration = c
				}),
			)
			h.Viper().Set("ztunnel-ca-type", string(tt.caType))
			h.Viper().Set("enable-ztunnel", tt.ztunnel)
			h.Viper().Set("enable-ztunnel-spiffeid-crd", tt.crd)
			h.Viper().Set("mesh-auth-spiffe-trust-domain", "test.cilium")
			err := h.Populate(log)
			if tt.wantErr {
				require.ErrorContains(t, err, "requires enable-ztunnel and ztunnel-ca-type=spire")
				return
			}
			require.NoError(t, err)
			if !tt.ztunnel || tt.caType != ztunnel.CATypeSpire {
				require.Nil(t, registration)
				return
			}
			_, prunes := registration.(ServiceAccountPruner)
			require.Equal(t, tt.crd, prunes)
			require.NotNil(t, registration.Initialized())
			select {
			case <-registration.Initialized():
				t.Fatal("backend initialized before lifecycle startup")
			default:
			}
			var entryCfg SpireEntryConfig
			if tt.crd {
				crd, ok := registration.(*CRDClient)
				require.True(t, ok)
				require.Equal(t, "test.cilium", crd.trustDomain)
				entryCfg = crd.entryCfg
				require.NoError(t, h.Start(log, t.Context()))
				select {
				case <-registration.Initialized():
				default:
					t.Fatal("CRD backend did not initialize")
				}
				require.NoError(t, h.Stop(log, t.Context()))
			} else {
				direct, ok := registration.(*Client)
				require.True(t, ok)
				require.Same(t, cs, direct.k8sClient)
				require.Nil(t, direct.entry)
				wantCfg := defaultClientConfig
				wantCfg.SpiffeTrustDomain = "test.cilium"
				require.Equal(t, wantCfg, direct.cfg)
				entryCfg = direct.entryCfg
			}
			require.Equal(t, DefaultSpireEntryConfig().entry("test.cilium", "shop/checkout"), entryCfg.entry("test.cilium", "shop/checkout"))
		})
	}
}

func TestSpireEntryConfig(t *testing.T) {
	cfg := DefaultSpireEntryConfig()
	require.Equal(t, &types.Entry{
		SpiffeId: &types.SPIFFEID{TrustDomain: "test.cilium", Path: "/ns/shop/sa/checkout"},
		ParentId: &types.SPIFFEID{TrustDomain: "test.cilium", Path: "/ztunnel"},
		Selectors: []*types.Selector{
			{Type: "k8s", Value: "ns:shop"},
			{Type: "k8s", Value: "sa:checkout"},
		},
	}, cfg.entry("test.cilium", "shop/checkout"))
}

type clientsetWithRestConfig struct {
	client.Clientset
	config *rest.Config
}

func (c clientsetWithRestConfig) RestConfig() *rest.Config {
	return c.config
}

func TestCRDClientConstructionErrors(t *testing.T) {
	log := hivetest.Logger(t)
	fake, cs := k8sClient.NewFakeClientset(log)
	cfg := defaultClientConfig
	cfg.UseCRD = true
	p := params{
		Logger: log, Lifecycle: &cell.DefaultLifecycle{},
		ClientConfig: cfg, EntryConfig: DefaultSpireEntryConfig(),
		ZtunnelConfig: ztunnel.Config{EnableZTunnel: true, CAType: ztunnel.CATypeSpire},
	}
	registration, err := NewClient(p)
	require.ErrorContains(t, err, "requires a Kubernetes client")
	require.True(t, registration == nil, "constructor errors must return a nil interface")
	fake.Disable()
	p.K8sClient = cs
	_, err = NewClient(p)
	require.ErrorContains(t, err, "requires a Kubernetes client")
	_, cs = k8sClient.NewFakeClientset(log)
	p.K8sClient = clientsetWithRestConfig{Clientset: cs}
	_, err = NewClient(p)
	require.ErrorContains(t, err, "requires a Kubernetes client")
	p.K8sClient = clientsetWithRestConfig{Clientset: cs, config: &rest.Config{Host: "https://[invalid"}}
	_, err = NewClient(p)
	require.ErrorContains(t, err, "create Kubernetes client")
	p.ClientConfig.SpiffeTrustDomain = "invalid/domain"
	_, err = NewClient(p)
	require.ErrorContains(t, err, "invalid SPIRE CRD trust domain")
}
