// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
	strictjson "sigs.k8s.io/json"
	strictyaml "sigs.k8s.io/yaml"
)

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	SHA256     string `json:"sha256"`
}

type ObjectIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

type Controller struct {
	Deployment     ObjectRef `json:"deployment"`
	ServiceAccount string    `json:"serviceAccount"`
}

type CleanupAttestation struct {
	ResourceUID   string `json:"resourceUID"`
	ControllerUID string `json:"controllerUID"`
	InvocationID  string `json:"invocationID,omitempty"`
	Evidence      string `json:"evidence"`
}

type Plan struct {
	Version                  int                  `json:"version"`
	Kubeconfig               string               `json:"kubeconfig"`
	Endpoint                 string               `json:"endpoint"`
	ClusterUID               string               `json:"clusterUID"`
	NamespaceUID             string               `json:"namespaceUID"`
	AuthorityNamespace       string               `json:"authorityNamespace"`
	AuthorityNamespaceUID    string               `json:"authorityNamespaceUID"`
	Release                  string               `json:"release"`
	ReleaseRevision          int                  `json:"releaseRevision"`
	Chart                    Artifact             `json:"chart"`
	Values                   Artifact             `json:"values"`
	Manifest                 Artifact             `json:"manifest"`
	PreviousManifest         *Artifact            `json:"previousManifest"`
	FailedRelease            *ObjectRef           `json:"failedRelease,omitempty"`
	PreviousDeployedRevision int                  `json:"previousDeployedRevision,omitempty"`
	PreviousDeployedManifest *Artifact            `json:"previousDeployedManifest,omitempty"`
	AbsentObjects            []ObjectIdentity     `json:"absentObjects,omitempty"`
	Controllers              []Controller         `json:"controllers"`
	Grants                   []ObjectRef          `json:"grants"`
	LegacyPolicies           []ObjectRef          `json:"legacyPolicies"`
	RetainedObjects          []ObjectRef          `json:"retainedObjects,omitempty"`
	CompletedMigrationJobs   []ObjectRef          `json:"completedMigrationJobs,omitempty"`
	CleanupAttestations      []CleanupAttestation `json:"cleanupAttestations,omitempty"`
	WorkloadNamespaceUIDs    map[string]string    `json:"workloadNamespaceUIDs"`
}

func ReadPlan(path string) (Plan, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	return ParsePlan(content, path)
}

