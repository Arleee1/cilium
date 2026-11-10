// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kubernetes_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/common"
	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartdeps "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
	"helm.sh/helm/v4/pkg/strvals"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	managedSpireCRD = ztunnelSpireValues + "," + ztunnelSpireCRD + ",authentication.mutual.spire.install.enabled=true"
	disposableSpire = "authentication.mutual.spire.install.server.dataStorage.enabled=false"
	spireCRDPath    = "cilium/charts/spire-crds/crds/spire.spiffe.io_clusterstaticentries.yaml"
	spireServerPath = "cilium/templates/spire/server/"
)

type spireRenderOptions struct {
	upgrade    bool
	hasCRD     bool
	skipSchema bool
	values     map[string]any
	lookup     func(string, string, string, string) (map[string]any, error)
}

func renderSpireChart(t *testing.T, opts spireRenderOptions, overrides ...string) (map[string]string, error) {
	t.Helper()
	chart, err := loader.Load("cilium")
	require.NoError(t, err)
	values := opts.values
	if values == nil {
		values = map[string]any{}
	}
	for _, override := range overrides {
		require.NoError(t, strvals.ParseInto(override, values))
	}
	require.NoError(t, chartdeps.ProcessDependencies(chart, values))
	caps := common.DefaultCapabilities.Copy()
	if opts.hasCRD {
		caps.APIVersions = append(caps.APIVersions, "spire.spiffe.io/v1alpha1/ClusterStaticEntry")
	}
	renderValues, err := chartutil.ToRenderValuesWithSchemaValidation(chart, values, common.ReleaseOptions{
		Name:      "cilium",
		Namespace: "kube-system",
		IsInstall: !opts.upgrade,
		IsUpgrade: opts.upgrade,
	}, caps, opts.skipSchema)
	if err != nil {
		return nil, err
	}
	renderer := engine.Engine{}
	if opts.lookup != nil {
		renderer.CustomTemplateFuncs = template.FuncMap{"lookup": opts.lookup}
	}
	rendered, err := renderer.RenderWithContext(t.Context(), chart, renderValues)
	if err != nil {
		return nil, err
	}
	for _, crd := range chart.CRDObjects() {
		rendered[crd.Filename] = string(crd.File.Data)
	}
	return rendered, nil
}

func spireManifest[T any](t *testing.T, rendered map[string]string, path string) T {
	t.Helper()
	require.NotEmpty(t, strings.TrimSpace(rendered[path]), path)
	var resource T
	require.NoError(t, yaml.UnmarshalStrict([]byte(rendered[path]), &resource), path)
	return resource
}

