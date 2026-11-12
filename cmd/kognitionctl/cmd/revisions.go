// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var revisionsCmd = &cobra.Command{
	Use:   "revisions <resource> <name>",
	Short: "List revisions of a resource",
	Long:  `Show the revision history for a specific resource.`,
	Args:  cobra.ExactArgs(2),
	RunE:  runRevisions,
}

var getRevisionCmd = &cobra.Command{
	Use:   "revision <resource> <name> <revision>",
	Short: "Get a specific revision of a resource",
	Args:  cobra.ExactArgs(3),
	RunE:  runGetRevision,
}

var diffRevisionsCmd = &cobra.Command{
	Use:   "revision-diff <resource> <name> <from-revision> <to-revision>",
	Short: "Show the configuration diff between two revisions",
	Args:  cobra.ExactArgs(4),
	RunE:  runDiffRevisions,
}

func init() {
	rootCmd.AddCommand(revisionsCmd)
	rootCmd.AddCommand(getRevisionCmd)
	rootCmd.AddCommand(diffRevisionsCmd)
}

func runRevisions(_ *cobra.Command, args []string) error {
	resource := args[0]
	name := args[1]

	var body []byte
	var err error
	if k8sMode {
		body, err = k8sListRevisions(namespace, resource, name)
	} else {
		url := fmt.Sprintf("%s/revisions", apiURLName(namespace, resource, name))
		body, err = doGet(url)
	}
	if err != nil {
		return err
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(body)
	}

	// Table output for revisions list
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return printOutput(body)
	}

	fmt.Printf("REVISION\tRESOURCE VERSION\tTIMESTAMP\n")
	for _, item := range list.Items {
		var rev map[string]any
		if err := json.Unmarshal(item, &rev); err != nil {
			continue
		}
		meta, _ := rev["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		rv := rev["revision"]
		if rv == nil {
			rv = meta["resourceVersion"]
		}
		gen := rev["revision"]
		if gen == nil {
			gen = meta["generation"]
		}
		ts, _ := rev["createdAt"].(string)
		if ts == "" {
			ts, _ = meta["createdAt"].(string)
		}
		if ts == "" {
			ts, _ = meta["created_at"].(string)
		}
		fmt.Printf("%v\t%v\t%s\n", gen, rv, ts)
	}
	return nil
}

func runGetRevision(_ *cobra.Command, args []string) error {
	resource := args[0]
	name := args[1]
	revision := args[2]

	var body []byte
	var err error
	if k8sMode {
		body, err = k8sGetRevision(namespace, resource, name, revision)
	} else {
		url := fmt.Sprintf("%s/revisions/%s", apiURLName(namespace, resource, name), revision)
		body, err = doGet(url)
	}
	if err != nil {
		return err
	}
	return printOutput(body)
}

func runDiffRevisions(_ *cobra.Command, args []string) error {
	resource := args[0]
	name := args[1]
	from := args[2]
	to := args[3]

	var body []byte
	var err error
	if k8sMode {
		body, err = k8sDiffRevisions(namespace, resource, name, from, to)
	} else {
		url := fmt.Sprintf("%s/revisions/diff?from=%s&to=%s", apiURLName(namespace, resource, name), from, to)
		body, err = doGet(url)
	}
	if err != nil {
		return err
	}
	return printOutput(body)
}

type revisionSnapshot struct {
	Revision  int64  `json:"revision"`
	Spec      []byte `json:"spec"`
	Data      []byte `json:"data"`
	CreatedAt string `json:"createdAt"`
}

type revisionParent struct {
	UID         string
	Annotations map[string]string
}