func ParsePlan(content []byte, path string) (Plan, error) {
	var plan Plan
	strictErrors, err := strictjson.UnmarshalStrict(content, &plan)
	if err != nil {
		return plan, err
	}
	if len(strictErrors) != 0 {
		return plan, errors.Join(strictErrors...)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return plan, err
	}
	for _, name := range []string{"version", "kubeconfig", "endpoint", "clusterUID", "namespaceUID", "release", "releaseRevision", "chart", "values", "manifest", "controllers", "grants", "legacyPolicies", "workloadNamespaceUIDs"} {
		if value, found := fields[name]; !found || bytes.Equal(value, []byte("null")) {
			return plan, fmt.Errorf("plan requires explicit non-null %s", name)
		}
	}
	for _, name := range []string{"retainedObjects", "completedMigrationJobs", "cleanupAttestations", "absentObjects", "failedRelease", "previousDeployedRevision", "previousDeployedManifest"} {
		if value, found := fields[name]; found && bytes.Equal(value, []byte("null")) {
			return plan, fmt.Errorf("optional %s must not be null", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return plan, errors.New("plan must contain one JSON object")
	}
	if plan.Version != 1 || plan.Kubeconfig == "" || plan.Endpoint == "" || plan.ClusterUID == "" || plan.NamespaceUID == "" || plan.ReleaseRevision < 0 || plan.Release == "" || len(validation.IsDNS1123Subdomain(plan.Release)) != 0 {
		return plan, errors.New("version 1 plan requires explicit kubeconfig, endpoint, cluster and namespace UIDs, release and nonnegative revision")
	}
	endpoint, err := url.Parse(plan.Endpoint)
	if plan.AuthorityNamespaceUID == "" || len(validation.IsDNS1123Label(plan.AuthorityNamespace)) != 0 || plan.AuthorityNamespace == "" || plan.AuthorityNamespace == "kube-system" {
		return plan, errors.New("explicit administrator authority namespace and UID required")
	}
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return plan, errors.New("endpoint must be an explicit HTTPS Kubernetes API origin")
	}
	if (plan.ReleaseRevision > 0) != (plan.PreviousManifest != nil) {
		return plan, errors.New("upgrades require previousManifest; fresh installs must omit it")
	}
	if plan.FailedRelease == nil && (len(plan.AbsentObjects) != 0 || plan.PreviousDeployedRevision != 0 || plan.PreviousDeployedManifest != nil) {
		return plan, errors.New("partial release inventory is only valid for an explicitly reviewed failed release")
	}
	if plan.FailedRelease != nil && (plan.ReleaseRevision == 0 || plan.PreviousDeployedRevision < 0 || plan.PreviousDeployedRevision >= plan.ReleaseRevision || (plan.PreviousDeployedRevision > 0) != (plan.PreviousDeployedManifest != nil)) {
		return plan, errors.New("failed release requires a positive revision and exact earlier deployed manifest, if any")
	}
	if err := plan.validateInventory(); err != nil {
		return plan, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return plan, err
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(base, path)
	}
	plan.Kubeconfig = resolve(plan.Kubeconfig)
	artifacts := []*Artifact{&plan.Chart, &plan.Values, &plan.Manifest}
	if plan.PreviousManifest != nil {
		artifacts = append(artifacts, plan.PreviousManifest)
	}
	if plan.PreviousDeployedManifest != nil {
		artifacts = append(artifacts, plan.PreviousDeployedManifest)
	}
	for _, artifact := range artifacts {
		if artifact.Path == "" || len(artifact.SHA256) != 64 {
			return plan, errors.New("every artifact requires a path and SHA256")
		}
		artifact.Path = resolve(artifact.Path)
	}
	if !strings.HasSuffix(plan.Chart.Path, ".tgz") {
		return plan, errors.New("reviewed chart must be a packaged .tgz archive; dependency builds are not performed")
	}
	return plan, nil
}

func (plan Plan) validateInventory() error {
	references := append(append([]ObjectRef{}, plan.Grants...), plan.LegacyPolicies...)
	cleanup := map[string]bool{}
	for _, ref := range references {
		cleanup[ref.UID] = true
	}
	policies := map[string]bool{}
	for _, ref := range plan.LegacyPolicies {
		policies[ref.UID] = true
	}
	controllers := map[string]bool{}
	for _, controller := range plan.Controllers {
		controllers[controller.Deployment.UID] = true
		references = append(references, controller.Deployment)
	}
	references = append(references, plan.RetainedObjects...)
	references = append(references, plan.CompletedMigrationJobs...)
	if plan.FailedRelease != nil {
		references = append(references, *plan.FailedRelease)
	}
	identities, uids := map[string]bool{}, map[string]bool{}
	for _, ref := range references {
		version, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || version.Version == "" || ref.Kind == "" || ref.Name == "" || strings.TrimSpace(ref.UID) == "" || len(content.IsPathSegmentName(ref.Name)) != 0 || (ref.Namespace != "" && len(validation.IsDNS1123Label(ref.Namespace)) != 0) {
			return errors.New("inventory reference requires valid API version, kind, name, namespace and UID")
		}
		digest, err := hex.DecodeString(ref.SHA256)
		if err != nil || len(digest) != sha256.Size || ref.SHA256 != strings.ToLower(ref.SHA256) {
			return errors.New("inventory reference requires a lowercase SHA256 digest")
		}
		identity := releaseIdentity(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
		if identities[identity] || uids[ref.UID] {
			return errors.New("duplicate object or UID in installation inventory")
		}
		identities[identity], uids[ref.UID] = true, true
	}
	for _, ref := range plan.AbsentObjects {
		version, err := schema.ParseGroupVersion(ref.APIVersion)
		identity := releaseIdentity(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
		if err != nil || version.Version == "" || ref.Kind == "" || ref.Name == "" || len(content.IsPathSegmentName(ref.Name)) != 0 || (ref.Namespace != "" && len(validation.IsDNS1123Label(ref.Namespace)) != 0) || identities[identity] {
			return errors.New("absent release objects require unique valid identities disjoint from live inventory")
		}
		identities[identity] = true
	}
	attested := map[string]bool{}
	for _, attestation := range plan.CleanupAttestations {
		if !cleanup[attestation.ResourceUID] || !controllers[attestation.ControllerUID] || attested[attestation.ResourceUID] || strings.TrimSpace(attestation.Evidence) == "" || (policies[attestation.ResourceUID] && strings.TrimSpace(attestation.InvocationID) == "") {
			return errors.New("cleanup attestation requires unique inventoried resource, controller lineage, evidence and policy invocation")
		}
		attested[attestation.ResourceUID] = true
	}
	return nil
}

func ReadArtifact(artifact Artifact) ([]byte, error) {
	content, err := os.ReadFile(artifact.Path)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(content)) != artifact.SHA256 {
		return nil, fmt.Errorf("artifact digest mismatch: %s", artifact.Path)
	}
	return content, nil
}

func ValidateTransition(previous, expected []byte, plan Plan) error {
	oldObjects, err := Documents(previous)
	if err != nil {
		return err
	}
	newObjects, err := Documents(expected)
	if err != nil {
		return err
	}
	key := func(apiVersion, kind, namespace, name string) string {
		version, _ := schema.ParseGroupVersion(apiVersion)
		return version.Group + "/" + kind + "/" + namespace + "/" + name
	}
	retained := map[string]bool{}
	for _, object := range newObjects {
		retained[key(object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName())] = true
	}
	reviewed := map[string]bool{}
	references := append(append([]ObjectRef{}, plan.Grants...), plan.LegacyPolicies...)
	references = append(references, plan.CompletedMigrationJobs...)
	for _, controller := range plan.Controllers {
		references = append(references, controller.Deployment)
	}
	for _, ref := range references {
		if version, err := schema.ParseGroupVersion(ref.APIVersion); err != nil || version.Version == "" || ref.Kind == "" || ref.Name == "" {
			return errors.New("cleanup reference requires API version, kind and name")
		}
		reviewed[key(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)] = true
	}
	for _, object := range oldObjects {
		identity := key(object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName())
		if !retained[identity] && !reviewed[identity] {
			return fmt.Errorf("Helm would remove an object outside the reviewed cleanup inventory: %s", identity)
		}
	}
	return nil
}

func Documents(content []byte) ([]*unstructured.Unstructured, error) {
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(content)))
	var documents []*unstructured.Unstructured
	seen := map[string]bool{}
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, err
		}
		encoded, err := strictyaml.YAMLToJSONStrict(document)
		if err != nil {
			return nil, err
		}
		object := &unstructured.Unstructured{}
		strictErrors, err := strictjson.UnmarshalStrict(encoded, &object.Object)
		if err != nil {
			return nil, err
		}
		if len(strictErrors) > 0 {
			return nil, errors.Join(strictErrors...)
		}
		if len(object.Object) == 0 {
			continue
		}
		version, err := schema.ParseGroupVersion(object.GetAPIVersion())
		if err != nil || version.Version == "" || object.GetKind() == "" {
			return nil, errors.New("manifest resource requires API version and kind")
		}
		key := version.Group + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		if seen[key] || object.GetName() == "" || object.GetAnnotations()["helm.sh/hook"] != "" {
			return nil, fmt.Errorf("duplicate, unnamed or hook resource: %s", key)
		}
		seen[key] = true
		documents = append(documents, object)
	}
}

