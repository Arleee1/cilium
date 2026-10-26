// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/spf13/pflag"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	netutil "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"

	ztunnel "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
)

const (
	registrationManagedByLabel = "app.kubernetes.io/managed-by"
	registrationManager        = "cilium-operator"
	registrationTimeout        = 2 * time.Minute
	registrationAttempts       = 6
	workloadIDTemplate         = "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"
)

var registrationGroupVersion = schema.GroupVersion{Group: "spire.spiffe.io", Version: "v1alpha1"}

type registrationConfig struct {
	NamePrefix      string            `mapstructure:"mesh-auth-spire-registration-name-prefix"`
	ClassName       string            `mapstructure:"mesh-auth-spire-registration-class"`
	ServiceAccounts map[string]string `mapstructure:"mesh-auth-spire-registration-service-accounts"`
	Labels          map[string]string `mapstructure:"mesh-auth-spire-registration-labels"`
	Annotations     map[string]string `mapstructure:"mesh-auth-spire-registration-annotations"`
}

var defaultRegistrationConfig = registrationConfig{
	NamePrefix: "cilium-spire-kube-system-cilium",
	ClassName:  "cilium-spire",
	ServiceAccounts: map[string]string{
		"spire-agent":     "cilium-spire/spire-agent",
		"cilium-agent":    "kube-system/cilium",
		"cilium-operator": "kube-system/cilium-operator",
		"ztunnel":         "kube-system/ztunnel-cilium",
	},
}

func (cfg registrationConfig) Flags(flags *pflag.FlagSet) {
	flags.String("mesh-auth-spire-registration-name-prefix", cfg.NamePrefix,
		"Name prefix for operator-managed SPIRE registration resources in CRD mode.")
	flags.String("mesh-auth-spire-registration-class", cfg.ClassName,
		"Class of operator-managed SPIRE registration resources; empty selects classless resources.")
	flags.StringToString("mesh-auth-spire-registration-service-accounts", cfg.ServiceAccounts,
		"Service accounts for SPIRE registration in CRD mode, as component=namespace/name for spire-agent, cilium-agent, cilium-operator and ztunnel.")
	flags.StringToString("mesh-auth-spire-registration-labels", cfg.Labels,
		"Labels for operator-managed SPIRE registration resources.")
	flags.StringToString("mesh-auth-spire-registration-annotations", cfg.Annotations,
		"Annotations for operator-managed SPIRE registration resources.")
}

type registrationParams struct {
	cell.In

	ClientConfig       ClientConfig
	RegistrationConfig registrationConfig
	ZtunnelConfig      ztunnel.Config
	K8sClient          k8sClient.Clientset
	Lifecycle          cell.Lifecycle
}

// registerResources runs under the operator's leader lifecycle. It needs only
// Kubernetes credentials, so registration does not depend on a SPIRE SVID.
func registerResources(p registrationParams) error {
	if !p.ClientConfig.UseCRD || !p.ZtunnelConfig.UseSpireCA() || !p.K8sClient.IsEnabled() {
		return nil
	}
	resources, err := registrationResources(p.RegistrationConfig, p.ClientConfig.SpiffeTrustDomain)
	if err != nil {
		return fmt.Errorf("invalid SPIRE registration configuration: %w", err)
	}
	r := registrationInitializer{client: p.K8sClient, resources: resources}
	p.Lifecycle.Append(cell.Hook{OnStart: func(ctx cell.HookContext) error {
		return r.initialize(ctx)
	}})
	return nil
}

type registrationResource struct {
	gvr    schema.GroupVersionResource
	object *unstructured.Unstructured
}