func spireContainer(t *testing.T, pod corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, container := range pod.Containers {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("container %s not found", name)
	return corev1.Container{}
}

func spireVolume(t *testing.T, pod corev1.PodSpec, name string) corev1.Volume {
	t.Helper()
	for _, volume := range pod.Volumes {
		if volume.Name == name {
			return volume
		}
	}
	t.Fatalf("volume %s not found", name)
	return corev1.Volume{}
}

func TestSpireServerModes(t *testing.T) {
	for _, tt := range []struct {
		name           string
		values         []string
		crd            bool
		disposable     bool
		replicas       int32
		upstreamSecret string
	}{
		{name: "legacy", values: []string{"authentication.mutual.spire.enabled=true"}, replicas: 1},
		{name: "legacy ztunnel", values: []string{ztunnelSpireValues, "authentication.mutual.spire.install.enabled=true"}, replicas: 1},
		{name: "legacy custom root", values: []string{"authentication.mutual.spire.enabled=true,authentication.mutual.spire.install.server.ca.upstreamSecret=custom-root"}, replicas: 1, upstreamSecret: "custom-root"},
		{name: "persistent CRD", values: []string{managedSpireCRD}, crd: true, replicas: 1},
		{name: "two persistent servers", values: []string{managedSpireCRD, "authentication.mutual.spire.install.server.replicas=2"}, crd: true, replicas: 2, upstreamSecret: "spire-upstream-ca"},
		{name: "three persistent servers", values: []string{managedSpireCRD, "authentication.mutual.spire.install.server.replicas=3"}, crd: true, replicas: 3, upstreamSecret: "spire-upstream-ca"},
		{name: "disposable single server", values: []string{managedSpireCRD, disposableSpire}, crd: true, disposable: true, replicas: 1, upstreamSecret: "spire-upstream-ca"},
		{name: "two disposable servers", values: []string{managedSpireCRD, disposableSpire, "authentication.mutual.spire.install.server.replicas=2"}, crd: true, disposable: true, replicas: 2, upstreamSecret: "spire-upstream-ca"},
		{name: "single custom root", values: []string{managedSpireCRD, "authentication.mutual.spire.install.server.ca.upstreamSecret=custom-root"}, crd: true, replicas: 1, upstreamSecret: "custom-root"},
		{name: "persistent custom root", values: []string{managedSpireCRD, "authentication.mutual.spire.install.server.replicas=2,authentication.mutual.spire.install.server.ca.upstreamSecret=custom-root"}, crd: true, replicas: 2, upstreamSecret: "custom-root"},
		{name: "disposable custom root", values: []string{managedSpireCRD, disposableSpire, "authentication.mutual.spire.install.server.ca.upstreamSecret=custom-root"}, crd: true, disposable: true, replicas: 1, upstreamSecret: "custom-root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rendered, err := renderZtunnelSpire(t, tt.values...)
			require.NoError(t, err)
			sts := spireManifest[appsv1.StatefulSet](t, rendered, spireServerPath+"statefulset.yaml")
			pod := sts.Spec.Template.Spec
			require.Equal(t, tt.replicas, *sts.Spec.Replicas)
			require.Len(t, pod.Containers, 2)
			require.Equal(t, !tt.crd, *pod.ShareProcessNamespace)
			server := spireContainer(t, pod, "spire-server")
			require.Contains(t, server.Image, "spire-server:1.15.3@sha256:")
			config := spireManifest[corev1.ConfigMap](t, rendered, spireServerPath+"configmap.yaml").Data["server.conf"]
			require.Contains(t, config, `connection_string = "/run/spire/data/datastore.sqlite3"`)
			require.Contains(t, config, `"spiffe://spiffe.cilium/cilium-operator"`)

			socket := spireVolume(t, pod, "spire-server-socket")
			if tt.crd {
				require.NotNil(t, socket.EmptyDir)
				require.Nil(t, socket.HostPath)
				require.NotEmpty(t, rendered[spireCRDPath])
				require.Equal(t, int64(1000), *pod.SecurityContext.RunAsUser)
				require.Equal(t, int64(1000), *pod.SecurityContext.FSGroup)
				controller := spireContainer(t, pod, "spire-controller-manager")
				require.Contains(t, controller.Image, "spire-controller-manager:0.6.2@sha256:")
				require.Equal(t, []corev1.EnvVar{{Name: "ENABLE_WEBHOOKS", Value: "false"}}, controller.Env)
				require.Equal(t, "/readyz", controller.ReadinessProbe.HTTPGet.Path)
				require.Contains(t, controller.VolumeMounts, corev1.VolumeMount{
					Name: "spire-server-socket", MountPath: "/tmp/spire-server/private", ReadOnly: true,
				})
				require.Len(t, controller.VolumeMounts, 2)
				cm := spireManifest[corev1.ConfigMap](t, rendered, spireServerPath+"controller-manager-configmap.yaml")
				var cfg map[string]any
				require.NoError(t, yaml.UnmarshalStrict([]byte(cm.Data["controller-manager.yaml"]), &cfg))
				require.Equal(t, map[string]any{"leaderElect": false}, cfg["leaderElection"])
				require.Equal(t, map[string]any{"clusterStaticEntries": true, "clusterSPIFFEIDs": false, "clusterFederatedTrustDomains": false}, cfg["reconcile"])
				require.Equal(t, "/tmp/spire-server/private/api.sock", cfg["spireServerSocketPath"])
				require.Equal(t, "cilium", cfg["entryIDPrefix"])
				require.NotContains(t, cfg, "entryIDPrefixCleanup")
				require.NotEmpty(t, sts.Spec.Template.Annotations["checksum/spire-server-config"])
				require.NotEmpty(t, sts.Spec.Template.Annotations["checksum/spire-controller-config"])
			} else {
				require.Equal(t, "/var/run/spire-server/sockets", socket.HostPath.Path)
				require.Empty(t, rendered[spireCRDPath])
				require.Empty(t, strings.TrimSpace(rendered[spireServerPath+"clusterstaticentries.yaml"]))
				require.Empty(t, strings.TrimSpace(rendered[spireServerPath+"controller-manager-configmap.yaml"]))
				init := spireContainer(t, pod, "cilium-init")
				require.Contains(t, init.Command[2], "Ensuring cilium-agent entry")
				require.Contains(t, init.Command[2], "Ensuring cilium-operator entry")
				require.Equal(t, tt.name == "legacy ztunnel", strings.Contains(init.Command[2], "Ensuring ztunnel entry"))
			}
			if tt.disposable {
				require.Empty(t, sts.Spec.VolumeClaimTemplates)
				require.NotNil(t, spireVolume(t, pod, "spire-data").EmptyDir)
				require.Contains(t, config, `KeyManager "memory"`)
				require.NotContains(t, config, `KeyManager "disk"`)
			} else {
				require.Len(t, sts.Spec.VolumeClaimTemplates, 1)
				claim := sts.Spec.VolumeClaimTemplates[0]
				require.Equal(t, "spire-data", claim.Name)
				require.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claim.Spec.AccessModes)
				require.Equal(t, "1Gi", claim.Spec.Resources.Requests.Storage().String())
				require.Contains(t, server.VolumeMounts, corev1.VolumeMount{Name: "spire-data", MountPath: "/run/spire/data"})
				for _, volume := range pod.Volumes {
					require.NotEqual(t, "spire-data", volume.Name, "the StatefulSet must create a separate claim for each ordinal")
				}
				require.Contains(t, config, `KeyManager "disk"`)
				require.Contains(t, config, `keys_path = "/run/spire/data/keys.json"`)
				require.NotContains(t, config, `KeyManager "memory"`)
			}
			if tt.upstreamSecret != "" {
				require.Equal(t, int64(1000), *pod.SecurityContext.FSGroup)
				require.Contains(t, config, `UpstreamAuthority "disk"`)
				require.Contains(t, config, `cert_file_path = "/run/spire/upstream/tls.crt"`)
				require.Contains(t, config, `key_file_path = "/run/spire/upstream/tls.key"`)
				secret := spireVolume(t, pod, "spire-upstream-ca").Secret
				require.Equal(t, tt.upstreamSecret, secret.SecretName)
				require.Equal(t, int32(0440), *secret.DefaultMode)
				require.Equal(t, []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}}, secret.Items)
				require.Contains(t, server.VolumeMounts, corev1.VolumeMount{Name: "spire-upstream-ca", MountPath: "/run/spire/upstream", ReadOnly: true})
			} else {
				require.NotContains(t, config, "UpstreamAuthority")
				for _, volume := range pod.Volumes {
					require.NotEqual(t, "spire-upstream-ca", volume.Name)
				}
			}
			service := spireManifest[corev1.Service](t, rendered, spireServerPath+"service.yaml")
			require.Equal(t, sts.Spec.Selector.MatchLabels, service.Spec.Selector)
			require.False(t, service.Spec.PublishNotReadyAddresses)
			if tt.replicas > 1 {
				require.Equal(t, corev1.ServiceAffinityClientIP, service.Spec.SessionAffinity)
				terms := pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
				require.Len(t, terms, 1)
				require.Equal(t, "kubernetes.io/hostname", terms[0].TopologyKey)
				require.Equal(t, service.Spec.Selector, terms[0].LabelSelector.MatchLabels)
			} else {
				require.Empty(t, service.Spec.SessionAffinity)
				require.Nil(t, pod.Affinity)
			}
		})
	}
}

