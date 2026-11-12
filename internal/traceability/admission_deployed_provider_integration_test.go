// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/providers/azuresearch"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

const deployedProviderSandboxProbe = `import errno, os, pathlib, runpy
assert os.geteuid() == 1000 and os.getegid() == 1000
assert not pathlib.Path('/var/run/secrets/kubernetes.io/serviceaccount/token').exists()
for name in ['index-config.json', 'client.crt', 'client.key', 'ca.crt']:
    path = pathlib.Path('/var/run/kognition-index-config') / name
    assert path.read_bytes(), name
    try:
        path.write_bytes(b'must not write')
    except OSError as error:
        assert error.errno in (errno.EROFS, errno.EACCES), error
    else:
        raise AssertionError('provider credentials writable')
spool = pathlib.Path(os.environ['KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE'])
spool.write_text('probe')
assert spool.read_text() == 'probe'
spool.unlink()
try:
    pathlib.Path('/app/sg08-provider-write').write_text('must fail')
except OSError as error:
    assert error.errno == errno.EROFS, error
else:
    raise AssertionError('root filesystem writable')
print('SG08 provider sandbox: uid=1000 credentials=readable-readonly spool=writable root=EROFS token=absent', flush=True)
runpy.run_path('/app/ingest.py', run_name='__main__')
`

