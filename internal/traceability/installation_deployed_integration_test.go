// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeployedInstallationCLI(test *testing.T) {
	if os.Getenv("KOGNITION_INSTALL_DEPLOYED_ACCEPTANCE") != "true" {
		test.Skip("explicit KOGNITION_INSTALL_DEPLOYED_ACCEPTANCE=true required")
	}
	test.Run("fresh", func(test *testing.T) { deployedInstallationCLI(test, false, "") })
	test.Run("legacy", func(test *testing.T) { deployedInstallationCLI(test, true, "") })
	test.Run("failed-install", func(test *testing.T) { deployedInstallationCLI(test, false, "quota") })
	test.Run("failed-upgrade", func(test *testing.T) { deployedInstallationCLI(test, true, "quota") })
	test.Run("certificate-recovery", func(test *testing.T) { deployedInstallationCLI(test, false, "certificate") })
	test.Run("interrupted-verification", func(test *testing.T) { deployedInstallationCLI(test, false, "interrupted") })
}

func validateInstallationNode(nodes []corev1.Node, expected string) error {
	if expected == "" || len(nodes) != 1 || nodes[0].Name != expected {
		return fmt.Errorf("exactly one explicitly selected task-owned node is required")
	}
	return nil
}

func validateInstallationCluster(nodes []corev1.Node, expectedNode string, root *corev1.Namespace, expectedUID, runID string) error {
	if err := validateInstallationNode(nodes, expectedNode); err != nil {
		return err
	}
	identity, err := uuid.Parse(runID)
	if err != nil || identity == uuid.Nil || identity.String() != runID {
		return fmt.Errorf("canonical nonzero acceptance run UUID required")
	}
	if expectedNode != "kognition-"+runID+"-control-plane" || nodes[0].Labels["kognition.dev/acceptance-run"] != runID {
		return fmt.Errorf("fresh acceptance node name and ownership label must match the run UUID")
	}
	if root == nil || root.Name != "kube-system" || expectedUID == "" || string(root.UID) != expectedUID {
		return fmt.Errorf("recorded kube-system UID must match the selected cluster")
	}
	return nil
}