func TestSpireBootstrapSchemaAndOverrides(t *testing.T) {
	rendered, err := renderZtunnelSpire(t, managedSpireCRD, disposableSpire,
		"cluster.name=lab-test,namespaceOverride=network-system,authentication.mutual.spire.trustDomain=workload.test,authentication.mutual.spire.install.namespace=identity-system,authentication.mutual.spire.install.existingNamespace=true",
		"serviceAccounts.cilium.name=cilium-custom,serviceAccounts.operator.name=operator-custom,serviceAccounts.ztunnel.name=ztunnel-custom,serviceAccounts.ztunnel.create=false",
		"authentication.mutual.spire.install.agent.serviceAccount.name=agent-custom,authentication.mutual.spire.install.server.serviceAccount.name=server-custom",
		"authentication.mutual.spire.adminSocketPath=/run/identity/admin/admin.sock,authentication.mutual.spire.agentSocketPath=/run/identity/workload/api.sock,encryption.ztunnel.spire.containerRuntimeSocketPath=/run/custom/containerd.sock",
		"encryption.ztunnel.spire.className=custom-class",
		`commonLabels.app\.kubernetes\.io/managed-by=cilium-operator-ztunnel`,
		"authentication.mutual.spire.install.server.securityContext.runAsUser=2000,authentication.mutual.spire.install.server.podSecurityContext.fsGroup=2000,commonLabels.team=networking,authentication.mutual.spire.annotations.owner=platform")
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(rendered["cilium/templates/spire/namespace.yaml"]))
	crd := spireManifest[apiextensionsv1.CustomResourceDefinition](t, rendered, spireCRDPath)
	require.Equal(t, "clusterstaticentries.spire.spiffe.io", crd.Name)
	require.Equal(t, apiextensionsv1.ClusterScoped, crd.Spec.Scope)
	require.Len(t, crd.Spec.Versions, 1)
	require.Equal(t, "v1alpha1", crd.Spec.Versions[0].Name)
	require.NotNil(t, crd.Spec.Versions[0].Subresources.Status)
	var internal apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil))
	validator, _, err := crdvalidation.NewSchemaValidator(&internal)
	require.NoError(t, err)

	agentID := "spiffe://workload.test/ns/identity-system/sa/agent-custom"
	entries := map[string]*unstructured.Unstructured{}
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered[spireServerPath+"clusterstaticentries.yaml"]), 4096)
	for {
		entry := &unstructured.Unstructured{}
		err := decoder.Decode(&entry.Object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(entry.Object) == 0 {
			continue
		}
		require.Equal(t, "ClusterStaticEntry", entry.GetKind())
		require.Empty(t, entry.GetNamespace())
		require.Equal(t, "Helm", entry.GetLabels()["app.kubernetes.io/managed-by"])
		require.Equal(t, "networking", entry.GetLabels()["team"])
		require.Equal(t, "platform", entry.GetAnnotations()["owner"])
		require.Equal(t, "custom-class", entry.Object["spec"].(map[string]any)["className"])
		require.Empty(t, crdvalidation.ValidateCustomResource(field.NewPath("entry"), entry.Object, validator))
		id, _, err := unstructured.NestedString(entry.Object, "spec", "spiffeID")
		require.NoError(t, err)
		entries[id] = entry
	}
	require.Len(t, entries, 4)
	for id, account := range map[string]string{"cilium-agent": "cilium-custom", "cilium-operator": "operator-custom", "ztunnel": "ztunnel-custom"} {
		entry := entries["spiffe://workload.test/"+id]
		require.NotNil(t, entry)
		parent, _, err := unstructured.NestedString(entry.Object, "spec", "parentID")
		require.NoError(t, err)
		require.Equal(t, agentID, parent)
		selectors, _, err := unstructured.NestedStringSlice(entry.Object, "spec", "selectors")
		require.NoError(t, err)
		require.Equal(t, []string{"k8s:ns:network-system", "k8s:sa:" + account}, selectors)
	}
	node := entries[agentID]
	require.NotNil(t, node)
	require.Equal(t, map[string]any{
		"className": "custom-class",
		"parentID":  "spiffe://workload.test/spire/server",
		"spiffeID":  agentID,
		"selectors": []any{"k8s_psat:cluster:lab-test", "k8s_psat:agent_ns:identity-system", "k8s_psat:agent_sa:agent-custom"},
	}, node.Object["spec"])
	invalid := node.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(invalid.Object, true, "spec", "spiffeID"))
	require.NotEmpty(t, crdvalidation.ValidateCustomResource(field.NewPath("entry"), invalid.Object, validator))
	unstructured.RemoveNestedField(invalid.Object, "spec", "parentID")
	require.NotEmpty(t, crdvalidation.ValidateCustomResource(field.NewPath("entry"), invalid.Object, validator))

	sts := spireManifest[appsv1.StatefulSet](t, rendered, spireServerPath+"statefulset.yaml")
	require.Equal(t, "identity-system", sts.Namespace)
	require.Equal(t, "server-custom", sts.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, int64(2000), *sts.Spec.Template.Spec.SecurityContext.FSGroup)
	for _, container := range sts.Spec.Template.Spec.Containers {
		require.Equal(t, int64(2000), *container.SecurityContext.RunAsUser)
	}
	serverConf := spireManifest[corev1.ConfigMap](t, rendered, spireServerPath+"configmap.yaml").Data["server.conf"]
	require.Contains(t, serverConf, `service_account_allow_list = ["identity-system:agent-custom"]`)
	require.Contains(t, serverConf, `"lab-test" = {`)
	require.Contains(t, serverConf, `trust_domain = "workload.test"`)
	agent := spireManifest[appsv1.DaemonSet](t, rendered, "cilium/templates/spire/agent/daemonset.yaml")
	require.Equal(t, "agent-custom", agent.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, "/run/identity/admin", spireVolume(t, agent.Spec.Template.Spec, "spire-agent-socket").HostPath.Path)
	require.Equal(t, "/run/identity/workload", spireVolume(t, agent.Spec.Template.Spec, "spire-workload-socket").HostPath.Path)
	agentConf := spireManifest[corev1.ConfigMap](t, rendered, "cilium/templates/spire/agent/configmap.yaml").Data["agent.conf"]
	require.Contains(t, agentConf, `"spiffe://workload.test/cilium-agent"`)
	require.Contains(t, agentConf, `"spiffe://workload.test/ztunnel"`)
	require.Contains(t, agentConf, `socket_path = "/run/identity/workload/api.sock"`)

	ztunnel := spireManifest[appsv1.DaemonSet](t, rendered, "cilium/templates/ztunnel/daemonset.yaml")
	pod := ztunnel.Spec.Template.Spec
	require.Equal(t, "ztunnel-custom", pod.ServiceAccountName)
	env := spireContainer(t, pod, "istio-proxy").Env
	for name, value := range map[string]string{
		"SPIRE_ENABLED": "true", "SPIRE_ADMIN_ENDPOINT_SOCKET": "unix:///run/identity/admin/admin.sock",
		"CONTAINER_RUNTIME_SOCK_PATH": "/run/custom/containerd.sock", "TRUST_DOMAIN": "workload.test", "CLUSTER_DOMAIN": "workload.test",
	} {
		require.Contains(t, env, corev1.EnvVar{Name: name, Value: value})
	}
	require.Equal(t, "/run/identity/admin", spireVolume(t, pod, "spire-agent-socket").HostPath.Path)
	require.Equal(t, "/run/custom/containerd.sock", spireVolume(t, pod, "container-runtime").HostPath.Path)
	require.Equal(t, corev1.HostPathSocket, *spireVolume(t, pod, "container-runtime").HostPath.Type)
	ciliumConf := spireManifest[corev1.ConfigMap](t, rendered, "cilium/templates/cilium-configmap.yaml")
	require.Equal(t, "workload.test", ciliumConf.Data["mesh-auth-spiffe-trust-domain"])
	require.Equal(t, "workload.test", ciliumConf.Data["ztunnel-spiffe-trust-domain"])
	ciliumAgent := spireManifest[appsv1.DaemonSet](t, rendered, "cilium/templates/cilium-agent/daemonset.yaml")
	require.Equal(t, "/run/identity/admin", spireVolume(t, ciliumAgent.Spec.Template.Spec, "spire-agent-socket").HostPath.Path)
	operator := spireManifest[appsv1.Deployment](t, rendered, "cilium/templates/cilium-operator/deployment.yaml")
	require.Equal(t, "/run/identity/workload", spireVolume(t, operator.Spec.Template.Spec, "spire-agent-socket").HostPath.Path)

	role := spireManifest[rbacv1.ClusterRole](t, rendered, spireServerPath+"clusterrole.yaml")
	binding := spireManifest[rbacv1.ClusterRoleBinding](t, rendered, spireServerPath+"clusterrolebinding.yaml")
	require.Equal(t, role.Name, binding.RoleRef.Name)
	require.Equal(t, "server-custom", binding.Subjects[0].Name)
	require.Equal(t, "identity-system", binding.Subjects[0].Namespace)
	var crdRules []rbacv1.PolicyRule
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "spire.spiffe.io") {
			crdRules = append(crdRules, rule)
		}
		require.NotContains(t, rule.Resources, "secrets")
		require.NotContains(t, rule.Resources, "leases")
		require.NotContains(t, rule.Resources, "validatingwebhookconfigurations")
	}
	require.Equal(t, []rbacv1.PolicyRule{
		{APIGroups: []string{"spire.spiffe.io"}, Resources: []string{"clusterstaticentries"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"spire.spiffe.io"}, Resources: []string{"clusterstaticentries/status"}, Verbs: []string{"get", "patch", "update"}},
	}, crdRules)
	nsRole := spireManifest[rbacv1.Role](t, rendered, spireServerPath+"role.yaml")
	nsBinding := spireManifest[rbacv1.RoleBinding](t, rendered, spireServerPath+"rolebinding.yaml")
	require.Equal(t, nsRole.Name, nsBinding.RoleRef.Name)
}

