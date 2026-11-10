// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kubernetes_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func spireBootstrapEntries(t *testing.T, rendered map[string]string) map[string]*unstructured.Unstructured {
	t.Helper()
	operator := spireManifest[appsv1.Deployment](t, rendered, "cilium/templates/cilium-operator/deployment.yaml")
	for _, arg := range spireContainer(t, operator.Spec.Template.Spec, "cilium-operator").Args {
		if value, ok := strings.CutPrefix(arg, "--mesh-auth-spire-bootstrap-entries="); ok {
			var entries []struct {
				Name string         `json:"name"`
				Spec map[string]any `json:"spec"`
			}
			require.NoError(t, json.Unmarshal([]byte(value), &entries))
			result := map[string]*unstructured.Unstructured{}
			for _, entry := range entries {
				require.NotContains(t, result, entry.Name)
				result[entry.Name] = &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "spire.spiffe.io/v1alpha1", "kind": "ClusterStaticEntry",
					"metadata": map[string]any{"name": entry.Name}, "spec": entry.Spec,
				}}
			}
			return result
		}
	}
	t.Fatal("operator bootstrap configuration missing")
	return nil
}

func TestSpireHABootstrapWiring(t *testing.T) {
	for _, class := range []string{"", "cilium-spire"} {
		for _, managed := range []bool{false, true} {
			t.Run(fmt.Sprintf("class=%s/managed=%t", class, managed), func(t *testing.T) {
				rendered, err := renderZtunnelSpire(t, ztunnelSpireValues, ztunnelSpireHA,
					"encryption.ztunnel.spire.className="+class,
					fmt.Sprintf("authentication.mutual.spire.install.enabled=%t", managed),
					"authentication.mutual.spire.trustDomain=workload.test,authentication.mutual.spire.install.namespace=identity-system,authentication.mutual.spire.install.agent.serviceAccount.name=agent-custom",
					"authentication.mutual.spire.install.server.controllerManager.entryIDPrefix=retained-prefix")
				require.NoError(t, err)
				alias := "spiffe://workload.test/ns/identity-system/sa/agent-custom"
				entries := spireBootstrapEntries(t, rendered)
				require.Len(t, entries, 2)
				expected := map[string]map[string]any{
					"cilium-spire-kube-system-cilium-agent": {
						"spiffeID": alias, "parentID": "spiffe://workload.test/spire/server",
						"selectors": []any{"k8s_psat:cluster:default", "k8s_psat:agent_ns:identity-system", "k8s_psat:agent_sa:agent-custom"},
					},
					"cilium-spire-kube-system-cilium-ztunnel": {
						"spiffeID": "spiffe://workload.test/ztunnel", "parentID": alias,
						"selectors": []any{"k8s:ns:kube-system", "k8s:sa:ztunnel-cilium"},
					},
				}
				for name, spec := range expected {
					if class != "" {
						spec["className"] = class
					}
					require.Equal(t, spec, entries[name].Object["spec"])
				}
				operator := spireManifest[appsv1.Deployment](t, rendered, "cilium/templates/cilium-operator/deployment.yaml")
				args := spireContainer(t, operator.Spec.Template.Spec, "cilium-operator").Args
				require.Contains(t, args, "--mesh-auth-spire-ha=true")
				require.Contains(t, args, "--mesh-auth-spire-crd-parent-id="+alias)
				require.Empty(t, rendered[spireServerPath+"clusterstaticentries.yaml"])
				require.Empty(t, rendered[spireCRDPath])
				if !managed {
					require.Empty(t, strings.TrimSpace(rendered[spireServerPath+"controller-manager-configmap.yaml"]))
					return
				}
				cm := spireManifest[corev1.ConfigMap](t, rendered, spireServerPath+"controller-manager-configmap.yaml")
				var cfg map[string]any
				require.NoError(t, yaml.UnmarshalStrict([]byte(cm.Data["controller-manager.yaml"]), &cfg))
				require.Equal(t, class == "", cfg["watchClassless"])
				require.Equal(t, "retained-prefix", cfg["entryIDPrefix"])
				require.NotContains(t, cfg, "entryIDPrefixCleanup")
				require.Equal(t, map[string]any{
					"clusterStaticEntries": true, "clusterSPIFFEIDs": false, "clusterFederatedTrustDomains": false,
				}, cfg["reconcile"])
			})
		}
	}
	rendered, err := renderZtunnelSpire(t, managedSpireCRD,
		"encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/ns/cilium-spire/sa/spire-agent")
	require.NoError(t, err)
	require.Len(t, spireBootstrapEntries(t, rendered), 2)
}