func deployedProviderAcceptance(test *testing.T, adminConfig, controllerConfig *rest.Config, admin client.Client, controller client.WithWatch, manifest activation.Manifest, image, providerImage string, created *[]client.Object) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	create := func(object client.Object) {
		test.Helper()
		if err := createAdmissionFixture(ctx, admin, object, created); err != nil {
			test.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "helm", "kognition-knowledge-azuresearch", "crds", "azuresearch.knowledge.kognition.dev_azuresearchbackends.yaml"))
	if err != nil {
		test.Fatal(err)
	}
	definition := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(raw, &definition.Object); err != nil {
		test.Fatal(err)
	}
	create(definition)
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
			return false, err
		}
		conditions, _, _ := unstructured.NestedSlice(definition.Object, "status", "conditions")
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
	workload := manifest.Namespaces[0]
	kind := schema.GroupVersionKind{Group: azuresearch.APIGroup, Version: azuresearch.APIVersion, Kind: azuresearch.Kind}
	backend := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"serviceName": "sg08-local", "indexName": "sg08-local", "embeddingDimensions": int64(8), "authentication": map[string]any{"mode": "workload-identity"},
	}}}
	backend.SetGroupVersionKind(kind)
	backend.SetNamespace(workload)
	backend.SetName("sg08-provider")
	create(backend)
	class := convert.KnowledgeBaseClassToK8s(&api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "sg08-provider-" + manifest.Namespace}, Spec: api.KnowledgeBaseClassSpec{
		Provisioner: "sg08-local", ReclaimPolicy: api.ReclaimDelete, EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
		BackendRef: &api.KnowledgeBackendReference{APIGroup: kind.Group, Kind: kind.Kind, Name: backend.GetName()},
	}})
	create(class)
	kb := convert.KnowledgeBaseToK8s(&api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "sg08-provider", Namespace: workload}, Spec: api.KnowledgeBaseSpec{
		KnowledgeBaseClassName: class.Name, Sources: []api.KBSource{{Type: "inline", Data: "SG08 provider-backed document."}},
	}})
	create(kb)
	resolver, err := knowledge.NewBindingResolver(controller, []knowledge.Registration{{ProviderName: "sg08-local", ConfigurationKind: kind,
		EmbeddingDimension: func(*unstructured.Unstructured) (int, error) { return 8, nil },
	}})
	if err != nil {
		test.Fatal(err)
	}
	pending, err := resolver.ResolvePending(ctx, client.ObjectKeyFromObject(kb))
	if err != nil {
		test.Fatal(err)
	}
	scope, err := protojson.Marshal(pending.Scope)
	if err != nil {
		test.Fatal(err)
	}
	clientMaterial, serverMaterial := deployedProviderCertificates(test)
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sg08-provider-client", Namespace: manifest.Namespace}, Data: clientMaterial})
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sg08-provider-server", Namespace: workload}, Data: serverMaterial})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sg08-provider", Namespace: workload, Labels: map[string]string{"sg08-provider": "true"}}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(1)),
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(1000)), RunAsGroup: ptr.To(int64(1000)), RunAsNonRoot: ptr.To(true)},
		Volumes:         []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "sg08-provider-server"}}}},
		Containers: []corev1.Container{{Name: "provider", Image: providerImage, ImagePullPolicy: corev1.PullNever,
			Env:             []corev1.EnvVar{{Name: "SG08_EXPECTED_SCOPE", Value: string(scope)}},
			VolumeMounts:    []corev1.VolumeMount{{Name: "tls", MountPath: "/fixture", ReadOnly: true}},
			SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(9443)}}, PeriodSeconds: 1},
		}},
	}}
	create(pod)
	create(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "sg08-provider", Namespace: workload}, Spec: corev1.ServiceSpec{Selector: pod.Labels, Ports: []corev1.ServicePort{{Port: 9443, TargetPort: intstr.FromInt32(9443)}}}})
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return false, err
		}
		return len(pod.Status.ContainerStatuses) == 1 && pod.Status.ContainerStatuses[0].Ready, nil
	}); err != nil {
		test.Fatal(err)
	}
	provider := k8scontroller.KBIngesterProviderOptions{Name: "sg08-local", Address: "sg08-provider." + workload + ".svc:9443", ServerName: "knowledge.test", PeerURI: "spiffe://kognition.test/providers/sg08",
		ConfigurationKind: kind, TLSSecretName: "sg08-provider-client", TLSCertificateKey: "tls.crt", TLSPrivateKeyKey: "tls.key", TLSTrustBundleKey: "ca.crt"}
	producerClient := interceptor.NewClient(controller, interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if job, ok := object.(*batchv1.Job); ok {
			job.Spec.Template.Spec.Containers[0].Command = []string{"python", "-c", deployedProviderSandboxProbe}
		}
		return underlying.Create(ctx, object, options...)
	}})
	newAdapter := func(options k8scontroller.KBIngesterProviderOptions) *k8scontroller.KnowledgeBaseReconcilerAdapter {
		manager, err := ctrl.NewManager(controllerConfig, ctrl.Options{Scheme: controller.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Controller: controllerconfig.Controller{SkipNameValidation: ptr.To(true)}, NewClient: func(*rest.Config, client.Options) (client.Client, error) { return producerClient, nil }})
		if err != nil {
			test.Fatal(err)
		}
		adapter := k8scontroller.NewKnowledgeBaseReconcilerAdapter(k8scontroller.NewStoreAdapterWithControlNamespace(producerClient, manifest.Namespace), zap.NewNop()).WithIngesterOptions(k8scontroller.KBIngesterOptions{Image: image, ControllerNamespace: manifest.Namespace, Provider: &options})
		if err := adapter.SetupWithManager(manager); err != nil {
			test.Fatal(err)
		}
		return adapter
	}
	adapter := newAdapter(provider)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(kb)}
	provider.Address = deployedPodForward(test, ctx, adminConfig, pod, 9443)
	cleanerAdapter := newAdapter(provider)
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		current := &k8sapi.KnowledgeBase{}
		if err := admin.Get(cleanup, request.NamespacedName, current); apierrors.IsNotFound(err) {
			return
		} else if err != nil || current.UID != kb.UID {
			test.Errorf("provider KB recovery identity: %v", err)
			return
		}
		if current.Status.BackendBinding == nil {
			return
		}
		if err := admin.Delete(cleanup, current, client.Preconditions{UID: &current.UID}); err != nil {
			test.Error(err)
			return
		}
		if _, err := cleanerAdapter.Reconcile(cleanup, request); err != nil {
			test.Error(err)
		}
	})
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		if _, err := adapter.Reconcile(ctx, request); err != nil {
			return false, err
		}
		if err := admin.Get(ctx, request.NamespacedName, kb); err != nil {
			return false, err
		}
		if string(kb.Status.Phase) == string(api.PhaseFailed) {
			return false, fmt.Errorf("provider ingestion failed: %+v", kb.Status)
		}
		return string(kb.Status.Phase) == string(api.PhaseReady), nil
	}); err != nil {
		test.Fatal(err)
	}
	binding := kb.Status.BackendBinding
	jobName := "kb-ingest-sg08-provider-1"
	if binding == nil || binding.ProviderName != "sg08-local" || binding.Generation != 1 || binding.ClassUID != string(class.UID) || binding.ClassGeneration != class.Generation || binding.ConfigurationUID != string(backend.GetUID()) || binding.ConfigurationDigest != pending.Scope.ConfigurationDigest || binding.ConfigurationAPIGroup != kind.Group || binding.ConfigurationKind != kind.Kind || binding.ConfigurationName != backend.GetName() || string(binding.ReclaimPolicy) != string(api.ReclaimDelete) || binding.OperationID != workload+"/"+jobName || binding.BackendOperationID != "sg08-local-commit" || kb.Status.DocumentCount != 1 || kb.Status.IndexedChunks != 1 {
		test.Fatalf("provider did not publish exact confirmed binding: %+v", kb.Status)
	}
	job := &batchv1.Job{}
	input := &corev1.Secret{}
	for _, object := range []client.Object{job, input} {
		name := jobName
		if object == input {
			name += "-input"
		}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: name}, object); err != nil {
			test.Fatal(err)
		}
		if !metav1.IsControlledBy(object, kb) {
			test.Fatalf("provider child lacks exact KB ownership: %T %s", object, name)
		}
	}
	clientset, err := kubernetes.NewForConfig(adminConfig)
	if err != nil {
		test.Fatal(err)
	}
	pods := &corev1.PodList{}
	if err := admin.List(ctx, pods, client.InNamespace(workload), client.MatchingLabels{"job-name": jobName}); err != nil || len(pods.Items) != 1 {
		test.Fatalf("provider ingestion pods=%d error=%v", len(pods.Items), err)
	}
	completion := &pods.Items[0]
	if !metav1.IsControlledBy(completion, job) || completion.Status.Phase != corev1.PodSucceeded || len(completion.Status.ContainerStatuses) != 1 || completion.Status.ContainerStatuses[0].ImageID == "" || completion.Spec.Containers[0].Image != image {
		test.Fatalf("provider completion lacks exact successful Job/image identity: %+v", completion)
	}
	logs, err := clientset.CoreV1().Pods(workload).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "ingester"}).DoRaw(ctx)
	if err != nil || !strings.Contains(string(logs), "SG08 provider sandbox:") {
		test.Fatalf("provider sandbox proof missing: %v %s", err, logs)
	}
	test.Logf("%s", logs)
	if err := admin.Delete(ctx, kb, client.Preconditions{UID: &kb.UID}); err != nil {
		test.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if _, err := cleanerAdapter.Reconcile(ctx, request); err != nil {
			return false, err
		}
		err := admin.Get(ctx, request.NamespacedName, &k8sapi.KnowledgeBase{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}); err != nil {
		test.Fatal(err)
	}
	for _, object := range []client.Object{job, completion, input} {
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 40*time.Second, true, func(ctx context.Context) (bool, error) {
			current := object.DeepCopyObject().(client.Object)
			err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err == nil && current.GetUID() != object.GetUID() {
				return false, fmt.Errorf("provider child replaced during cleanup: %T %s", object, object.GetName())
			}
			return false, err
		}); err != nil {
			test.Fatalf("provider child cleanup did not settle: %T %s: %v", object, object.GetName(), err)
		}
	}
	test.Logf("provider Job UID=%s Pod UID=%s input Secret UID=%s absent after finalizer deletion; ingester imageID=%s", job.UID, completion.UID, input.UID, completion.Status.ContainerStatuses[0].ImageID)
	logs, err = clientset.CoreV1().Pods(workload).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "provider"}).DoRaw(ctx)
	if err != nil || !strings.Contains(string(logs), "SG08 provider cleaned operation="+binding.OperationID) {
		test.Fatalf("provider cleanup proof missing: %v %s", err, logs)
	}
	test.Logf("provider Pod UID=%s imageID=%s KB UID=%s binding=%+v; production cleanup/finalizer confirmed\n%s", pod.UID, pod.Status.ContainerStatuses[0].ImageID, kb.UID, binding, logs)
}

func deployedProviderCertificates(test *testing.T) (map[string][]byte, map[string][]byte) {
	test.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SG08 isolated fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		test.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		test.Fatal(err)
	}
	issue := func(serial int64, identity string) map[string][]byte {
		private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			test.Fatal(err)
		}
		uri, err := url.Parse(identity)
		if err != nil {
			test.Fatal(err)
		}
		certificate := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: []string{"knowledge.test"}, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		encoded, err := x509.CreateCertificate(rand.Reader, certificate, ca, &private.PublicKey, key)
		if err != nil {
			test.Fatal(err)
		}
		privateDER, err := x509.MarshalECPrivateKey(private)
		if err != nil {
			test.Fatal(err)
		}
		return map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateDER}), "ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
	}
	return issue(2, "spiffe://kognition.test/core/sg08"), issue(3, "spiffe://kognition.test/providers/sg08")
}