func TestSpireInstallationOwnership(t *testing.T) {
	for _, tt := range []struct {
		values  []string
		wantCRD bool
	}{
		{},
		{values: []string{ztunnelSpireValues}},
		{values: []string{ztunnelSpireValues, ztunnelSpireCRD, "authentication.mutual.spire.install.crds.enabled=false,authentication.mutual.spire.install.server.replicas=3"}},
		{values: []string{ztunnelSpireValues, ztunnelSpireCRD}, wantCRD: true},
		{values: []string{"authentication.mutual.spire.install.crds.enabled=true"}, wantCRD: true},
	} {
		rendered, err := renderZtunnelSpire(t, tt.values...)
		require.NoError(t, err)
		require.Equal(t, tt.wantCRD, strings.TrimSpace(rendered[spireCRDPath]) != "")
		for path, manifest := range rendered {
			if strings.HasPrefix(path, "cilium/templates/spire/") {
				require.Empty(t, strings.TrimSpace(manifest), path)
			}
		}
	}
	for _, opts := range []spireRenderOptions{{hasCRD: true}, {upgrade: true, hasCRD: true}} {
		rendered, err := renderSpireChart(t, opts, managedSpireCRD, "authentication.mutual.spire.install.crds.enabled=false")
		require.NoError(t, err)
		require.Empty(t, rendered[spireCRDPath])
		require.NotEmpty(t, rendered[spireServerPath+"clusterstaticentries.yaml"])
	}
	for _, override := range []string{"rbac.create=false", "authentication.mutual.spire.install.server.serviceAccount.create=false"} {
		rendered, err := renderZtunnelSpire(t, managedSpireCRD, override)
		require.NoError(t, err)
		for _, name := range []string{"role.yaml", "rolebinding.yaml", "clusterrole.yaml", "clusterrolebinding.yaml"} {
			require.Empty(t, strings.TrimSpace(rendered[spireServerPath+name]))
		}
	}
	for _, mode := range []string{disposableSpire, "authentication.mutual.spire.install.server.replicas=2"} {
		rendered, err := renderZtunnelSpire(t, managedSpireCRD, mode, "authentication.mutual.spire.annotations.owner=platform")
		require.NoError(t, err)
		namespace := spireManifest[corev1.Namespace](t, rendered, "cilium/templates/spire/namespace.yaml")
		require.Equal(t, "keep", namespace.Annotations["helm.sh/resource-policy"])
		require.Equal(t, "platform", namespace.Annotations["owner"])
	}
}