func TestSpireUpgradeRegistrationScope(t *testing.T) {
	for _, tt := range []struct {
		name, previousClass, class, previousPrefix, prefix string
		wantError                                          bool
	}{
		{"classless unchanged", "", "", "cilium", "cilium", false},
		{"named unchanged", "cilium-spire", "cilium-spire", "retained", "retained", false},
		{"classless to named", "", "cilium-spire", "cilium", "cilium", true},
		{"named to classless", "cilium-spire", "", "cilium", "cilium", true},
		{"named to another class", "first", "second", "cilium", "cilium", true},
		{"prefix change", "", "", "retained", "cilium", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(apiVersion, kind, namespace, name string) (map[string]any, error) {
				if apiVersion != "v1" || kind != "ConfigMap" {
					return map[string]any{}, nil
				}
				require.Equal(t, "cilium-spire", namespace)
				require.Equal(t, "spire-controller-manager", name)
				return map[string]any{"data": map[string]any{
					"controller-manager.yaml": fmt.Sprintf("className: %q\nentryIDPrefix: %q\n", tt.previousClass, tt.previousPrefix),
				}}, nil
			}
			_, err := renderSpireChart(t, spireRenderOptions{upgrade: true, hasCRD: true, lookup: lookup},
				managedSpireCRD, "encryption.ztunnel.spire.className="+tt.class,
				"authentication.mutual.spire.install.server.controllerManager.entryIDPrefix="+tt.prefix)
			if tt.wantError {
				require.ErrorContains(t, err, "requires a planned registration migration")
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, config := range []string{"", "not: [valid", "className: cilium-spire"} {
		_, err := renderSpireChart(t, spireRenderOptions{
			upgrade: true, hasCRD: true,
			lookup: func(apiVersion, kind, namespace, name string) (map[string]any, error) {
				if kind == "ConfigMap" {
					return map[string]any{"data": map[string]any{"controller-manager.yaml": config}}, nil
				}
				return map[string]any{}, nil
			},
		}, managedSpireCRD)
		require.ErrorContains(t, err, "Cannot read existing chart-managed SCM configuration")
	}
	_, err := renderSpireChart(t, spireRenderOptions{
		upgrade: true, hasCRD: true,
		lookup: func(apiVersion, kind, namespace, name string) (map[string]any, error) {
			if kind == "ConfigMap" {
				return nil, errors.New("cannot read existing SCM ConfigMap")
			}
			return map[string]any{}, nil
		},
	}, managedSpireCRD)
	require.ErrorContains(t, err, "cannot read existing SCM ConfigMap")
}

func TestSpireHARejectsHelmOwnedBootstrapUpgrade(t *testing.T) {
	for _, owner := range []string{"cilium", "another-release", ""} {
		_, err := renderSpireChart(t, spireRenderOptions{
			upgrade: true, hasCRD: true,
			lookup: func(apiVersion, kind, namespace, name string) (map[string]any, error) {
				if kind == "ClusterStaticEntry" {
					return map[string]any{"items": []any{map[string]any{"metadata": map[string]any{
						"annotations": map[string]any{
							"meta.helm.sh/release-name": owner, "meta.helm.sh/release-namespace": "kube-system",
							"helm.sh/resource-policy": "keep",
						},
					}}}}, nil
				}
				return map[string]any{}, nil
			},
		}, managedSpireCRD)
		if owner == "cilium" {
			require.ErrorContains(t, err, "staged retention and operator ownership handoff")
		} else {
			require.NoError(t, err)
		}
	}
}

func TestSpireHALegacyAndStructuredValidation(t *testing.T) {
	for _, tt := range []struct {
		name, values, want string
	}{
		{"old enable", "encryption.ztunnel.spire.useCRD=true", "Remove legacy"},
		{"old disabled", "encryption.ztunnel.spire.useCRD=false", "Remove legacy"},
		{"old profile", "encryption.ztunnel.spire.bootstrapProfile=ztunnel", "Remove legacy"},
		{"old legacy profile", "encryption.ztunnel.spire.bootstrapProfile=legacy", "Remove legacy"},
		{"old replica count", "authentication.mutual.spire.install.server.replicas=2", "Remove legacy"},
		{"old CRD switch", "authentication.mutual.spire.install.crds.enabled=false", "Remove legacy"},
		{"old CRD empty map", "authentication.mutual.spire.install.crds=null", "Remove legacy"},
		{"Entry API parent", "authentication.mutual.spire.ha.enabled=false,encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/ztunnel", "require authentication.mutual.spire.ha.enabled=true"},
		{"Entry API class", "authentication.mutual.spire.ha.enabled=false,encryption.ztunnel.spire.className=custom", "require authentication.mutual.spire.ha.enabled=true"},
		{"boolean parent", "encryption.ztunnel.spire.parentID=false", "parentID"},
		{"numeric parent", "encryption.ztunnel.spire.parentID=123", "parentID"},
		{"boolean class", "encryption.ztunnel.spire.className=false", "className"},
		{"blank class", "encryption.ztunnel.spire.className= ", "className"},
		{"split class", "encryption.ztunnel.spire.className=two classes", "className"},
		{"foreign parent", "encryption.ztunnel.spire.parentID=spiffe://other.test/ztunnel", "configured trust domain"},
		{"root parent", "encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/", "nonempty path"},
		{"relative parent", "encryption.ztunnel.spire.parentID=/ztunnel", "SPIFFE ID"},
		{"dot parent", "encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/../ztunnel", "SPIFFE ID"},
		{"query parent", "encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/ztunnel?query", "SPIFFE ID"},
		{"wrong parent", "encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/ztunnel", "requires the SPIRE agent alias"},
		{"empty prefix", "authentication.mutual.spire.install.server.controllerManager.entryIDPrefix=", "entryIDPrefix"},
		{"blank prefix", "authentication.mutual.spire.install.server.controllerManager.entryIDPrefix= ", "entryIDPrefix"},
		{"Set mode", "encryption.ztunnel.spire.useSPIFFEIDSet=true", "Remove obsolete"},
		{"stale disabled Set", "encryption.ztunnel.spire.useSPIFFEIDSet=false", "Remove obsolete"},
		{"Set template", "encryption.ztunnel.spire.spiffeIDSet.spec.parentIDTemplate=stale", "Remove obsolete"},
		{"old SA mode", "encryption.ztunnel.spire.useServiceAccountSPIFFEID=false", "Remove obsolete"},
		{"Pod selectors", "encryption.ztunnel.spire.workloadSelectorTemplates=null", "Remove obsolete"},
		{"Set flag", "operator.extraArgs[0]=--enable-ztunnel-spiffeid-set-crd=true", "Remove obsolete"},
		{"old bootstrap flag", "operator.extraArgs[0]=--mesh-auth-spire-registration-class=cilium-spire", "Remove obsolete"},
		{"raw class", "operator.extraArgs[0]=--mesh-auth-spire-crd-class=other", "Use encryption.ztunnel.spire.parentID and className"},
		{"raw parent", "operator.extraArgs[0]=--mesh-auth-spire-crd-parent-id=spiffe://spiffe.cilium/other", "Use encryption.ztunnel.spire.parentID and className"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, skipSchema := range []bool{false, true} {
				_, err := renderSpireChart(t, spireRenderOptions{skipSchema: skipSchema}, managedSpireCRD, tt.values)
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

func TestSpireHAReplicasValidation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, value := range []any{nil, "2", false, 0, -1, 1.5, 2147483648, []any{2}, map[string]any{}} {
			for _, skipSchema := range []bool{false, true} {
				t.Run(fmt.Sprintf("enabled=%t/value=%v/schema=%t", enabled, value, !skipSchema), func(t *testing.T) {
					values := map[string]any{"authentication": map[string]any{"mutual": map[string]any{"spire": map[string]any{
						"ha": map[string]any{"enabled": enabled, "replicas": value},
					}}}}
					_, err := renderSpireChart(t, spireRenderOptions{values: values, skipSchema: skipSchema},
						ztunnelSpireValues, "authentication.mutual.spire.install.enabled=true")
					require.Error(t, err)
					require.Contains(t, err.Error(), "replicas")
				})
			}
		}
	}
	for _, count := range []int{1, 2, 3, 2147483647} {
		for _, skipSchema := range []bool{false, true} {
			_, err := renderSpireChart(t, spireRenderOptions{skipSchema: skipSchema},
				"authentication.mutual.spire.ha.enabled=false", fmt.Sprintf("authentication.mutual.spire.ha.replicas=%d", count))
			require.Error(t, err, "explicit counts must fail even with SPIRE itself disabled")
			rendered, err := renderSpireChart(t, spireRenderOptions{skipSchema: skipSchema}, managedSpireCRD,
				fmt.Sprintf("authentication.mutual.spire.ha.replicas=%d", count))
			require.NoError(t, err)
			sts := spireManifest[appsv1.StatefulSet](t, rendered, spireServerPath+"statefulset.yaml")
			require.EqualValues(t, count, *sts.Spec.Replicas)
		}
	}
	_, err := renderZtunnelSpire(t, ztunnelSpireValues, ztunnelSpireHA, "authentication.mutual.spire.ha.replicas=2")
	require.ErrorContains(t, err, "external SPIRE replicas cannot be configured")
}
