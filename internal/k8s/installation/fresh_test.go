// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestFreshPlanRejectsExistingInstallationInventory(t *testing.T) {
	if err := ValidateFreshPlan(Plan{Version: 1}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Plan){
		func(plan *Plan) { plan.ReleaseRevision = 1 },
		func(plan *Plan) { plan.PreviousManifest = &Artifact{} },
		func(plan *Plan) { plan.FailedRelease = &ObjectRef{} },
		func(plan *Plan) { plan.Controllers = []Controller{{}} },
		func(plan *Plan) { plan.Grants = []ObjectRef{{}} },
		func(plan *Plan) { plan.LegacyPolicies = []ObjectRef{{}} },
		func(plan *Plan) { plan.RetainedObjects = []ObjectRef{{}} },
		func(plan *Plan) { plan.CompletedMigrationJobs = []ObjectRef{{}} },
		func(plan *Plan) { plan.CleanupAttestations = []CleanupAttestation{{}} },
	} {
		plan := Plan{Version: 1}
		mutate(&plan)
		if err := ValidateFreshPlan(plan); err == nil {
			t.Fatal("existing installation accepted as fresh")
		}
	}
}

func TestFreshRenderFreezesOnlyGeneratedSecretMaterial(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart-backed render verification")
	}
	chart := filepath.Join("..", "..", "..", "deploy", "helm", "kognition")
	render := func() []byte {
		t.Helper()
		output, err := exec.Command(helm, "template", "fresh-test", chart, "--namespace", "control", "--kube-version", "1.35.0",
			"--set", "mode=kubernetes", "--set", "k8sController.managedNamespaces[0]=tenant",
			"--set", "k8sController.image.digest=sha256:"+strings.Repeat("a", 64),
			"--set", "admissionGuard.stdioIsolationProfile=cilium", "--set", "networkPolicy.enabled=true",
			"--set", "networkPolicy.ciliumHostProbeIngress=true", "--set", "networkPolicy.ciliumApiServerEgress=true",
			"--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=10.0.0.0/24").CombinedOutput()
		if err != nil {
			t.Fatalf("chart rendering failed: %v\n%s", err, output)
		}
		return output
	}
	expected, actual := render(), render()
	encode := func(objects []*unstructured.Unstructured) []byte {
		t.Helper()
		var result strings.Builder
		for _, object := range objects {
			content, err := json.Marshal(object.Object)
			if err != nil {
				t.Fatal(err)
			}
			result.Write(content)
			result.WriteString("\n---\n")
		}
		return []byte(result.String())
	}
	if _, err := ValidateRender(expected, actual, "fresh-test"); err == nil {
		t.Fatal("fixture must exercise independently generated credentials")
	}
	pinned, err := FreshRender(expected, actual, "fresh-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRender(expected, pinned, "fresh-test"); err != nil {
		t.Fatal(err)
	}
	objects, err := Documents(actual)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if object.GetKind() == "Deployment" {
			if err := unstructured.SetNestedField(object.Object, int64(8), "spec", "replicas"); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, err := FreshRender(expected, encode(objects), "fresh-test"); err == nil {
		t.Fatal("non-credential render drift was accepted")
	}
	for _, scenario := range []string{"world-probes", "runtime-namespace-escape", "probe-egress"} {
		t.Run(scenario, func(t *testing.T) {
			objects, err := Documents(expected)
			if err != nil {
				t.Fatal(err)
			}
			changed := false
			for _, object := range objects {
				if scenario == "runtime-namespace-escape" && object.GetKind() == "CiliumClusterwideNetworkPolicy" {
					unstructured.RemoveNestedField(object.Object, "spec", "endpointSelector", "matchExpressions")
					changed = true
				}
				if strings.HasSuffix(object.GetName(), "-postgres-probes") {
					if scenario == "world-probes" {
						err = unstructured.SetNestedSlice(object.Object, []interface{}{map[string]interface{}{
							"fromEntities": []interface{}{"world"},
							"toPorts":      []interface{}{map[string]interface{}{"ports": []interface{}{map[string]interface{}{"port": "5432", "protocol": "TCP"}}}},
						}}, "spec", "ingress")
						changed = true
					}
					if scenario == "probe-egress" {
						err = unstructured.SetNestedSlice(object.Object, []interface{}{map[string]interface{}{"toEntities": []interface{}{"world"}}}, "spec", "egress")
						changed = true
					}
				}
			}
			if err != nil || !changed {
				t.Fatalf("invalid policy fixture: changed=%v err=%v", changed, err)
			}
			content := encode(objects)
			if _, err := FreshRender(content, content, "fresh-test"); err == nil {
				t.Fatal("broadened node-health policy accepted")
			}
		})
	}
}