func TestSpireExternalWiring(t *testing.T) {
	externalEnv := []corev1.EnvVar{
		{Name: "SPIRE_ENABLED", Value: "true"},
		{Name: "SPIRE_ADMIN_ENDPOINT_SOCKET", Value: "unix:///run/external/admin.sock"},
		{Name: "CONTAINER_RUNTIME_SOCK_PATH", Value: "/run/custom/containerd.sock"},
		{Name: "TRUST_DOMAIN", Value: "external.test"},
		{Name: "CLUSTER_DOMAIN", Value: "external.test"},
	}
	var env []any
	for _, variable := range externalEnv {
		env = append(env, map[string]any{"name": variable.Name, "value": variable.Value})
	}
	values := map[string]any{
		"encryption": map[string]any{
			"ztunnel": map[string]any{
				"extraEnv": env,
				"extraVolumes": []any{
					map[string]any{"name": "spire-agent-socket", "hostPath": map[string]any{"path": "/run/external", "type": "Directory"}},
					map[string]any{"name": "container-runtime", "hostPath": map[string]any{"path": "/run/custom/containerd.sock", "type": "Socket"}},
				},
				"extraVolumeMounts": []any{
					map[string]any{"name": "spire-agent-socket", "mountPath": "/run/external", "readOnly": true},
					map[string]any{"name": "container-runtime", "mountPath": "/run/custom/containerd.sock", "readOnly": true},
				},
			},
		},
	}
	rendered, err := renderSpireChart(t, spireRenderOptions{values: values}, ztunnelSpireValues, ztunnelSpireCRD,
		"authentication.mutual.spire.trustDomain=external.test,authentication.mutual.spire.install.crds.enabled=false")
	require.NoError(t, err)
	require.Empty(t, rendered[spireCRDPath])
	ztunnel := spireManifest[appsv1.DaemonSet](t, rendered, "cilium/templates/ztunnel/daemonset.yaml")
	pod := ztunnel.Spec.Template.Spec
	container := spireContainer(t, pod, "istio-proxy")
	var actual []corev1.EnvVar
	for _, variable := range container.Env {
		if slices.ContainsFunc(externalEnv, func(expected corev1.EnvVar) bool { return expected.Name == variable.Name }) {
			actual = append(actual, variable)
		}
	}
	require.ElementsMatch(t, externalEnv, actual)
	require.Equal(t, "/run/external", spireVolume(t, pod, "spire-agent-socket").HostPath.Path)
	require.Equal(t, "/run/custom/containerd.sock", spireVolume(t, pod, "container-runtime").HostPath.Path)
	require.Contains(t, container.VolumeMounts, corev1.VolumeMount{Name: "spire-agent-socket", MountPath: "/run/external", ReadOnly: true})
	require.Contains(t, container.VolumeMounts, corev1.VolumeMount{Name: "container-runtime", MountPath: "/run/custom/containerd.sock", ReadOnly: true})
	ciliumConf := spireManifest[corev1.ConfigMap](t, rendered, "cilium/templates/cilium-configmap.yaml")
	require.Equal(t, "external.test", ciliumConf.Data["ztunnel-spiffe-trust-domain"])
}