func k8sListRevisions(namespace, resource, name string) ([]byte, error) {
	if k8sCoreClient == nil {
		return nil, fmt.Errorf("kube client not initialized")
	}
	kind := kindFor(resource)
	if kind == "" {
		return nil, fmt.Errorf("unknown resource %q", resource)
	}
	parent, err := k8sRevisionParent(namespace, resource, name)
	if err != nil {
		return nil, err
	}
	labels := fmt.Sprintf("kognition.dev/revision-parent=%s,kognition.dev/revision-kind=%s", revisionParentKey(kind, name), strings.ToLower(kind))
	list, err := k8sCoreClient.CoreV1().ConfigMaps(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: labels})
	if err != nil {
		return nil, fmt.Errorf("list %s revisions: %w", kind, err)
	}
	revisions := make([]revisionSnapshot, 0, len(list.Items))
	for index := range list.Items {
		snapshot, ok := revisionSnapshotFromConfigMap(&list.Items[index], kind, name, parent.UID)
		if ok {
			snapshot.Data = sanitizeRevisionSnapshotData(kind, snapshot.Data)
			revisions = append(revisions, snapshot)
		}
	}
	if legacy, ok := legacyRevisionSnapshot(parent.Annotations); ok && !containsRevisionSnapshot(revisions, legacy.Revision) {
		legacy.Data = sanitizeRevisionSnapshotData(kind, legacy.Data)
		revisions = append(revisions, legacy)
	}
	sort.Slice(revisions, func(left, right int) bool { return revisions[left].Revision < revisions[right].Revision })
	return json.Marshal(map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "RevisionList",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"items":      revisions,
	})
}

func k8sGetRevision(namespace, resource, name, revision string) ([]byte, error) {
	snapshot, kind, err := k8sRevisionSnapshot(namespace, resource, name, revision)
	if err != nil {
		return nil, err
	}
	snapshot.Data = sanitizeRevisionSnapshotData(kind, snapshot.Data)
	return json.Marshal(map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Revision",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
			"revision":  snapshot.Revision,
			"createdAt": snapshot.CreatedAt,
		},
		"spec":     json.RawMessage(snapshot.Spec),
		"data":     json.RawMessage(snapshot.Data),
		"resource": kind,
	})
}

func k8sDiffRevisions(namespace, resource, name, from, to string) ([]byte, error) {
	fromSnapshot, _, err := k8sRevisionSnapshot(namespace, resource, name, from)
	if err != nil {
		return nil, err
	}
	toSnapshot, _, err := k8sRevisionSnapshot(namespace, resource, name, to)
	if err != nil {
		return nil, err
	}
	if fromSnapshot.Revision == toSnapshot.Revision {
		return nil, fmt.Errorf("from and to must name different revisions")
	}
	patch, err := jsonpatch.CreatePatch(fromSnapshot.Spec, toSnapshot.Spec)
	if err != nil {
		return nil, fmt.Errorf("create revision diff: %w", err)
	}
	return json.Marshal(map[string]any{
		"apiVersion":   "kognition.dev/v1alpha1",
		"kind":         "RevisionDiff",
		"metadata":     map[string]any{"namespace": namespace, "name": name, "resource": pluralize(resource)},
		"fromRevision": fromSnapshot.Revision,
		"toRevision":   toSnapshot.Revision,
		"patch":        patch,
	})
}

func k8sRevisionSnapshot(namespace, resource, name, revision string) (revisionSnapshot, string, error) {
	if k8sCoreClient == nil {
		return revisionSnapshot{}, "", fmt.Errorf("kube client not initialized")
	}
	kind := kindFor(resource)
	if kind == "" {
		return revisionSnapshot{}, "", fmt.Errorf("unknown resource %q", resource)
	}
	revisionNumber, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || revisionNumber < 1 {
		return revisionSnapshot{}, "", fmt.Errorf("revision must be a positive integer")
	}
	parent, err := k8sRevisionParent(namespace, resource, name)
	if err != nil {
		return revisionSnapshot{}, "", err
	}
	configMap, err := k8sCoreClient.CoreV1().ConfigMaps(namespace).Get(context.Background(), revisionConfigMapName(kind, name, revisionNumber), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return revisionSnapshot{}, "", fmt.Errorf("get revision %d for %s %q: %w", revisionNumber, kind, name, err)
	}
	if err == nil {
		snapshot, ok := revisionSnapshotFromConfigMap(configMap, kind, name, parent.UID)
		if ok && snapshot.Revision == revisionNumber {
			return snapshot, kind, nil
		}
	}
	if legacy, ok := legacyRevisionSnapshot(parent.Annotations); ok && legacy.Revision == revisionNumber {
		return legacy, kind, nil
	}
	return revisionSnapshot{}, "", fmt.Errorf("revision %d not found for %s %q", revisionNumber, kind, name)
}