func TestFreshRecoveryPreservesUserWorkAndAcceptsOnlyOwnedProbes(t *testing.T) {
	for _, scenario := range []string{"empty", "owned-probe", "replaced-probe", "foreign-probe", "user-agent", "busy-control"} {
		t.Run(scenario, func(t *testing.T) {
			localScheme := runtime.NewScheme()
			if err := scheme.AddToScheme(localScheme); err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{"AgentList", "MemoryClassList"} {
				localScheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: kind}, &unstructured.UnstructuredList{})
			}
			memory := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "kognition.dev/v1alpha1", "kind": "MemoryClass", "metadata": map[string]interface{}{"name": "default", "uid": "class-uid"},
			}}
			content, err := json.Marshal(memory.Object)
			if err != nil {
				t.Fatal(err)
			}
			objects := []client.Object{memory}
			var probes []activation.ResourceIntent
			if strings.Contains(scenario, "probe") {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "tenant", UID: "probe-uid",
					Annotations: map[string]string{"kognition.dev/activation-transaction": "transaction-uid"}}}
				probes = []activation.ResourceIntent{{APIVersion: "v1", Kind: "Pod", Namespace: "tenant", Name: "probe", UID: "probe-uid", Probe: true}}
				if scenario == "replaced-probe" {
					pod.UID = "replacement"
				}
				if scenario == "foreign-probe" {
					pod.Annotations = nil
				}
				objects = append(objects, pod)
			}
			if scenario == "user-agent" {
				objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "kognition.dev/v1alpha1", "kind": "Agent",
					"metadata": map[string]interface{}{"name": "existing", "namespace": "tenant", "uid": "agent-uid"},
				}})
			}
			if scenario == "busy-control" {
				objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other-app", Namespace: "control", UID: "other-uid"}})
			}
			admin := fake.NewClientBuilder().WithScheme(localScheme).WithObjects(objects...).Build()
			operations := &Kubernetes{Admin: admin, Manifest: activation.Manifest{Namespace: "control", Namespaces: []string{"tenant"}},
				Discovery: &preferredDiscovery{resources: []*metav1.APIResourceList{{GroupVersion: "kognition.dev/v1alpha1", APIResources: []metav1.APIResource{
					{Name: "agents", Kind: "Agent", Verbs: metav1.Verbs{"list"}}, {Name: "memoryclasses", Kind: "MemoryClass", Verbs: metav1.Verbs{"list"}},
				}}}}}
			err = operations.FreshWorkloadsEmpty(context.Background(), content, probes, "transaction-uid", scenario == "busy-control")
			allowed := scenario == "empty" || scenario == "owned-probe"
			if (err == nil) != allowed {
				t.Fatalf("allowed=%v: %v", allowed, err)
			}
		})
	}
}