func TestSpirePreservesInternalCATrustDomain(t *testing.T) {
	rendered, err := renderZtunnelSpire(t, ztunnelSpireValues, "encryption.ztunnel.ca.type=internal")
	require.NoError(t, err)
	ciliumConf := spireManifest[corev1.ConfigMap](t, rendered, "cilium/templates/cilium-configmap.yaml")
	require.Equal(t, "spiffe.cilium", ciliumConf.Data["mesh-auth-spiffe-trust-domain"])
	require.NotContains(t, ciliumConf.Data, "ztunnel-spiffe-trust-domain")
}

func TestSpireInvalidConfigurations(t *testing.T) {
	for _, tt := range []struct {
		name     string
		override string
		error    string
	}{
		{"zero replicas", "authentication.mutual.spire.install.server.replicas=0", "replicas"},
		{"negative replicas", "authentication.mutual.spire.install.server.replicas=-1", "replicas"},
		{"fractional replicas", "authentication.mutual.spire.install.server.replicas=1.5", "replicas"},
		{"boolean replicas", "authentication.mutual.spire.install.server.replicas=true", "replicas"},
		{"invalid replicas", "authentication.mutual.spire.install.server.replicas=many", "replicas"},
		{"overflow replicas", "authentication.mutual.spire.install.server.replicas=2147483648", "replicas"},
		{"Entry API replicas", "encryption.ztunnel.spire.useCRD=false", "Multiple SPIRE servers require encryption.ztunnel.spire.useCRD=true"},
		{"invalid root name", "authentication.mutual.spire.install.server.ca.upstreamSecret=bad_name", "upstreamSecret"},
		{"invalid root type", "authentication.mutual.spire.install.server.ca.upstreamSecret=true", "upstreamSecret"},
		{"invalid CRD override", "authentication.mutual.spire.install.crds.enabled=auto", "/crds/enabled"},
		{"NodePort routing", "authentication.mutual.spire.install.server.service.type=NodePort", "service.type=ClusterIP"},
		{"server override", "authentication.mutual.spire.serverAddress=other.example:8081", "leave authentication.mutual.spire.serverAddress unset"},
		{"missing CRD", "authentication.mutual.spire.install.crds.enabled=false", "requires the ClusterStaticEntry CRD"},
		{"relative socket", "authentication.mutual.spire.adminSocketPath=relative/admin.sock", "clean absolute socket path"},
		{"host root mount", "authentication.mutual.spire.adminSocketPath=/admin.sock", "non-root directory"},
		{"shared socket", "authentication.mutual.spire.agentSocketPath=/run/spire/sockets/admin.sock", "must be different"},
		{"runtime directory", "encryption.ztunnel.spire.containerRuntimeSocketPath=/run/containerd/", "containerRuntimeSocketPath"},
		{"duplicate env", "encryption.ztunnel.extraEnv[0].name=SPIRE_ENABLED,encryption.ztunnel.extraEnv[0].value=false", "Remove SPIRE_ENABLED"},
		{"duplicate volume", "encryption.ztunnel.extraVolumes[0].name=spire-agent-socket", "Remove the spire-agent-socket"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := renderZtunnelSpire(t, managedSpireCRD, "authentication.mutual.spire.install.server.replicas=2", tt.override)
			require.ErrorContains(t, err, tt.error)
		})
	}
	_, err := renderSpireChart(t, spireRenderOptions{upgrade: true}, managedSpireCRD)
	require.ErrorContains(t, err, "Install the bundled spire-crds chart before upgrading")
	for _, value := range []any{"2", 1.5, false, 0, -1, 2147483648} {
		t.Run("schema", func(t *testing.T) {
			values := map[string]any{"authentication": map[string]any{"mutual": map[string]any{"spire": map[string]any{"install": map[string]any{"server": map[string]any{"replicas": value}}}}}}
			_, err := renderSpireChart(t, spireRenderOptions{values: values}, managedSpireCRD)
			require.ErrorContains(t, err, "replicas")
		})
		t.Run("without schema", func(t *testing.T) {
			values := map[string]any{"authentication": map[string]any{"mutual": map[string]any{"spire": map[string]any{"install": map[string]any{"server": map[string]any{"replicas": value}}}}}}
			_, err := renderSpireChart(t, spireRenderOptions{values: values, skipSchema: true}, managedSpireCRD)
			require.ErrorContains(t, err, "replicas must be an integer")
		})
	}
}

