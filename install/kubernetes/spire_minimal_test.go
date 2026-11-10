// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kubernetes_test

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func TestSpireBootstrapProfiles(t *testing.T) {
	for _, profile := range []string{"legacy", "ztunnel"} {
		for _, class := range []string{"", "cilium-spire"} {
			for _, managed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/class=%s/managed=%t", profile, class, managed), func(t *testing.T) {
					values := []string{
						ztunnelSpireValues, ztunnelSpireCRD,
						"encryption.ztunnel.spire.bootstrapProfile=" + profile,
						"encryption.ztunnel.spire.className=" + class,
						fmt.Sprintf("authentication.mutual.spire.install.enabled=%t", managed),
						"authentication.mutual.spire.trustDomain=workload.test,authentication.mutual.spire.install.namespace=identity-system,authentication.mutual.spire.install.agent.serviceAccount.name=agent-custom",
						"authentication.mutual.spire.install.server.controllerManager.entryIDPrefix=retained-prefix",
					}
					rendered, err := renderZtunnelSpire(t, values...)
					require.NoError(t, err)
					minimal := profile == "ztunnel"
					alias := "spiffe://workload.test/ns/identity-system/sa/agent-custom"
					operator := spireManifest[appsv1.Deployment](t, rendered, "cilium/templates/cilium-operator/deployment.yaml")
					container := spireContainer(t, operator.Spec.Template.Spec, "cilium-operator")
					args := []string{"--config-dir=/tmp/cilium/config-map", "--debug=$(CILIUM_DEBUG)", ztunnelSpireFlag + "=true"}
					if minimal {
						args = append(args, "--mesh-auth-spire-crd-parent-id="+alias)
					}
					if class != "" {
						args = append(args, "--mesh-auth-spire-crd-class="+class)
					}
					require.Equal(t, args, container.Args)
					for _, pod := range []corev1.PodSpec{
						operator.Spec.Template.Spec,
						spireManifest[appsv1.DaemonSet](t, rendered, "cilium/templates/cilium-agent/daemonset.yaml").Spec.Template.Spec,
					} {
						require.True(t, slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
							return v.Name == "spire-agent-socket"
						}))
						for _, c := range pod.Containers {
							require.True(t, slices.ContainsFunc(c.VolumeMounts, func(v corev1.VolumeMount) bool {
								return v.Name == "spire-agent-socket"
							}))
						}
					}
					if !managed {
						require.Empty(t, strings.TrimSpace(rendered[spireServerPath+"clusterstaticentries.yaml"]))
						require.Empty(t, strings.TrimSpace(rendered[spireServerPath+"controller-manager-configmap.yaml"]))
						return
					}
					entries := map[string]map[string]any{}
					decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered[spireServerPath+"clusterstaticentries.yaml"]), 4096)
					for {
						var obj unstructured.Unstructured
						err := decoder.Decode(&obj.Object)
						if err == io.EOF {
							break
						}
						require.NoError(t, err)
						if len(obj.Object) == 0 {
							continue
						}
						require.Equal(t, "ClusterStaticEntry", obj.GetKind())
						require.Equal(t, "Helm", obj.GetLabels()["app.kubernetes.io/managed-by"])
						spec, found, err := unstructured.NestedMap(obj.Object, "spec")
						require.NoError(t, err)
						require.True(t, found)
						if class == "" {
							require.NotContains(t, spec, "className")
						} else {
							require.Equal(t, class, spec["className"])
						}
						entries[obj.GetName()] = spec
					}
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
					if !minimal {
						for _, name := range []string{"cilium-agent", "cilium-operator"} {
							account := name
							if name == "cilium-agent" {
								account = "cilium"
							}
							expected["cilium-spire-kube-system-cilium-"+name] = map[string]any{
								"spiffeID": "spiffe://workload.test/" + name, "parentID": alias,
								"selectors": []any{"k8s:ns:kube-system", "k8s:sa:" + account},
							}
						}
					}
					if class != "" {
						for _, spec := range expected {
							spec["className"] = class
						}
					}
					require.Equal(t, expected, entries)
					cm := spireManifest[corev1.ConfigMap](t, rendered, spireServerPath+"controller-manager-configmap.yaml")
					var cfg map[string]any
					require.NoError(t, yaml.UnmarshalStrict([]byte(cm.Data["controller-manager.yaml"]), &cfg))
					require.Equal(t, class == "", cfg["watchClassless"])
					if class != "" {
						require.Equal(t, class, cfg["className"])
					}
					require.Equal(t, "retained-prefix", cfg["entryIDPrefix"])
					require.NotContains(t, cfg, "entryIDPrefixCleanup")
					require.Equal(t, map[string]any{
						"clusterStaticEntries": true, "clusterSPIFFEIDs": false, "clusterFederatedTrustDomains": false,
					}, cfg["reconcile"])
					agent := spireManifest[corev1.ConfigMap](t, rendered, "cilium/templates/spire/agent/configmap.yaml").Data["agent.conf"]
					server := spireManifest[corev1.ConfigMap](t, rendered, spireServerPath+"configmap.yaml").Data["server.conf"]
					require.Contains(t, agent, `"spiffe://workload.test/ztunnel"`)
					require.Contains(t, agent, `"spiffe://workload.test/cilium-agent"`)
					require.Contains(t, server, `"spiffe://workload.test/cilium-operator"`)
					legacy, err := renderZtunnelSpire(t, append(values, "encryption.ztunnel.spire.bootstrapProfile=legacy")...)
					require.NoError(t, err)
					for _, path := range []string{
						"cilium/templates/cilium-operator/clusterrole.yaml",
						"cilium/templates/cilium-agent/daemonset.yaml",
						"cilium/templates/spire/agent/configmap.yaml",
						"cilium/templates/spire/agent/daemonset.yaml",
						spireServerPath + "clusterrole.yaml",
						spireServerPath + "configmap.yaml",
						spireServerPath + "statefulset.yaml",
					} {
						require.Equal(t, legacy[path], rendered[path], "bootstrap selection must not broaden RBAC or roll unrelated components")
					}
				})
			}
		}
	}
}