func TestFreshAuthorizationRequiresCompletedFreshClaim(t *testing.T) {
	for _, scenario := range []string{"ready", "upgrade", "unfinished", "target-drift"} {
		t.Run(scenario, func(t *testing.T) {
			digest := strings.Repeat("a", 64)
			plan := Plan{Version: 1, ClusterUID: "cluster-uid", NamespaceUID: "control-uid", AuthorityNamespaceUID: "authority-uid",
				Release: "demo", AuthorityNamespace: "authority", Endpoint: "https://target.example",
				Chart: Artifact{SHA256: digest}, Manifest: Artifact{SHA256: digest}, WorkloadNamespaceUIDs: map[string]string{"tenant": "tenant-uid"}}
			key := JournalKey(plan)
			claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "claim-uid", ResourceVersion: "1"},
				Data: map[string]string{"version": "1", "mode": "fresh", "planSHA256": digest, "events": "claimed\nverified-inactive\n",
					"clusterUID": plan.ClusterUID, "namespaceUID": plan.NamespaceUID, "release": plan.Release}}
			root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid",
				Annotations: map[string]string{"kognition.dev/" + key.Name: "authority/authority-uid"}}}
			creates := 0
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(claim, root).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
					creates++
					object.SetUID(types.UID("authorization-uid"))
					return underlying.Create(ctx, object, options...)
				},
			}).Build()
			journal, err := OpenJournal(context.Background(), admin, plan, digest, "claim-uid")
			if err != nil {
				t.Fatal(err)
			}
			target := activation.ActivationTarget{ClusterUID: plan.ClusterUID, Endpoint: plan.Endpoint, ServerVersion: "v1.35.0",
				Release: plan.Release, Namespace: "control", Namespaces: []string{"tenant"}, AuthorityNamespace: plan.AuthorityNamespace,
				Controller: "controller", Deployment: "controller", Guard: "guard", ChartSHA256: digest, ManifestSHA256: digest,
				ToolVersion: digest, Replicas: 1, Images: map[string]string{"controller": digest, "guard": digest},
				Objects: []activation.ObjectEvidence{{APIVersion: "v1", Kind: "Namespace", Name: "control", UID: "control-uid", SHA256: digest}}}
			for name, uid := range map[string]string{"kube-system": plan.ClusterUID, "authority": plan.AuthorityNamespaceUID, "tenant": "tenant-uid"} {
				target.Objects = append(target.Objects, activation.ObjectEvidence{APIVersion: "v1", Kind: "Namespace", Name: name, UID: uid, SHA256: digest})
			}
			switch scenario {
			case "upgrade":
				plan.ReleaseRevision = 1
			case "unfinished":
				journal.record.Data["events"] = "claimed\n"
			case "target-drift":
				target.ManifestSHA256 = strings.Repeat("b", 64)
			}
			record, err := journal.AuthorizeFresh(context.Background(), plan, target)
			if scenario == "ready" {
				if err != nil || creates != 1 || record.Immutable == nil || !*record.Immutable || record.Data["fresh-bootstrap.json"] == "" || record.Data["SG-07.json"] != "" {
					t.Fatalf("fresh authorization failed or fabricated certification: %v", err)
				}
			} else if err == nil || creates != 0 {
				t.Fatalf("invalid claim issued an authorization: err=%v writes=%d", err, creates)
			}
		})
	}
}

func TestFreshServicesRequireCompletedMigrationsAndCurrentRollouts(t *testing.T) {
	manifest := []byte(`apiVersion: apps/v1
kind: Deployment
metadata: {name: memory, namespace: control}
spec:
  replicas: 1
  template:
    metadata:
      labels: {app.kubernetes.io/component: memory-service}
---
apiVersion: batch/v1
kind: Job
metadata: {name: migration, namespace: control}
spec: {}
`)
	for _, scenario := range []string{"ready", "old-rollout", "migration-pending", "migration-failed"} {
		t.Run(scenario, func(t *testing.T) {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "memory", Namespace: "control", UID: "memory-uid", Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
			}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: "control", UID: "job-uid"},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
			switch scenario {
			case "old-rollout":
				deployment.Status.UpdatedReplicas = 0
			case "migration-pending":
				job.Status.Conditions = nil
			case "migration-failed":
				job.Status.Conditions[0].Type = batchv1.JobFailed
			}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(deployment, job).Build()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := WaitForFreshServices(ctx, admin, manifest)
			if scenario == "ready" && err != nil {
				t.Fatal(err)
			}
			if scenario != "ready" && err == nil {
				t.Fatal("controller startup allowed before service and migration readiness")
			}
		})
	}
}

func TestFreshSnapshotSupportsExplicitCredentialPluginsWithoutRunningThem(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "artifact")
	content := []byte("captured")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	artifact := Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "kubeconfig")
	encoded := `{"apiVersion":"v1","kind":"Config","current-context":"target","contexts":[{"name":"target","context":{"cluster":"target","user":"operator"}}],"clusters":[{"name":"target","cluster":{"server":"https://target.example"}}],"users":[{"name":"operator","user":{"exec":{"apiVersion":"client.authentication.k8s.io/v1","interactiveMode":"Never","command":` + strconv.Quote(executable) + `}}}]}`
	if err := os.WriteFile(config, []byte(encoded), 0600); err != nil {
		t.Fatal(err)
	}
	plan := Plan{Version: 1, Kubeconfig: config, Chart: artifact, Values: artifact, Manifest: artifact}
	_, _, cleanup, err := SnapshotFresh(plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if _, _, cleanup, err := Snapshot(plan); err == nil {
		cleanup()
		t.Fatal("existing upgrade workflow unexpectedly accepted dynamic credentials")
	}
}