func TestSpireHAExampleAndRollout(t *testing.T) {
	data, err := os.ReadFile("../../examples/kubernetes-ztunnel/spire-ha.yaml")
	require.NoError(t, err)
	values := map[string]any{}
	require.NoError(t, yaml.UnmarshalStrict(data, &values))
	for _, path := range [][]string{
		{"authentication", "mutual", "spire", "install", "crds"},
		{"authentication", "mutual", "spire", "install", "server", "dataStorage"},
		{"authentication", "mutual", "spire", "install", "server", "ca"},
	} {
		_, found, err := unstructured.NestedFieldNoCopy(values, path...)
		require.NoError(t, err)
		require.False(t, found, "the ordinary example must not require %v", path)
	}
	rendered, err := renderSpireChart(t, spireRenderOptions{values: values})
	require.NoError(t, err)
	sts := spireManifest[appsv1.StatefulSet](t, rendered, spireServerPath+"statefulset.yaml")
	require.Equal(t, int32(2), *sts.Spec.Replicas)
	require.Len(t, sts.Spec.VolumeClaimTemplates, 1)
	require.Equal(t, "spire-upstream-ca", spireVolume(t, sts.Spec.Template.Spec, "spire-upstream-ca").Secret.SecretName)
	require.NotEmpty(t, rendered[spireCRDPath])
	changed, err := renderSpireChart(t, spireRenderOptions{values: values},
		"authentication.mutual.spire.trustDomain=changed.example,authentication.mutual.spire.install.server.controllerManager.image.override=registry.example/controller:custom",
		"authentication.mutual.spire.install.server.controllerManager.resources.requests.cpu=150m,authentication.mutual.spire.install.server.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100,authentication.mutual.spire.install.server.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].podAffinityTerm.topologyKey=kubernetes.io/hostname")
	require.NoError(t, err)
	next := spireManifest[appsv1.StatefulSet](t, changed, spireServerPath+"statefulset.yaml")
	for _, key := range []string{"checksum/spire-server-config", "checksum/spire-controller-config"} {
		require.NotEqual(t, sts.Spec.Template.Annotations[key], next.Spec.Template.Annotations[key])
	}
	beforeAgent := spireManifest[appsv1.DaemonSet](t, rendered, "cilium/templates/spire/agent/daemonset.yaml")
	afterAgent := spireManifest[appsv1.DaemonSet](t, changed, "cilium/templates/spire/agent/daemonset.yaml")
	require.NotEqual(t, beforeAgent.Spec.Template.Annotations["checksum/spire-agent-config"], afterAgent.Spec.Template.Annotations["checksum/spire-agent-config"])
	controller := spireContainer(t, next.Spec.Template.Spec, "spire-controller-manager")
	require.Equal(t, "registry.example/controller:custom", controller.Image)
	require.Equal(t, int64(150), controller.Resources.Requests.Cpu().MilliValue())
	require.Empty(t, next.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
}

func TestSpirePersistentScale(t *testing.T) {
	const storage = "authentication.mutual.spire.install.server.dataStorage.storageClass=managed-csi,authentication.mutual.spire.install.server.dataStorage.size=2Gi"
	before, err := renderZtunnelSpire(t, managedSpireCRD, storage, "authentication.mutual.spire.install.server.replicas=2")
	require.NoError(t, err)
	after, err := renderZtunnelSpire(t, managedSpireCRD, storage, "authentication.mutual.spire.install.server.replicas=3")
	require.NoError(t, err)
	first := spireManifest[appsv1.StatefulSet](t, before, spireServerPath+"statefulset.yaml")
	second := spireManifest[appsv1.StatefulSet](t, after, spireServerPath+"statefulset.yaml")
	require.Equal(t, int32(3), *second.Spec.Replicas)
	require.Equal(t, first.Spec.VolumeClaimTemplates, second.Spec.VolumeClaimTemplates)
	require.Equal(t, first.Spec.Template, second.Spec.Template)
	require.Equal(t, "managed-csi", *first.Spec.VolumeClaimTemplates[0].Spec.StorageClassName)
	require.Equal(t, "2Gi", first.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String())
}

func TestSpireUpgradeTrust(t *testing.T) {
	for _, tt := range []struct {
		name         string
		previousRoot string
		replicas     int
		override     string
		wantError    bool
	}{
		{name: "single unchanged", replicas: 1},
		{name: "multiple unchanged", previousRoot: "spire-upstream-ca", replicas: 2},
		{name: "automatic scale up", replicas: 2, wantError: true},
		{name: "explicit migration", replicas: 2, override: "spire-upstream-ca"},
		{name: "automatic scale down", previousRoot: "spire-upstream-ca", replicas: 1, wantError: true},
		{name: "retain root on scale down", previousRoot: "spire-upstream-ca", replicas: 1, override: "spire-upstream-ca"},
		{name: "custom root not replaced", previousRoot: "custom-root", replicas: 2, wantError: true},
		{name: "retain custom root", previousRoot: "custom-root", replicas: 2, override: "custom-root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			volumes := []any{}
			if tt.previousRoot != "" {
				volumes = append(volumes, map[string]any{"name": "spire-upstream-ca", "secret": map[string]any{"secretName": tt.previousRoot}})
			}
			lookups := 0
			lookup := func(apiVersion, kind, namespace, name string) (map[string]any, error) {
				if apiVersion != "apps/v1" || kind != "StatefulSet" {
					return map[string]any{}, nil
				}
				require.Equal(t, "cilium-spire", namespace)
				require.Equal(t, "spire-server", name)
				lookups++
				return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": volumes}}}}, nil
			}
			_, err := renderSpireChart(t, spireRenderOptions{upgrade: true, hasCRD: true, lookup: lookup},
				managedSpireCRD, fmt.Sprintf("authentication.mutual.spire.install.server.replicas=%d,authentication.mutual.spire.install.server.ca.upstreamSecret=%s", tt.replicas, tt.override))
			if tt.wantError {
				require.ErrorContains(t, err, "Changing the automatic SPIRE upstream root")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.override == "", lookups == 1)
		})
	}
	_, err := renderSpireChart(t, spireRenderOptions{
		upgrade: true,
		hasCRD:  true,
		lookup: func(apiVersion, kind, namespace, name string) (map[string]any, error) {
			if apiVersion == "apps/v1" && kind == "StatefulSet" {
				return nil, errors.New("cannot read existing SPIRE StatefulSet")
			}
			return map[string]any{}, nil
		},
	}, managedSpireCRD)
	require.ErrorContains(t, err, "cannot read existing SPIRE StatefulSet")
}

func TestSpireLegacyAgentRollout(t *testing.T) {
	const path = "cilium/templates/spire/agent/daemonset.yaml"
	rendered, err := renderZtunnelSpire(t, "authentication.mutual.spire.enabled=true")
	require.NoError(t, err)
	legacy := spireManifest[appsv1.DaemonSet](t, rendered, path)
	require.NotContains(t, legacy.Spec.Template.Annotations, "checksum/spire-agent-config")

	rendered, err = renderZtunnelSpire(t, ztunnelSpireValues, "authentication.mutual.spire.install.enabled=true")
	require.NoError(t, err)
	enabled := spireManifest[appsv1.DaemonSet](t, rendered, path)
	require.NotEmpty(t, enabled.Spec.Template.Annotations["checksum/spire-agent-config"])

	rendered, err = renderZtunnelSpire(t, ztunnelSpireValues,
		"authentication.mutual.spire.install.enabled=true,authentication.mutual.spire.trustDomain=changed.test")
	require.NoError(t, err)
	changed := spireManifest[appsv1.DaemonSet](t, rendered, path)
	require.NotEqual(t, enabled.Spec.Template.Annotations["checksum/spire-agent-config"], changed.Spec.Template.Annotations["checksum/spire-agent-config"])
}
