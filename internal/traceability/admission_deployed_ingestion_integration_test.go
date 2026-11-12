// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

const deployedIngesterSandboxProbe = `import errno, os, pathlib, runpy
assert os.geteuid() == 1000 and os.getegid() == 1000, "unexpected runtime identity"
assert not pathlib.Path("/var/run/secrets/kubernetes.io/serviceaccount/token").exists(), "unexpected service account token"
try:
    pathlib.Path("/app/sg08-root-write").write_text("must fail")
except OSError as error:
    assert error.errno == errno.EROFS, error
else:
    raise AssertionError("root filesystem is writable")
temporary = pathlib.Path("/tmp/sg08-writable")
temporary.write_text("sandbox-check")
assert temporary.read_text() == "sandbox-check"
temporary.unlink()
assert pathlib.Path(os.environ["KOGNITION_INGEST_INPUT_FILE"]).read_bytes(), "unreadable input"
print("SG08 sandbox: uid=1000 gid=1000 root=EROFS tmp=writable input=readable token=absent", flush=True)
runpy.run_path("/app/ingest.py", run_name="__main__")
`

func deployedPostgresFixture(namespace, image string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sg08-postgres", Namespace: namespace}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(1)),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65534)), RunAsGroup: ptr.To(int64(65534)), FSGroup: ptr.To(int64(65534)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{Name: "postgres", Image: image, ImagePullPolicy: deployedFixturePullPolicy(),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			Env: []corev1.EnvVar{
				{Name: "POSTGRES_HOST_AUTH_METHOD", Value: "trust"},
				{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/var/lib/postgresql/data"},
				{Name: "postgres-run", MountPath: "/var/run/postgresql"},
				{Name: "tmp", MountPath: "/tmp"},
			},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres"}}}, PeriodSeconds: 1},
		}},
		Volumes: []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "postgres-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}}
}