func TestSpireStructuredParent(t *testing.T) {
	for _, profile := range []string{"legacy", "ztunnel"} {
		const parent = "spiffe://spiffe.cilium/ns/cilium-spire/sa/spire-agent"
		rendered, err := renderZtunnelSpire(t, managedSpireCRD,
			"encryption.ztunnel.spire.bootstrapProfile="+profile,
			"encryption.ztunnel.spire.parentID="+parent)
		require.NoError(t, err)
		operator := spireManifest[appsv1.Deployment](t, rendered, "cilium/templates/cilium-operator/deployment.yaml")
		require.Contains(t, operator.Spec.Template.Spec.Containers[0].Args, "--mesh-auth-spire-crd-parent-id="+parent)
	}
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

func TestSpireProfileValidation(t *testing.T) {
	for _, tt := range []struct {
		name, values, want string
	}{
		{"Entry API minimal", "encryption.ztunnel.spire.useCRD=false,encryption.ztunnel.spire.bootstrapProfile=ztunnel", "require encryption.ztunnel.spire.useCRD=true"},
		{"Entry API parent", "encryption.ztunnel.spire.useCRD=false,encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/ztunnel", "require encryption.ztunnel.spire.useCRD=true"},
		{"Entry API class", "encryption.ztunnel.spire.useCRD=false,encryption.ztunnel.spire.className=custom", "require encryption.ztunnel.spire.useCRD=true"},
		{"unknown profile", "encryption.ztunnel.spire.bootstrapProfile=all", "bootstrapProfile"},
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
		{"wrong minimal parent", "encryption.ztunnel.spire.bootstrapProfile=ztunnel,encryption.ztunnel.spire.parentID=spiffe://spiffe.cilium/ztunnel", "requires the SPIRE agent alias"},
		{"empty prefix", "authentication.mutual.spire.install.server.controllerManager.entryIDPrefix=", "entryIDPrefix"},
		{"blank prefix", "authentication.mutual.spire.install.server.controllerManager.entryIDPrefix= ", "entryIDPrefix"},
		{"Set mode", "encryption.ztunnel.spire.useSPIFFEIDSet=true", "Remove obsolete"},
		{"stale disabled Set", "encryption.ztunnel.spire.useSPIFFEIDSet=false", "Remove obsolete"},
		{"Set template", "encryption.ztunnel.spire.spiffeIDSet.spec.parentIDTemplate=stale", "Remove obsolete"},
		{"old SA mode", "encryption.ztunnel.spire.useServiceAccountSPIFFEID=false", "Remove obsolete"},
		{"Pod selectors", "encryption.ztunnel.spire.workloadSelectorTemplates=null", "Remove obsolete"},
		{"Set flag", "operator.extraArgs[0]=--enable-ztunnel-spiffeid-set-crd=true", "Remove obsolete"},
		{"bootstrap flag", "operator.extraArgs[0]=--mesh-auth-spire-registration-class=cilium-spire", "Remove obsolete"},
		{"raw managed class", "operator.extraArgs[0]=--mesh-auth-spire-crd-class=other", "Use encryption.ztunnel.spire.parentID and className"},
		{"raw managed parent", "operator.extraArgs[0]=--mesh-auth-spire-crd-parent-id=spiffe://spiffe.cilium/other", "Use encryption.ztunnel.spire.parentID and className"},
		{"raw external minimal", "authentication.mutual.spire.install.enabled=false,encryption.ztunnel.spire.bootstrapProfile=ztunnel,operator.extraArgs[0]=--mesh-auth-spire-crd-class=other", "Use encryption.ztunnel.spire.parentID and className"},
		{"raw external duplicate", "authentication.mutual.spire.install.enabled=false,encryption.ztunnel.spire.className=class,operator.extraArgs[0]=--mesh-auth-spire-crd-class=other", "Use encryption.ztunnel.spire.parentID and className"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, skipSchema := range []bool{false, true} {
				_, err := renderSpireChart(t, spireRenderOptions{skipSchema: skipSchema}, managedSpireCRD, tt.values)
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}
