// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

func failedHelmRecord(secret *corev1.Secret, plan Plan, namespace string) (*corev1.Secret, error) {
	revision := strconv.Itoa(plan.ReleaseRevision + 1)
	if secret.Name != "sh.helm.release.v1."+plan.Release+".v"+revision || secret.Namespace != namespace || secret.Type != "helm.sh/release.v1" || secret.Labels["owner"] != "helm" || secret.Labels["name"] != plan.Release || secret.Labels["version"] != revision || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 || secret.DeletionTimestamp != nil {
		return nil, errors.New("Helm record identity or ownership differs from interrupted installation")
	}
	status := secret.Labels["status"]
	if status != "pending-install" && status != "pending-upgrade" {
		return nil, errors.New("only pending install or upgrade records can be settled; rollback is unsupported")
	}
	if (plan.ReleaseRevision == 0) != (status == "pending-install") {
		return nil, errors.New("pending Helm operation differs from reviewed installation")
	}
	compressed, err := base64.StdEncoding.DecodeString(string(secret.Data["release"]))
	if err != nil {
		return nil, errors.New("invalid Helm record encoding")
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, errors.New("invalid Helm record compression")
	}
	content, err := io.ReadAll(io.LimitReader(reader, 32*1024*1024+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil || len(content) > 32*1024*1024 {
		return nil, errors.New("invalid or oversized Helm record")
	}
	var release map[string]json.RawMessage
	violations, err := strictjson.UnmarshalStrict(content, &release)
	if err != nil || len(violations) != 0 {
		return nil, errors.New("invalid or ambiguous Helm record JSON")
	}
	var name, releaseNamespace, manifest string
	var version int
	var info map[string]json.RawMessage
	for _, field := range []struct {
		name  string
		value any
	}{{"name", &name}, {"namespace", &releaseNamespace}, {"version", &version}, {"manifest", &manifest}, {"info", &info}} {
		violations, err := strictjson.UnmarshalStrict(release[field.name], field.value)
		if err != nil || len(violations) != 0 {
			return nil, errors.New("incomplete Helm record")
		}
	}
	var storedStatus string
	if err := json.Unmarshal(info["status"], &storedStatus); err != nil || storedStatus != status || name != plan.Release || releaseNamespace != namespace || version != plan.ReleaseRevision+1 {
		return nil, errors.New("Helm record payload differs from reviewed identity or status")
	}
	expected, err := ReadArtifact(plan.Manifest)
	if err != nil {
		return nil, err
	}
	left, err := CanonicalManifest(expected)
	if err != nil {
		return nil, err
	}
	right, err := CanonicalManifest([]byte(manifest))
	if err != nil || !bytes.Equal(left, right) {
		return nil, errors.New("pending Helm manifest differs from reviewed inactive artifact")
	}
	info["status"] = json.RawMessage(`"failed"`)
	info["description"] = json.RawMessage(`"Settled by reviewed inactive installation recovery; no rollback or resource changes performed"`)
	release["info"], err = json.Marshal(info)
	if err != nil {
		return nil, err
	}
	content, err = json.Marshal(release)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	next := secret.DeepCopy()
	next.Labels["status"] = "failed"
	next.Data["release"] = []byte(base64.StdEncoding.EncodeToString(buffer.Bytes()))
	return next, nil
}

func (operations *Kubernetes) SettleHelm(ctx context.Context, uid, digest string) error {
	if operations.Journal == nil || !operations.Journal.AdmissionStarted() || uid == "" || len(digest) != 64 {
		return errors.New("Helm settlement requires an admission-stage journal and exact reviewed record UID and digest")
	}
	if err := operations.VerifyTarget(ctx); err != nil {
		return err
	}
	if err := operations.VerifyDrained(ctx); err != nil {
		return err
	}
	revision := operations.Plan.ReleaseRevision + 1
	ref := ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: operations.Manifest.Namespace, Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", operations.Plan.Release, revision), UID: uid, SHA256: digest}
	var records corev1.SecretList
	if err := operations.Admin.List(ctx, &records, client.InNamespace(ref.Namespace)); err != nil {
		return err
	}
	prefix := "sh.helm.release.v1." + operations.Plan.Release + ".v"
	for _, record := range records.Items {
		if !strings.HasPrefix(record.Name, prefix) {
			continue
		}
		version, err := strconv.Atoi(strings.TrimPrefix(record.Name, prefix))
		if err != nil || version > revision {
			return errors.New("Helm history advanced or is ambiguous; settlement refused")
		}
	}
	live, err := operations.reviewed(ctx, ref)
	if err != nil {
		intent, found, intentErr := operations.Journal.mutation("helm-failed", ref)
		if intentErr != nil {
			return intentErr
		}
		if !found {
			return err
		}
		ref.SHA256 = intent.ResultSHA256
		_, err = operations.reviewed(ctx, ref)
		return err
	}
	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object, &secret); err != nil {
		return err
	}
	next, err := failedHelmRecord(&secret, operations.Plan, ref.Namespace)
	if err != nil {
		return err
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(next)
	if err != nil {
		return err
	}
	result, err := ObjectDigest(&unstructured.Unstructured{Object: object})
	if err != nil {
		return err
	}
	if err := operations.Journal.prepareMutation("helm-failed", ref, result); err != nil {
		return err
	}
	return operations.Admin.Patch(ctx, next, client.MergeFromWithOptions(&secret, client.MergeFromWithOptimisticLock{}))
}

func (operations *Kubernetes) VerifyFailedRelease(ctx context.Context) error {
	ref := operations.Plan.FailedRelease
	if ref == nil {
		return nil
	}
	if ref.APIVersion != "v1" || ref.Kind != "Secret" || ref.Namespace != operations.Manifest.Namespace || ref.Name != fmt.Sprintf("sh.helm.release.v1.%s.v%d", operations.Plan.Release, operations.Plan.ReleaseRevision) {
		return errors.New("failed release reference differs from reviewed Helm history")
	}
	object, err := operations.reviewed(ctx, *ref)
	if err != nil {
		return err
	}
	labels := object.GetLabels()
	recordType, _, err := unstructured.NestedString(object.Object, "type")
	if err != nil || recordType != "helm.sh/release.v1" || labels["status"] != "failed" || labels["owner"] != "helm" || labels["name"] != operations.Plan.Release || labels["version"] != strconv.Itoa(operations.Plan.ReleaseRevision) || len(object.GetOwnerReferences()) != 0 {
		return errors.New("reviewed Helm history is not an unowned failed release record")
	}
	return nil
}