func k8sRevisionParent(namespace, resource, name string) (revisionParent, error) {
	parent, err := k8sGet(namespace, resource, name)
	if err != nil {
		return revisionParent{}, fmt.Errorf("get revision parent %s/%s: %w", resource, name, err)
	}
	var metadata struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(parent, &metadata); err != nil {
		return revisionParent{}, fmt.Errorf("decode revision parent %s/%s: %w", resource, name, err)
	}
	if metadata.Metadata.UID == "" {
		return revisionParent{}, fmt.Errorf("revision parent %s/%s has no UID", resource, name)
	}
	return revisionParent{UID: metadata.Metadata.UID, Annotations: metadata.Metadata.Annotations}, nil
}

func legacyRevisionSnapshot(annotations map[string]string) (revisionSnapshot, bool) {
	revision, err := strconv.ParseInt(annotations["kognition.dev/revision-count"], 10, 64)
	if err != nil || revision < 1 {
		return revisionSnapshot{}, false
	}
	spec, ok := annotations["kognition.dev/revision-spec"]
	if !ok {
		return revisionSnapshot{}, false
	}
	specBytes, err := base64.StdEncoding.DecodeString(spec)
	if err != nil {
		return revisionSnapshot{}, false
	}
	var dataBytes []byte
	if data, ok := annotations["kognition.dev/revision-data"]; ok {
		dataBytes, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			return revisionSnapshot{}, false
		}
	}
	return revisionSnapshot{Revision: revision, Spec: specBytes, Data: dataBytes}, true
}

func containsRevisionSnapshot(revisions []revisionSnapshot, revision int64) bool {
	for _, candidate := range revisions {
		if candidate.Revision == revision {
			return true
		}
	}
	return false
}

func revisionSnapshotFromConfigMap(configMap *corev1.ConfigMap, kind, name, ownerUID string) (revisionSnapshot, bool) {
	annotations := configMap.Annotations
	if configMap.Immutable == nil || !*configMap.Immutable ||
		configMap.Labels["kognition.dev/revision-parent"] != revisionParentKey(kind, name) ||
		configMap.Labels["kognition.dev/revision-kind"] != strings.ToLower(kind) ||
		!hasRevisionOwner(configMap, kind, name, ownerUID) {
		return revisionSnapshot{}, false
	}
	revision, err := strconv.ParseInt(annotations["kognition.dev/revision"], 10, 64)
	if err != nil || revision < 1 {
		return revisionSnapshot{}, false
	}
	createdAt := annotations["kognition.dev/created-at"]
	if createdAt == "" && !configMap.CreationTimestamp.IsZero() {
		createdAt = configMap.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	return revisionSnapshot{
		Revision:  revision,
		Spec:      append([]byte(nil), configMap.BinaryData["spec"]...),
		Data:      append([]byte(nil), configMap.BinaryData["data"]...),
		CreatedAt: createdAt,
	}, true
}

func hasRevisionOwner(configMap *corev1.ConfigMap, kind, name, ownerUID string) bool {
	for _, owner := range configMap.OwnerReferences {
		if owner.APIVersion == "kognition.dev/v1alpha1" && owner.Kind == kind && owner.Name == name && string(owner.UID) == ownerUID {
			return true
		}
	}
	return false
}

func sanitizeRevisionSnapshotData(kind string, data []byte) []byte {
	if kind != "SecretRef" || len(data) == 0 {
		return data
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return data
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		return data
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	if !ok {
		return data
	}
	if _, ok := annotations["kognition.dev/internal-secret-data"]; !ok {
		return data
	}
	delete(annotations, "kognition.dev/internal-secret-data")
	if len(annotations) == 0 {
		delete(metadata, "annotations")
	} else {
		metadata["annotations"] = annotations
	}
	object["metadata"] = metadata
	sanitized, err := json.Marshal(object)
	if err != nil {
		return data
	}
	return sanitized
}

func revisionParentKey(kind, name string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + name))
	return fmt.Sprintf("%x", digest[:12])
}

func revisionConfigMapName(kind, name string, revision int64) string {
	prefix := strings.ToLower(kind + "-" + name)
	prefix = strings.Trim(strings.ReplaceAll(prefix, "_", "-"), "-.")
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return fmt.Sprintf("kognition-revision-%s-%s-%d", prefix, revisionParentKey(kind, name)[:12], revision)
}
