// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiextensionstyped "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	ztunnelconfig "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	"github.com/cilium/cilium/operator/pkg/ztunnel/spire"
	bgpconfig "github.com/cilium/cilium/pkg/bgp/config"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/k8s/apis"
	"github.com/cilium/cilium/pkg/k8s/client"
	clienttest "github.com/cilium/cilium/pkg/k8s/client/testutils"
)

type haLeaderClientset struct {
	client.Clientset
	config     *rest.Config
	extensions *apiextensionsfake.Clientset
}

func (c haLeaderClientset) RestConfig() *rest.Config { return c.config }
func (c haLeaderClientset) ApiextensionsV1() apiextensionstyped.ApiextensionsV1Interface {
	return c.extensions.ApiextensionsV1()
}

func TestSpireHAStartsOnlyAsLeaderWithCiliumCRDCreationSkipped(t *testing.T) {
	log := hivetest.Logger(t)
	extensions := apiextensionsfake.NewSimpleClientset()
	extensions.PrependReactor("create", "customresourcedefinitions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		crd := action.(k8stesting.CreateAction).GetObject().(*apiextensionsv1.CustomResourceDefinition)
		require.Equal(t, "clusterstaticentries.spire.spiffe.io", crd.Name)
		crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
		}
		return false, nil, nil
	})
	alias := "spiffe://spiffe.cilium/ns/cilium-spire/sa/spire-agent"
	entries := []map[string]any{
		{"name": "agent-alias", "spec": map[string]any{
			"spiffeID": alias, "parentID": "spiffe://spiffe.cilium/spire/server",
			"selectors": []string{"k8s_psat:agent_ns:cilium-spire", "k8s_psat:agent_sa:spire-agent"},
		}},
		{"name": "ztunnel-bootstrap", "spec": map[string]any{
			"spiffeID": "spiffe://spiffe.cilium/ztunnel", "parentID": alias,
			"selectors": []string{"k8s:ns:kube-system", "k8s:sa:ztunnel-cilium"},
		}},
	}
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("matching bootstraps must not be changed: %s", r.Method)
			http.Error(w, "unexpected write", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/apis/spire.spiffe.io/v1alpha1/clusterstaticentries/")
		for _, entry := range entries {
			if entry["name"] == name {
				w.Header().Set("Content-Type", "application/json")
				err := json.NewEncoder(w).Encode(map[string]any{
					"apiVersion": "spire.spiffe.io/v1alpha1", "kind": "ClusterStaticEntry",
					"metadata": map[string]any{"name": name, "labels": map[string]any{
						"app.kubernetes.io/managed-by": "cilium-operator-spire-bootstrap",
					}},
					"spec": entry["spec"],
				})
				if err != nil {
					t.Errorf("encode bootstrap: %v", err)
				}
				return
			}
		}
		http.Error(w, "unexpected bootstrap", http.StatusNotFound)
	}))
	defer server.Close()
	_, fake := clienttest.NewFakeClientset(log)
	var leader *LeaderLifecycle
	var registration spire.SpireClient
	h := hive.New(
		cell.Provide(func() client.Clientset {
			return haLeaderClientset{Clientset: fake, config: &rest.Config{Host: server.URL}, extensions: extensions}
		}),
		cell.Config(bgpconfig.DefaultConfig),
		cell.Config(ztunnelconfig.DefaultConfig),
		WithLeaderLifecycle(
			apis.RegisterCRDsCell,
			spire.Cell,
			cell.Invoke(func(lc *LeaderLifecycle, c spire.SpireClient) { leader, registration = lc, c }),
		),
	)
	h.Viper().Set("skip-crd-creation", true)
	h.Viper().Set("enable-ztunnel", true)
	h.Viper().Set("ztunnel-ca-type", "spire")
	h.Viper().Set("mesh-auth-spire-ha", true)
	h.Viper().Set("mesh-auth-spire-bootstrap-entries", string(data))
	require.NoError(t, h.Start(log, t.Context()))
	require.Empty(t, extensions.Actions(), "followers must not install any CRDs")
	require.Zero(t, requests.Load(), "followers must not query or create bootstrap intent")
	select {
	case <-registration.Initialized():
		t.Fatal("follower must not release workload reconciliation")
	default:
	}
	require.NoError(t, leader.Start(log, t.Context()))
	<-registration.Initialized()
	require.EqualValues(t, 2, requests.Load())
	for _, action := range extensions.Actions() {
		switch action := action.(type) {
		case k8stesting.GetAction:
			require.Equal(t, "clusterstaticentries.spire.spiffe.io", action.GetName())
		case k8stesting.CreateAction:
			require.Equal(t, "clusterstaticentries.spire.spiffe.io", action.GetObject().(*apiextensionsv1.CustomResourceDefinition).Name)
		default:
			t.Fatalf("unexpected CRD mutation: %v", action)
		}
	}
	require.NoError(t, leader.Stop(log, t.Context()))
	require.NoError(t, h.Stop(log, t.Context()))
}