func deployedInstallationCLI(test *testing.T, legacy bool, failure string) {
	kubeconfig := os.Getenv("KOGNITION_ADMISSION_KUBECONFIG")
	image := os.Getenv("KOGNITION_ADMISSION_GUARD_IMAGE")
	separator := strings.LastIndex(image, ":")
	if kubeconfig == "" || separator <= strings.LastIndex(image, "/") || separator == len(image)-1 || strings.Contains(image, "@") {
		test.Fatal("explicit isolated kubeconfig and tagged preloaded guard image required")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.HasPrefix(config.Host, "https://127.0.0.1:") || os.Getenv("KOGNITION_ADMISSION_ISOLATED") != "true" {
		test.Fatal("isolated loopback API endpoint required")
	}
	config.Timeout = 15 * time.Second
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	nodes := &corev1.NodeList{}
	if err := admin.List(ctx, nodes); err != nil {
		test.Fatal(err)
	}
	root := &corev1.Namespace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, root); err != nil {
		test.Fatal(err)
	}
	if err := validateInstallationCluster(nodes.Items, os.Getenv("KOGNITION_ADMISSION_EXPECTED_NODE"), root, os.Getenv("KOGNITION_ADMISSION_EXPECTED_CLUSTER_UID"), os.Getenv("KOGNITION_ADMISSION_RUN_ID")); err != nil {
		test.Fatal(err)
	}
	version := nodes.Items[0].Status.NodeInfo.KubeletVersion
	if expected := os.Getenv("KOGNITION_ADMISSION_EXPECTED_VERSION"); expected == "" || expected != version {
		test.Fatalf("reviewed Kubernetes version mismatch: %q, actual %q", expected, version)
	}
	directory := test.TempDir()
	executable := filepath.Join(directory, "kognition-install.exe")
	run := func(program string, arguments ...string) []byte {
		test.Helper()
		command := exec.CommandContext(ctx, program, arguments...)
		output, err := command.CombinedOutput()
		if err != nil {
			test.Fatalf("%s %v: %v: %s", program, arguments, err, output)
		}
		return output
	}
	run("go", "build", "-o", executable, "../../tools/kognition-install")
	run("helm", "package", "../../deploy/helm/kognition", "--destination", directory)
	archives, err := filepath.Glob(filepath.Join(directory, "*.tgz"))
	if err != nil || len(archives) != 1 {
		test.Fatalf("one temporary chart archive required: %v, %v", archives, err)
	}
	suffix := uuid.NewString()[:8]
	release, control, workload, authority := "sg05-"+suffix, "sg05-control-"+suffix, "sg05-work-"+suffix, "sg05-authority-"+suffix
	var namespaces []*corev1.Namespace
	var documents []*unstructured.Unstructured
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		for index := len(documents) - 1; index >= 0; index-- {
			object := documents[index].DeepCopy()
			if err := admin.Get(cleanup, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				test.Error(err)
				continue
			}
			if object.GetAnnotations()["meta.helm.sh/release-name"] != release || object.GetAnnotations()["meta.helm.sh/release-namespace"] != control {
				test.Errorf("refusing cleanup of non-fixture object %s/%s", object.GetKind(), object.GetName())
				continue
			}
			if err := admin.Delete(cleanup, object, client.Preconditions{UID: ptr.To(object.GetUID())}); client.IgnoreNotFound(err) != nil {
				test.Error(err)
			}
		}
		for _, namespace := range namespaces {
			if err := admin.Delete(cleanup, namespace, client.Preconditions{UID: ptr.To(namespace.UID)}); client.IgnoreNotFound(err) != nil {
				test.Error(err)
				continue
			}
			if err := wait.PollUntilContextTimeout(cleanup, 200*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
				current := &corev1.Namespace{}
				err := admin.Get(ctx, client.ObjectKey{Name: namespace.Name}, current)
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			}); err != nil {
				test.Errorf("namespace cleanup %s: %v", namespace.Name, err)
			}
		}
	})
	for _, name := range []string{control, workload, authority} {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := admin.Create(ctx, namespace); err != nil {
			test.Fatal(err)
		}
		namespaces = append(namespaces, namespace)
	}
	guard := release + "-admission-guard"
	certificate, key := admissionServingCertificate(test, guard+"."+control+".svc")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: guard + "-tls", Namespace: control}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": certificate, "tls.key": key}}
	if err := admin.Create(ctx, secret); err != nil {
		test.Fatal(err)
	}
	writeArtifact := func(name string, content []byte) installation.Artifact {
		test.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, content, 0600); err != nil {
			test.Fatal(err)
		}
		return installation.Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}
	}
	values, err := json.Marshal(map[string]any{
		"mode": "kubernetes", "fullnameOverride": release,
		"memoryService":  map[string]any{"enabled": false},
		"toolGateway":    map[string]any{"enabled": false},
		"webhook":        map[string]any{"certManager": map[string]any{"enabled": false}},
		"networkPolicy":  map[string]any{"kubeletProbeIngressCIDRs": []string{"172.16.0.0/12"}},
		"k8sController":  map[string]any{"activated": true, "managedNamespaces": []string{workload}, "image": map[string]any{"repository": image[:separator], "tag": image[separator+1:], "pullPolicy": "Never"}},
		"admissionGuard": map[string]any{"tls": map[string]any{"existingSecret": secret.Name, "caBundle": base64.StdEncoding.EncodeToString(certificate)}},
	})
	if err != nil {
		test.Fatal(err)
	}
	valuesArtifact := writeArtifact("values.json", values)
	manifestBytes := run("helm", "template", release, archives[0], "--namespace", control, "--kube-version", version, "--no-hooks", "--values", valuesArtifact.Path, "--set", "k8sController.activated=false")
	manifest, err := activation.Parse(bytes.NewReader(manifestBytes))
	if err != nil {
		test.Fatal(err)
	}
	documents, err = installation.Documents(manifestBytes)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range documents {
		mapping, err := admin.RESTMapper().RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
		if err != nil {
			test.Fatal(err)
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace && object.GetNamespace() == "" {
			object.SetNamespace(control)
		}
	}
	chart, err := os.ReadFile(archives[0])
	if err != nil {
		test.Fatal(err)
	}
	plan := installation.Plan{Version: 1, Kubeconfig: kubeconfig, Endpoint: config.Host, ClusterUID: string(root.UID), NamespaceUID: string(namespaces[0].UID), AuthorityNamespace: authority, AuthorityNamespaceUID: string(namespaces[2].UID), Release: release, Chart: installation.Artifact{Path: archives[0], SHA256: fmt.Sprintf("%x", sha256.Sum256(chart))}, Values: valuesArtifact, Manifest: writeArtifact("manifest.yaml", manifestBytes), Controllers: []installation.Controller{}, Grants: []installation.ObjectRef{}, LegacyPolicies: []installation.ObjectRef{}, WorkloadNamespaceUIDs: map[string]string{workload: string(namespaces[1].UID)}}
	if legacy {
		streamImage := os.Getenv("KOGNITION_ADMISSION_STREAM_IMAGE")
		if streamImage == "" {
			test.Fatal("explicit preloaded KOGNITION_ADMISSION_STREAM_IMAGE required for legacy controller fixture")
		}
		run("helm", "install", release, "testdata/sg05-legacy", "--kubeconfig", kubeconfig, "--namespace", control, "--set", "fixture.image="+streamImage, "--set", "k8sController.activated=true", "--wait", "--timeout", "90s")
		previousBytes := run("helm", "get", "manifest", release, "--kubeconfig", kubeconfig, "--namespace", control)
		previous := writeArtifact("legacy.yaml", previousBytes)
		plan.ReleaseRevision, plan.PreviousManifest = 1, &previous
		oldObjects, err := installation.Documents(previousBytes)
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range oldObjects {
			if err := admin.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				test.Fatal(err)
			}
			digest, err := installation.ObjectDigest(object)
			if err != nil {
				test.Fatal(err)
			}
			reference := installation.ObjectRef{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), SHA256: digest}
			switch object.GetKind() {
			case "Deployment":
				plan.Controllers = append(plan.Controllers, installation.Controller{Deployment: reference, ServiceAccount: release + "-controller"})
			case "RoleBinding":
				plan.Grants = append(plan.Grants, reference)
			default:
				plan.RetainedObjects = append(plan.RetainedObjects, reference)
			}
		}
		test.Cleanup(func() {
			documents = append(documents, oldObjects...)
		})
		plan.Manifest = writeArtifact("legacy-upgrade.yaml", run("helm", "template", release, archives[0], "--namespace", control, "--kube-version", version, "--no-hooks", "--is-upgrade", "--values", valuesArtifact.Path, "--set", "k8sController.activated=false"))
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		test.Fatal(err)
	}
	planPath := writeArtifact("reviewed plan.json", planBytes).Path
	test.Logf("node=%s version=%s endpoint=%s release=%s image=%s", nodes.Items[0].Name, version, config.Host, release, image)
	if legacy {
		held := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "held-invocation", Namespace: workload, Finalizers: []string{"sg05.kognition.dev/held"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fixture", Image: os.Getenv("KOGNITION_ADMISSION_STREAM_IMAGE"), ImagePullPolicy: corev1.PullNever, Command: []string{"sh", "-c", "sleep 600"}}}}}
		if err := admin.Create(ctx, held); err != nil {
			test.Fatal(err)
		}
		releaseHeld := func(ctx context.Context) error {
			return retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &corev1.Pod{}
				if err := admin.Get(ctx, client.ObjectKeyFromObject(held), current); err != nil {
					return client.IgnoreNotFound(err)
				}
				if current.UID != held.UID {
					return fmt.Errorf("refusing cleanup of replacement held Pod %s", current.UID)
				}
				current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == "sg05.kognition.dev/held" })
				if err := admin.Update(ctx, current); err != nil {
					return err
				}
				return client.IgnoreNotFound(admin.Delete(ctx, current, client.Preconditions{UID: ptr.To(held.UID)}, client.GracePeriodSeconds(0)))
			})
		}
		test.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := releaseHeld(cleanup); err != nil {
				test.Error(err)
			}
		})
		output, err := exec.CommandContext(ctx, executable, "bootstrap", "--plan", planPath, "--writers-frozen").CombinedOutput()
		if err == nil || !bytes.Contains(output, []byte("workloads-drained:")) {
			test.Fatalf("held invocation must block before controller shutdown: %v: %s", err, output)
		}
		oldController := &appsv1.Deployment{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: control, Name: release + "-k8s-controller"}, oldController); err != nil || oldController.Spec.Replicas == nil || *oldController.Spec.Replicas != 1 || oldController.Status.AvailableReplicas != 1 {
			test.Fatalf("drain failure stopped legacy controller: %v", err)
		}
		grant := &unstructured.Unstructured{}
		grant.SetAPIVersion("rbac.authorization.k8s.io/v1")
		grant.SetKind("RoleBinding")
		if err := admin.Get(ctx, client.ObjectKey{Namespace: control, Name: release + "-legacy-controller"}, grant); err != nil {
			test.Fatalf("drain failure revoked finalization grant: %v", err)
		}
		if err := releaseHeld(ctx); err != nil {
			test.Fatal(err)
		}
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			err := admin.Get(ctx, client.ObjectKeyFromObject(held), &corev1.Pod{})
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}); err != nil {
			test.Fatal(err)
		}
		failedClaim := &corev1.ConfigMap{}
		if err := admin.Get(ctx, installation.JournalKey(plan), failedClaim); err != nil {
			test.Fatal(err)
		}
		test.Logf("drain rejection: %s", output)
		test.Logf("pre-mutation recovery: %s", run(executable, "recover", "--plan", planPath, "--writers-frozen", "--executors-stopped", "--requests-settled", "--claim-uid", string(failedClaim.UID)))
	}
	if failure == "quota" {
		limit := "0"
		if legacy {
			limit = "1"
		}
		quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "admission-fault", Namespace: control}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{"count/deployments.apps": resource.MustParse(limit)}}}
		if err := admin.Create(ctx, quota); err != nil {
			test.Fatal(err)
		}
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			current := &corev1.ResourceQuota{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(quota), current); err != nil {
				return false, err
			}
			_, ready := current.Status.Hard["count/deployments.apps"]
			return ready, nil
		}); err != nil {
			test.Fatal(err)
		}
		output, err := exec.CommandContext(ctx, executable, "bootstrap", "--plan", planPath, "--writers-frozen").CombinedOutput()
		if err == nil || !bytes.Contains(output, []byte("admission-bootstrap:")) || !bytes.Contains(output, []byte("exceeded quota")) {
			test.Fatalf("expected quota failure during real Helm apply: %v: %s", err, output)
		}
		test.Logf("admission failure: %s", output)
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatalf("failed Helm apply restored producers or grants: %v", err)
		}
		failedClaim := &corev1.ConfigMap{}
		if err := admin.Get(ctx, installation.JournalKey(plan), failedClaim); err != nil || !strings.Contains(failedClaim.Data["events"], "admission-bootstrap:failed") {
			test.Fatalf("missing failed admission journal: %v: %v", err, failedClaim.Data)
		}
		test.Logf("failed admission recovery: %s", run(executable, "recover", "--plan", planPath, "--writers-frozen", "--executors-stopped", "--requests-settled", "--claim-uid", string(failedClaim.UID)))
		if err := admin.Delete(ctx, quota, client.Preconditions{UID: ptr.To(quota.UID)}); err != nil {
			test.Fatal(err)
		}
		priorObjects := []*unstructured.Unstructured{}
		if legacy {
			plan.PreviousDeployedRevision, plan.PreviousDeployedManifest = plan.ReleaseRevision, plan.PreviousManifest
			previousBytes, err := installation.ReadArtifact(*plan.PreviousManifest)
			if err != nil {
				test.Fatal(err)
			}
			priorObjects, err = installation.Documents(previousBytes)
			if err != nil {
				test.Fatal(err)
			}
		}
		plan.ReleaseRevision++
		failedManifest := writeArtifact("failed.yaml", run("helm", "get", "manifest", release, "--kubeconfig", kubeconfig, "--namespace", control, "--revision", fmt.Sprint(plan.ReleaseRevision)))
		plan.PreviousManifest = &failedManifest
		record := &unstructured.Unstructured{}
		record.SetAPIVersion("v1")
		record.SetKind("Secret")
		if err := admin.Get(ctx, client.ObjectKey{Namespace: control, Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, plan.ReleaseRevision)}, record); err != nil {
			test.Fatal(err)
		}
		if record.GetLabels()["status"] != "failed" {
			test.Fatalf("Helm must retain failed revision, not roll back: %v", record.GetLabels())
		}
		digest, err := installation.ObjectDigest(record)
		if err != nil {
			test.Fatal(err)
		}
		plan.FailedRelease = &installation.ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: control, Name: record.GetName(), UID: string(record.GetUID()), SHA256: digest}
		plan.Controllers, plan.Grants, plan.RetainedObjects = []installation.Controller{}, []installation.ObjectRef{}, nil
		seen := map[string]bool{}
		for _, object := range append(append([]*unstructured.Unstructured{}, documents...), priorObjects...) {
			identity := client.ObjectKeyFromObject(object)
			key := object.GroupVersionKind().GroupKind().String() + "/" + identity.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			live := object.DeepCopy()
			if err := admin.Get(ctx, identity, live); apierrors.IsNotFound(err) {
				plan.AbsentObjects = append(plan.AbsentObjects, installation.ObjectIdentity{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()})
				continue
			} else if err != nil {
				test.Fatal(err)
			}
			digest, err := installation.ObjectDigest(live)
			if err != nil {
				test.Fatal(err)
			}
			reference := installation.ObjectRef{APIVersion: live.GetAPIVersion(), Kind: live.GetKind(), Namespace: live.GetNamespace(), Name: live.GetName(), UID: string(live.GetUID()), SHA256: digest}
			if live.GetKind() == "Deployment" && live.GetName() == manifest.Deployment {
				plan.Controllers = append(plan.Controllers, installation.Controller{Deployment: reference, ServiceAccount: manifest.Controller})
			} else {
				plan.RetainedObjects = append(plan.RetainedObjects, reference)
			}
		}
		plan.Manifest = writeArtifact("retry.yaml", run("helm", "template", release, archives[0], "--namespace", control, "--kube-version", version, "--no-hooks", "--is-upgrade", "--values", valuesArtifact.Path, "--set", "k8sController.activated=false"))
		planBytes, err = json.Marshal(plan)
		if err != nil {
			test.Fatal(err)
		}
		planPath = writeArtifact("reviewed retry.json", planBytes).Path
	}
	var output []byte
	if failure == "certificate" || failure == "interrupted" {
		if err := admin.Delete(ctx, secret, client.Preconditions{UID: ptr.To(secret.UID)}); err != nil {
			test.Fatal(err)
		}
		if failure == "interrupted" {
			command := exec.CommandContext(ctx, executable, "bootstrap", "--plan", planPath, "--writers-frozen")
			var captured bytes.Buffer
			command.Stdout, command.Stderr = &captured, &captured
			if err := command.Start(); err != nil {
				test.Fatal(err)
			}
			waitErr := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				current := &corev1.ConfigMap{}
				if err := admin.Get(ctx, installation.JournalKey(plan), current); apierrors.IsNotFound(err) {
					return false, nil
				} else if err != nil {
					return false, err
				}
				return strings.Contains(current.Data["events"], "verify-bootstrap:pending"), nil
			})
			killErr := command.Process.Kill()
			exitErr := command.Wait()
			output = captured.Bytes()
			if waitErr != nil || killErr != nil || exitErr == nil {
				test.Fatalf("interrupt installer after Helm exit: poll=%v kill=%v exit=%v: %s", waitErr, killErr, exitErr, output)
			}
		} else {
			output, err = exec.CommandContext(ctx, executable, "bootstrap", "--plan", planPath, "--writers-frozen").CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte("verify-bootstrap:")) {
				test.Fatalf("missing serving certificate must prevent verified bootstrap: %v: %s", err, output)
			}
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatal(err)
		}
		secret.ResourceVersion, secret.UID = "", ""
		if err := admin.Create(ctx, secret); err != nil {
			test.Fatal(err)
		}
		if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
				return false, err
			}
			return activation.Verify(ctx, admin, manifest) == nil, nil
		}); err != nil {
			test.Fatal(err)
		}
	} else {
		output = run(executable, "bootstrap", "--plan", planPath, "--writers-frozen")
	}
	test.Logf("bootstrap: %s", output)
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatal(err)
	}
	saved := run("helm", "get", "values", release, "--kubeconfig", kubeconfig, "--namespace", control, "--output", "json")
	var savedValues struct {
		Controller struct {
			Activated bool `json:"activated"`
		} `json:"k8sController"`
	}
	if err := json.Unmarshal(saved, &savedValues); err != nil || savedValues.Controller.Activated {
		test.Fatalf("saved activation survived inactive bootstrap: %s, %v", saved, err)
	}
	claim := &corev1.ConfigMap{}
	if err := admin.Get(ctx, installation.JournalKey(plan), claim); err != nil {
		test.Fatal(err)
	}
	expectedEvent := "verified-inactive"
	if failure == "certificate" || failure == "interrupted" {
		expectedEvent = "verify-bootstrap:failed"
		if failure == "interrupted" {
			expectedEvent = "verify-bootstrap:pending"
		}
		if strings.Contains(claim.Data["events"], "verified-inactive") {
			test.Fatal("certificate failure incorrectly recorded verified bootstrap")
		}
	}
	if !strings.Contains(claim.Data["events"], expectedEvent) {
		test.Fatalf("missing verified-inactive journal: %v", claim.Data)
	}
	for range 2 {
		output := run(executable, "recover", "--plan", planPath, "--writers-frozen", "--executors-stopped", "--requests-settled", "--claim-uid", string(claim.UID))
		test.Logf("recover: %s", output)
	}
	if err := admin.Get(ctx, installation.JournalKey(plan), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		test.Fatalf("claim not retired: %v", err)
	}
	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: control}, Data: map[string]string{"preserve": "exactly"}}
	if err := admin.Create(ctx, unrelated); err != nil {
		test.Fatal(err)
	}
	unrelatedBefore := unrelated.DeepCopy()
	previous := writeArtifact("previous.yaml", run("helm", "get", "manifest", release, "--kubeconfig", kubeconfig, "--namespace", control, "--revision", fmt.Sprint(plan.ReleaseRevision+1)))
	plan.ReleaseRevision, plan.PreviousManifest = plan.ReleaseRevision+1, &previous
	plan.FailedRelease, plan.PreviousDeployedManifest, plan.PreviousDeployedRevision, plan.AbsentObjects = nil, nil, 0, nil
	plan.Controllers, plan.Grants, plan.RetainedObjects = nil, []installation.ObjectRef{}, nil
	for _, object := range documents {
		live := object.DeepCopy()
		if err := admin.Get(ctx, client.ObjectKeyFromObject(object), live); err != nil {
			test.Fatal(err)
		}
		digest, err := installation.ObjectDigest(live)
		if err != nil {
			test.Fatal(err)
		}
		reference := installation.ObjectRef{APIVersion: live.GetAPIVersion(), Kind: live.GetKind(), Namespace: live.GetNamespace(), Name: live.GetName(), UID: string(live.GetUID()), SHA256: digest}
		if live.GetKind() == "Deployment" && live.GetName() == manifest.Deployment {
			plan.Controllers = append(plan.Controllers, installation.Controller{Deployment: reference, ServiceAccount: manifest.Controller})
		} else {
			plan.RetainedObjects = append(plan.RetainedObjects, reference)
		}
	}
	plan.Manifest = writeArtifact("upgrade.yaml", run("helm", "template", release, archives[0], "--namespace", control, "--kube-version", version, "--no-hooks", "--is-upgrade", "--values", valuesArtifact.Path, "--set", "k8sController.activated=false"))
	planBytes, err = json.Marshal(plan)
	if err != nil {
		test.Fatal(err)
	}
	planPath = writeArtifact("reviewed upgrade.json", planBytes).Path
	test.Logf("upgrade: %s", run(executable, "bootstrap", "--plan", planPath, "--writers-frozen"))
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatal(err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(unrelated), unrelated); err != nil || unrelated.UID != unrelatedBefore.UID || unrelated.ResourceVersion != unrelatedBefore.ResourceVersion || unrelated.Data["preserve"] != "exactly" {
		test.Fatalf("unrelated resource changed: %v", err)
	}
	if err := admin.Get(ctx, installation.JournalKey(plan), claim); err != nil {
		test.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"recover", "--plan", planPath, "--writers-frozen", "--executors-stopped", "--requests-settled", "--claim-uid", "wrong-uid"},
		{"recover", "--plan", planPath, "--writers-frozen", "--executors-stopped", "--claim-uid", string(claim.UID)},
	} {
		output, err := exec.CommandContext(ctx, executable, arguments...).CombinedOutput()
		if err == nil {
			test.Fatalf("unsafe recovery accepted: %v: %s", arguments, output)
		}
		current := &corev1.ConfigMap{}
		if err := admin.Get(ctx, installation.JournalKey(plan), current); err != nil || current.UID != claim.UID || current.ResourceVersion != claim.ResourceVersion {
			test.Fatalf("rejected recovery changed claim: %v", err)
		}
	}
	test.Logf("upgrade recovery: %s", run(executable, "recover", "--plan", planPath, "--writers-frozen", "--executors-stopped", "--requests-settled", "--claim-uid", string(claim.UID)))
}
