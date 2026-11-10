// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Verify the client-side three-way merge used by Helm without contacting a cluster.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

type identity struct {
	APIVersion, Kind, Namespace, Name string
}

func objectID(obj unstructured.Unstructured) identity {
	return identity{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName()}
}

func readObjects(path string, jsonList bool) (map[identity]unstructured.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var objects []unstructured.Unstructured
	if jsonList {
		if err := json.Unmarshal(data, &objects); err != nil {
			return nil, err
		}
	} else {
		decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var obj unstructured.Unstructured
			if err := decoder.Decode(&obj.Object); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			if len(obj.Object) > 0 {
				objects = append(objects, obj)
			}
		}
	}
	result := make(map[identity]unstructured.Unstructured, len(objects))
	for _, obj := range objects {
		id := objectID(obj)
		if _, exists := result[id]; exists || id.APIVersion == "" || id.Kind == "" || id.Name == "" {
			return nil, fmt.Errorf("invalid or duplicate resource identity: %v", id)
		}
		result[id] = obj
	}
	return result, nil
}

func normalizedJSON(obj unstructured.Unstructured) ([]byte, error) {
	copy := obj.DeepCopy()
	delete(copy.Object, "status")
	for _, field := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink"} {
		unstructured.RemoveNestedField(copy.Object, "metadata", field)
	}
	return json.Marshal(copy)
}

func verifyExisting(old, current, desired unstructured.Unstructured) error {
	oldData, err := json.Marshal(old.Object)
	if err != nil {
		return err
	}
	newData, err := json.Marshal(desired.Object)
	if err != nil {
		return err
	}
	currentData, err := json.Marshal(current.Object)
	if err != nil {
		return err
	}
	gv, err := schema.ParseGroupVersion(desired.GetAPIVersion())
	if err != nil {
		return err
	}
	versioned, err := scheme.Scheme.New(gv.WithKind(desired.GetKind()))
	var nextData []byte
	if desired.GetAPIVersion() == "spire.spiffe.io/v1alpha1" && desired.GetKind() == "ClusterStaticEntry" {
		// Helm's client-side upgrade uses two-way JSON merge for CRs.
		patch, err := jsonpatch.CreateMergePatch(oldData, newData)
		if err != nil {
			return err
		}
		nextData, err = jsonpatch.MergePatch(currentData, patch)
		if err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("only existing built-in release resources are supported: %w", err)
	} else {
		meta, err := strategicpatch.NewPatchMetaFromStruct(versioned)
		if err != nil {
			return err
		}
		patch, err := strategicpatch.CreateThreeWayMergePatch(oldData, newData, currentData, meta, true)
		if err != nil {
			return fmt.Errorf("construct Helm three-way patch: %w", err)
		}
		nextData, err = strategicpatch.StrategicMergePatch(currentData, patch, versioned)
		if err != nil {
			return fmt.Errorf("simulate Helm three-way patch: %w", err)
		}
	}
	var next unstructured.Unstructured
	if err := json.Unmarshal(nextData, &next); err != nil {
		return err
	}
	before, err := normalizedJSON(current)
	if err != nil {
		return err
	}
	after, err := normalizedJSON(next)
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return fmt.Errorf("Helm would change an existing resource outside bootstrap ownership")
	}
	return nil
}

func verify(previousPath, livePath, targetPath, snapshotPath string, recovery bool) error {
	previous, err := readObjects(previousPath, false)
	if err != nil {
		return err
	}
	live, err := readObjects(livePath, true)
	if err != nil {
		return err
	}
	target, err := readObjects(targetPath, false)
	if err != nil {
		return err
	}
	if len(previous) != len(live) {
		return fmt.Errorf("recorded release and live resource inventories differ")
	}
	for id, old := range previous {
		current, found := live[id]
		if !found {
			return fmt.Errorf("recorded resource is missing: %v", id)
		}
		desired, found := target[id]
		if !found {
			return fmt.Errorf("target would remove a recorded resource: %v", id)
		}
		if err := verifyExisting(old, current, desired); err != nil {
			return fmt.Errorf("%v: %w", id, err)
		}
	}
	var snapshot struct {
		Bootstraps map[string]struct {
			Name, UID           string
			Labels, Annotations map[string]string
			Spec                map[string]any
		}
	}
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	roles := []string{"agent-alias", "ztunnel"}
	if recovery {
		roles = append(roles, "cilium-agent", "operator")
	}
	names := make([]string, 0, len(roles))
	added := 0
	for _, role := range roles {
		saved, found := snapshot.Bootstraps[role]
		if !found || saved.UID == "" || saved.Labels == nil || saved.Annotations == nil {
			return fmt.Errorf("missing saved bootstrap identity: %s", role)
		}
		id := identity{"spire.spiffe.io/v1alpha1", "ClusterStaticEntry", "", saved.Name}
		desired, found := target[id]
		if !found || !reflect.DeepEqual(desired.Object["spec"], saved.Spec) {
			return fmt.Errorf("bootstrap name or spec changed: %s", role)
		}
		saved.Labels["app.kubernetes.io/managed-by"] = "Helm"
		saved.Annotations["meta.helm.sh/release-name"] = "cilium"
		saved.Annotations["meta.helm.sh/release-namespace"] = "kube-system"
		if !reflect.DeepEqual(desired.GetLabels(), saved.Labels) || !reflect.DeepEqual(desired.GetAnnotations(), saved.Annotations) {
			return fmt.Errorf("bootstrap ownership differs from the guarded adoption: %s", role)
		}
		if _, exists := previous[id]; !exists {
			added++
		}
		names = append(names, saved.Name)
	}
	if len(target) != len(previous)+added {
		return fmt.Errorf("unexpected additional resources in target")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"existingResourcesVerified": len(previous), "bootstrapOwnershipTargets": names,
		"predictedExistingResourceMutations": 0, "clusterContacted": false,
	})
}

func main() {
	previous := flag.String("previous", "", "Saved revision 6 manifest")
	live := flag.String("live", "", "Saved JSON array of live release resources")
	target := flag.String("target", "", "Offline-rendered migration manifest")
	snapshot := flag.String("snapshot", "", "Protected four-bootstrap snapshot")
	recovery := flag.Bool("recovery", false, "Expect four recovery bootstraps instead of two")
	flag.Parse()
	if err := verify(*previous, *live, *target, *snapshot, *recovery); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