func registrationResources(cfg registrationConfig, trustDomain string) ([]registrationResource, error) {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, fmt.Errorf("trust domain: %w", err)
	}
	if cfg.NamePrefix == "" {
		return nil, errors.New("resource name prefix must not be empty")
	}
	if errs := validation.IsDNS1123Subdomain(cfg.NamePrefix + "-ztunnel-workloads"); len(errs) != 0 {
		return nil, fmt.Errorf("resource name prefix: %s", strings.Join(errs, ", "))
	}
	components := []string{"spire-agent", "cilium-agent", "cilium-operator", "ztunnel"}
	accounts := make(map[string]types.NamespacedName, len(components))
	for _, component := range components {
		namespace, name, found := strings.Cut(cfg.ServiceAccounts[component], "/")
		if !found || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, fmt.Errorf("service account for %s must be a valid namespace/name", component)
		}
		accounts[component] = types.NamespacedName{Namespace: namespace, Name: name}
	}
	if len(cfg.ServiceAccounts) != len(components) {
		return nil, errors.New("service accounts must contain only spire-agent, cilium-agent, cilium-operator and ztunnel")
	}
	if manager, found := cfg.Labels[registrationManagedByLabel]; found && manager != registrationManager {
		return nil, fmt.Errorf("label %s must be %q", registrationManagedByLabel, registrationManager)
	}
	for _, key := range []string{"meta.helm.sh/release-name", "meta.helm.sh/release-namespace"} {
		if _, found := cfg.Annotations[key]; found {
			return nil, fmt.Errorf("annotation %s would give Helm ownership of an operator-managed resource", key)
		}
	}

	newResource := func(kind, plural, suffix string, spec map[string]any) registrationResource {
		if cfg.ClassName != "" {
			spec["className"] = cfg.ClassName
		}
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": registrationGroupVersion.String(),
			"kind":       kind,
			"spec":       spec,
		}}
		object.SetName(cfg.NamePrefix + "-" + suffix)
		labels := make(map[string]string, len(cfg.Labels)+1)
		maps.Copy(labels, cfg.Labels)
		labels[registrationManagedByLabel] = registrationManager
		object.SetLabels(labels)
		if len(cfg.Annotations) > 0 {
			object.SetAnnotations(cfg.Annotations)
		}
		return registrationResource{gvr: registrationGroupVersion.WithResource(plural), object: object}
	}

	agent := accounts["spire-agent"]
	agentID := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", td.Name(), agent.Namespace, agent.Name)
	resources := []registrationResource{newResource("ClusterStaticEntry", "clusterstaticentries", "spire-agent", map[string]any{
		"spiffeID": agentID,
		"parentID": fmt.Sprintf("spiffe://%s/spire/server", td.Name()),
		"selectors": []any{
			"k8s_psat:agent_ns:" + agent.Namespace,
			"k8s_psat:agent_sa:" + agent.Name,
		},
	})}
	for _, component := range components[1:] {
		account := accounts[component]
		resources = append(resources, newResource("ClusterStaticEntry", "clusterstaticentries", component, map[string]any{
			"spiffeID":  fmt.Sprintf("spiffe://%s/%s", td.Name(), component),
			"parentID":  agentID,
			"selectors": []any{"k8s:ns:" + account.Namespace, "k8s:sa:" + account.Name},
		}))
	}
	resources = append(resources, newResource("ClusterSPIFFEID", "clusterspiffeids", "ztunnel-workloads", map[string]any{
		"namespaceSelector": map[string]any{"matchLabels": map[string]any{"io.cilium/mtls-enabled": "true"}},
		"spiffeIDTemplate":  workloadIDTemplate,
	}))
	return resources, nil
}

type registrationInitializer struct {
	client    dynamic.Interface
	resources []registrationResource
}

func (r *registrationInitializer) initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, registrationTimeout)
	defer cancel()
	for _, resource := range r.resources {
		var lastErr error
		err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
			Duration: time.Second,
			Factor:   2,
			Steps:    registrationAttempts,
		}, func(ctx context.Context) (bool, error) {
			lastErr = r.upsert(ctx, resource)
			if lastErr == nil {
				return true, nil
			}
			if isTransientRegistrationError(lastErr) {
				return false, nil
			}
			return false, lastErr
		})
		if err != nil {
			if wait.Interrupted(err) && !errors.Is(lastErr, err) {
				err = errors.Join(err, lastErr)
			}
			return fmt.Errorf("initialize SPIRE %s %s: %w", resource.object.GetKind(), resource.object.GetName(), err)
		}
	}
	return nil
}

func isTransientRegistrationError(err error) bool {
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) ||
		netutil.IsTimeout(err) || netutil.IsProbableEOF(err) {
		return true
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		switch errno {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return true
		}
	}
	return false
}

func (r *registrationInitializer) upsert(ctx context.Context, resource registrationResource) error {
	client := r.client.Resource(resource.gvr)
	desired := resource.object
	current, err := client.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.Create(ctx, desired.DeepCopy(), metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if current.GetLabels()[registrationManagedByLabel] != registrationManager ||
		current.GetAnnotations()["meta.helm.sh/release-name"] != "" ||
		current.GetAnnotations()["meta.helm.sh/release-namespace"] != "" {
		return errors.New("refusing to overwrite a registration resource not exclusively managed by the Cilium operator")
	}
	if current.GetDeletionTimestamp() != nil {
		return apierrors.NewConflict(resource.gvr.GroupResource(), desired.GetName(), errors.New("registration resource is being deleted"))
	}
	if reflect.DeepEqual(current.Object["spec"], desired.Object["spec"]) &&
		maps.Equal(current.GetLabels(), desired.GetLabels()) &&
		maps.Equal(current.GetAnnotations(), desired.GetAnnotations()) {
		return nil
	}
	// Keep status, finalizers and resourceVersion. Initialization retries
	// conflicts with sidecar status updates using a fresh GET.
	updated := current.DeepCopy()
	updated.Object["spec"] = runtime.DeepCopyJSONValue(desired.Object["spec"])
	updated.SetLabels(desired.GetLabels())
	updated.SetAnnotations(desired.GetAnnotations())
	_, err = client.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}