func CanonicalManifest(content []byte) ([]byte, error) {
	objects, err := Documents(content)
	if err != nil {
		return nil, err
	}
	encoded := make([]string, 0, len(objects))
	for _, object := range objects {
		data, err := json.Marshal(object.Object)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, string(data))
	}
	sort.Strings(encoded)
	return json.Marshal(encoded)
}

func ValidateRender(expected, actual []byte, release string) (activation.Manifest, error) {
	manifest, err := activation.Parse(bytes.NewReader(expected))
	if err != nil {
		return manifest, err
	}
	if _, err := activation.ParseCandidate(expected, release); err != nil {
		return manifest, err
	}
	left, err := CanonicalManifest(expected)
	if err != nil {
		return manifest, err
	}
	right, err := CanonicalManifest(actual)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(left, right) {
		return manifest, errors.New("Helm output differs from reviewed inactive manifest")
	}
	return manifest, nil
}

func ObjectDigest(object *unstructured.Unstructured) (string, error) {
	copy := object.DeepCopy()
	unstructured.RemoveNestedField(copy.Object, "status")
	for _, field := range []string{"resourceVersion", "managedFields"} {
		unstructured.RemoveNestedField(copy.Object, "metadata", field)
	}
	content, err := json.Marshal(copy.Object)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(content)), nil
}