func deployedIngestionProducerAcceptance(test *testing.T, config *rest.Config, manifest activation.Manifest, roles []*unstructured.Unstructured, image, databaseImage string) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := k8sapi.AddToScheme(scheme.Scheme); err != nil {
		test.Fatal(err)
	}
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	var created []client.Object
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for index := len(created) - 1; index >= 0; index-- {
			object := created[index]
			if object.GetUID() == "" {
				test.Errorf("refusing ingestion cleanup without UID: %T %s", object, object.GetName())
				continue
			}
			if err := admin.Delete(cleanup, object, client.Preconditions{UID: ptr.To(object.GetUID())}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				test.Errorf("ingestion cleanup %T %s: %v", object, object.GetName(), err)
				continue
			}
			if err := wait.PollUntilContextTimeout(cleanup, 100*time.Millisecond, 40*time.Second, true, func(ctx context.Context) (bool, error) {
				current := object.DeepCopyObject().(client.Object)
				err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err == nil && current.GetUID() != object.GetUID() {
					return false, fmt.Errorf("replacement UID during cleanup: %s", current.GetUID())
				}
				return false, err
			}); err != nil {
				test.Error(err)
			}
		}
	})
	create := func(object client.Object) {
		test.Helper()
		if err := createAdmissionFixture(ctx, admin, object, &created); err != nil {
			test.Fatal(err)
		}
	}
	for _, resource := range []string{"knowledgebases", "knowledgebaseclasses", "knowledgebaseindexschedules", "models", "secretrefs", "agents", "agentservices", "agentinstances"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "helm", "kognition", "crds", "kognition.dev_"+resource+".yaml"))
		if err != nil {
			test.Fatal(err)
		}
		definition := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &definition.Object); err != nil {
			test.Fatal(err)
		}
		current := definition.DeepCopy()
		readErr := admin.Get(ctx, client.ObjectKeyFromObject(definition), current)
		if os.Getenv("KOGNITION_ADMISSION_TARGET_MANIFEST") != "" && readErr == nil {
			for _, field := range []string{"group", "names", "scope", "versions"} {
				expected, _, err := unstructured.NestedFieldCopy(definition.Object, "spec", field)
				if err != nil {
					test.Fatal(err)
				}
				actual, _, err := unstructured.NestedFieldCopy(current.Object, "spec", field)
				if err != nil {
					test.Fatal(err)
				}
				expectedJSON, expectedErr := json.Marshal(expected)
				actualJSON, actualErr := json.Marshal(actual)
				if expectedErr != nil || actualErr != nil || !bytes.Equal(expectedJSON, actualJSON) {
					test.Fatalf("existing CRD %s differs from reviewed %s", definition.GetName(), field)
				}
			}
			test.Logf("retaining existing reviewed CRD %s UID=%s", current.GetName(), current.GetUID())
		} else {
			if readErr != nil && !apierrors.IsNotFound(readErr) {
				test.Fatal(readErr)
			}
			create(definition)
		}
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			current := definition.DeepCopy()
			if err := admin.Get(ctx, client.ObjectKeyFromObject(definition), current); err != nil {
				return false, err
			}
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			for _, value := range conditions {
				condition, ok := value.(map[string]any)
				if ok && condition["type"] == "Established" && condition["status"] == "True" {
					return true, nil
				}
			}
			return false, nil
		}); err != nil {
			test.Fatal(err)
		}
	}
	grantDeployedProducerRoles(test, ctx, admin, manifest, roles, &created)
	controllerConfig := rest.CopyConfig(config)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + manifest.Namespace, "system:authenticated"}}
	controller, err := client.NewWithWatch(controllerConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	workload := manifest.Namespaces[0]
	database := deployedPostgresFixture(workload, databaseImage)
	create(database)
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(database), database); err != nil {
			return false, err
		}
		for _, status := range database.Status.ContainerStatuses {
			if status.Ready {
				test.Logf("local PostgreSQL Pod UID=%s imageID=%s", database.UID, status.ImageID)
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		test.Logf("database fixture phase=%s node=%s containers=%+v", database.Status.Phase, database.Spec.NodeName, database.Status.ContainerStatuses)
		diagnostics, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			test.Logf("database diagnostic client: %v", clientErr)
		} else {
			logs, logErr := diagnostics.CoreV1().Pods(workload).GetLogs(database.Name, &corev1.PodLogOptions{Container: "postgres"}).DoRaw(ctx)
			if logErr != nil {
				test.Logf("database diagnostic logs: %v", logErr)
			} else {
				test.Logf("database fixture logs: %s", logs)
			}
		}
		test.Fatal(err)
	}
	deployedProducerExec(test, ctx, config, database, "postgres", []string{"psql", "-h", "127.0.0.1", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "CREATE EXTENSION vector"})
	test.Setenv("KOGNITION_SG08_LOCAL_DSN", "postgresql://postgres@"+database.Status.PodIP+":5432/postgres?sslmode=disable")
	create(&k8sapi.SecretRef{ObjectMeta: metav1.ObjectMeta{Name: "pgvector", Namespace: workload}, Spec: k8sapi.SecretRefSpec{BackendType: "env", BackendRef: "KOGNITION_SG08_LOCAL_DSN", Key: "connectionString"}})
	create(convert.ModelToK8s(&api.Model{ObjectMeta: api.ObjectMeta{Name: "embed", Namespace: workload}, Spec: api.ModelSpec{Provider: "deterministic", ModelID: "sg08-local"}}))
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "sg08-docs", Namespace: workload}, Spec: api.KnowledgeBaseSpec{KnowledgeBaseClassName: "pgvector", Sources: []api.KBSource{{Type: "inline", Data: "SG08 restricted ingestion persists this document."}}}}
	kbObject := convert.KnowledgeBaseToK8s(kb)
	create(kbObject)
	kb.ObjectMeta.UID = string(kbObject.UID)
	kbc := &api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "pgvector"}, Spec: api.KnowledgeBaseClassSpec{
		EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
		VectorStore:       &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"}, Dimension: 8}},
	}}
	probeInjected := false
	ingestionClient := interceptor.NewClient(controller, interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if job, ok := object.(*batchv1.Job); ok && !probeInjected {
			job.Spec.Template.Spec.Containers[0].Command = []string{"python", "-c", deployedIngesterSandboxProbe}
			probeInjected = true
		}
		return underlying.Create(ctx, object, options...)
	}})
	launcher := k8scontroller.NewKBIngesterLauncher(ingestionClient, k8scontroller.NewStoreAdapterWithControlNamespace(controller, manifest.Namespace), zap.NewNop(), image, "", manifest.Namespace)
	for attempt := 0; attempt < 3; attempt++ {
		if attempt == 2 {
			kb.Status.Generation++
		}
		trackingStart := len(created)
		state, stats, err := launcher.Ensure(ctx, kb, kbc)
		if err != nil || state != intctrl.IngestRunning {
			test.Fatalf("launch state=%v stats=%+v error=%v", state, stats, err)
		}
		job := &batchv1.Job{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: fmt.Sprintf("kb-ingest-sg08-docs-%d", kb.Status.Generation+1)}, job); err != nil {
			test.Fatal(err)
		}
		created = append(created, job.DeepCopy())
		input := &corev1.Secret{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: job.Name + "-input"}, input); err != nil {
			test.Fatal(err)
		}
		created = append(created, input.DeepCopy())
		launcher = k8scontroller.NewKBIngesterLauncher(ingestionClient, k8scontroller.NewStoreAdapterWithControlNamespace(controller, manifest.Namespace), zap.NewNop(), image, "", manifest.Namespace)
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			state, stats, err = launcher.Ensure(ctx, kb, kbc)
			if err != nil {
				return false, err
			}
			if state == intctrl.IngestFailed {
				return false, fmt.Errorf("ingestion failed: %+v", stats)
			}
			return state == intctrl.IngestSucceeded, nil
		}); err != nil {
			test.Fatal(err)
		}
		var pods corev1.PodList
		if err := admin.List(ctx, &pods, client.InNamespace(workload), client.MatchingLabels{"job-name": job.Name}); err != nil {
			test.Fatal(err)
		}
		if len(pods.Items) != 1 || !metav1.IsControlledBy(&pods.Items[0], job) {
			test.Fatal("missing exact Job-owned completion Pod")
		}
		pod := &pods.Items[0]
		created = append(created, pod.DeepCopy())
		if attempt == 0 {
			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				test.Fatal(err)
			}
			logs, err := clientset.CoreV1().Pods(workload).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "ingester"}).DoRaw(ctx)
			marker := "SG08 sandbox: uid=1000 gid=1000 root=EROFS tmp=writable input=readable token=absent"
			if err != nil || !strings.Contains(string(logs), marker) {
				test.Fatalf("missing successful sandbox probe: %v", err)
			}
			test.Log(marker)
		}
		currentJob := &batchv1.Job{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(job), currentJob); err != nil || currentJob.UID != job.UID {
			test.Fatalf("launcher restart replaced Job: UID=%s expected=%s error=%v", currentJob.UID, job.UID, err)
		}
		if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 1000 || pod.Status.Phase != corev1.PodSucceeded {
			test.Fatalf("ingester did not complete as UID 1000: %+v", pod.Status)
		}
		for _, container := range pod.Spec.Containers {
			if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
				test.Fatalf("completed ingester lacked read-only root: %s", container.Name)
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 || status.ImageID == "" {
				test.Fatalf("ingester termination not successful: %+v", status)
			}
			test.Logf("production ingestion Job UID=%s Pod UID=%s imageID=%s summary=%s", job.UID, pod.UID, status.ImageID, status.State.Terminated.Message)
		}
		rows := deployedProducerExec(test, ctx, config, database, "postgres", []string{"psql", "-U", "postgres", "-At", "-v", "ON_ERROR_STOP=1", "-c", "SELECT count(*) FROM kognition_kb_chunks WHERE kb_namespace='" + workload + "' AND kb_name='sg08-docs' AND content='SG08 restricted ingestion persists this document.' AND vector_dims(embedding)=8"})
		if strings.TrimSpace(rows) != "1" {
			test.Fatalf("persisted chunk count=%q, want 1", rows)
		}
		if err := launcher.Reset(ctx, kb); err != nil {
			test.Fatalf("production reset: %v", err)
		}
		for _, object := range created[trackingStart:] {
			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 40*time.Second, true, func(ctx context.Context) (bool, error) {
				current := object.DeepCopyObject().(client.Object)
				err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err == nil && current.GetUID() != object.GetUID() {
					return false, fmt.Errorf("replacement during reset: %T %s UID=%s", object, object.GetName(), current.GetUID())
				}
				return false, err
			}); err != nil {
				test.Fatalf("production reset did not settle for %T %s: %v", object, object.GetName(), err)
			}
		}
		created = created[:trackingStart]
		if err := launcher.Reset(ctx, kb); err != nil {
			test.Fatalf("idempotent reset: %v", err)
		}
		test.Logf("ingestion attempt=%d generation=%d restart preserved Job UID=%s; reset confirmed Job/Pod/input Secret absence", attempt+1, kb.Status.Generation+1, job.UID)
	}
	test.Logf("production ingestion completed with persisted document under %s", controllerConfig.Impersonate.UserName)
	test.Run("non-root-failure", func(test *testing.T) {
		failedKB := *kb
		failedKB.ObjectMeta.Name = "sg08-root-write"
		failedKB.Status = api.KnowledgeBaseStatus{}
		failedObject := convert.KnowledgeBaseToK8s(&failedKB)
		failedObject.UID = ""
		create(failedObject)
		failedKB.ObjectMeta.UID = string(failedObject.UID)
		failingClient := interceptor.NewClient(controller, interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if job, ok := object.(*batchv1.Job); ok {
				job.Spec.BackoffLimit = ptr.To(int32(0))
				job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
				job.Spec.Template.Spec.Containers[0].Command = []string{"python", "-c", "import os, pathlib; assert os.geteuid() == 1000; pathlib.Path('/app/sg08-forbidden').write_text('must fail')"}
			}
			return underlying.Create(ctx, object, options...)
		}})
		failedLauncher := k8scontroller.NewKBIngesterLauncher(failingClient, k8scontroller.NewStoreAdapterWithControlNamespace(controller, manifest.Namespace), zap.NewNop(), image, "", manifest.Namespace)
		if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			state, _, err := failedLauncher.Ensure(ctx, &failedKB, kbc)
			if state == intctrl.IngestSucceeded {
				return false, fmt.Errorf("root-writing ingester unexpectedly succeeded")
			}
			return state == intctrl.IngestFailed, err
		}); err != nil {
			test.Fatal(err)
		}
		job := &batchv1.Job{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: "kb-ingest-sg08-root-write-1"}, job); err != nil {
			test.Fatal(err)
		}
		var pods corev1.PodList
		if err := admin.List(ctx, &pods, client.InNamespace(workload), client.MatchingLabels{"job-name": job.Name}); err != nil {
			test.Fatal(err)
		}
		if len(pods.Items) != 1 || !metav1.IsControlledBy(&pods.Items[0], job) || pods.Items[0].Status.Phase != corev1.PodFailed {
			test.Fatal("missing exact failed Job-owned Pod")
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			test.Fatal(err)
		}
		logs, err := clientset.CoreV1().Pods(workload).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "ingester"}).DoRaw(ctx)
		if err != nil || !strings.Contains(string(logs), "Read-only file system") {
			test.Fatalf("failure was not enforced read-only root: %v logs=%s", err, logs)
		}
		if err := failedLauncher.Reset(ctx, &failedKB); err != nil {
			test.Fatal(err)
		}
		for _, object := range []client.Object{job, &pods.Items[0], &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: workload, Name: job.Name + "-input"}}} {
			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 40*time.Second, true, func(ctx context.Context) (bool, error) {
				err := admin.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object))
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			}); err != nil {
				test.Fatal(err)
			}
		}
		test.Logf("UID 1000 root-write failed with EROFS; production launcher reported failure and Reset removed Job/Pod/input: Job UID=%s Pod UID=%s", job.UID, pods.Items[0].UID)
	})
	class := *kbc
	class.ObjectMeta.Name = "sg08-" + manifest.Namespace
	class.Spec.Provisioner = "pgvector"
	class.Spec.ReclaimPolicy = api.ReclaimDelete
	create(convert.KnowledgeBaseClassToK8s(&class))
	reconciled := convert.KnowledgeBaseToK8s(&api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "sg08-reconciled", Namespace: workload},
		Spec: api.KnowledgeBaseSpec{KnowledgeBaseClassName: class.ObjectMeta.Name,
			Sources: []api.KBSource{{Type: "inline", Data: "SG08 reconciled document."}}},
	})
	create(reconciled)
	request := intctrl.Request{Namespace: workload, Name: reconciled.Name, Kind: "KnowledgeBase"}
	reconcile := func(ctx context.Context) error {
		store := k8scontroller.NewStoreAdapterWithControlNamespace(controller, manifest.Namespace)
		currentLauncher := k8scontroller.NewKBIngesterLauncher(controller, store, zap.NewNop(), image, "", manifest.Namespace)
		_, err := intctrl.NewKnowledgeBaseReconciler(store, zap.NewNop()).WithIngesterLauncher(currentLauncher).Reconcile(ctx, request)
		return err
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := admin.Delete(cleanup, reconciled, client.Preconditions{UID: ptr.To(reconciled.UID)}); err != nil && !apierrors.IsNotFound(err) {
			test.Error(err)
			return
		}
		if err := reconcile(cleanup); err != nil {
			test.Error(err)
		}
	})
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := reconcile(ctx); err != nil {
			return false, err
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(reconciled), reconciled); err != nil {
			return false, err
		}
		if string(reconciled.Status.Phase) == string(api.PhaseFailed) {
			return false, fmt.Errorf("reconciled ingestion failed: %+v", reconciled.Status)
		}
		return string(reconciled.Status.Phase) == string(api.PhaseReady), nil
	}); err != nil {
		test.Fatal(err)
	}
	if reconciled.Status.Generation != 1 || reconciled.Status.DocumentCount != 1 || reconciled.Status.IndexedChunks != 1 || !slices.Contains(reconciled.Finalizers, intctrl.KnowledgeBaseFinalizer) {
		test.Fatalf("reconciled KB did not publish completed generation/counts/finalizer: %+v", reconciled)
	}
	if err := admin.Delete(ctx, reconciled, client.Preconditions{UID: ptr.To(reconciled.UID)}); err != nil {
		test.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := reconcile(ctx); err != nil {
			return false, err
		}
		err := admin.Get(ctx, client.ObjectKeyFromObject(reconciled), &k8sapi.KnowledgeBase{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}); err != nil {
		test.Fatal(err)
	}
	test.Logf("production KB reconcile published Ready generation=1 documents=1 chunks=1; reconstructed reconciler completed finalizer deletion UID=%s", reconciled.UID)
	test.Run("provider-binding", func(test *testing.T) {
		providerImage := os.Getenv("KOGNITION_ADMISSION_PROVIDER_IMAGE")
		if providerImage == "" {
			test.Skip("KOGNITION_ADMISSION_PROVIDER_IMAGE required for local provider binding acceptance")
		}
		deployedProviderAcceptance(test, config, controllerConfig, admin, controller, manifest, image, providerImage, &created)
	})
	test.Run("agent-runtime", func(test *testing.T) {
		image := os.Getenv("KOGNITION_ADMISSION_AGENT_IMAGE")
		if image == "" {
			test.Skip("KOGNITION_ADMISSION_AGENT_IMAGE required for local Agent runtime acceptance")
		}
		agentCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		deployedAgentProducerAcceptance(test, agentCtx, config, admin, controller, manifest, roles, image, &created)
	})
	test.Run("autogen-runtime", func(test *testing.T) {
		image := os.Getenv("KOGNITION_ADMISSION_AUTOGEN_IMAGE")
		if image == "" {
			test.Skip("KOGNITION_ADMISSION_AUTOGEN_IMAGE required for local AutoGen TLS acceptance")
		}
		autogenCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		deployedAutoGenProducerAcceptance(test, autogenCtx, config, admin, controller, manifest, image, &created)
	})
}

func deployedProducerExec(test *testing.T, ctx context.Context, config *rest.Config, pod *corev1.Pod, container string, command []string) string {
	test.Helper()
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	request := clientset.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: container, Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
	if err != nil {
		test.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		test.Fatalf("local fixture exec: %v stderr=%s", err, stderr.String())
	}
	return stdout.String()
}
