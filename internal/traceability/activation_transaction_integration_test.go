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
	"encoding/pem"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestActivationTransactionAPI(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	admin, err := client.NewWithWatch(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	for _, namespace := range []string{"control", "tenant", "authority"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			test.Fatal(err)
		}
	}
	var rendered strings.Builder
	var bootstrap []*unstructured.Unstructured
	digest := strings.Repeat("a", 64)
	for _, document := range renderControllerChart(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(document.Raw); err != nil {
			test.Fatal(err)
		}
		if object.GetKind() == "Deployment" {
			containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
			for _, entry := range containers {
				entry.(map[string]interface{})["image"] = "example.test/fixture@sha256:" + digest
			}
			if err := unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				test.Fatal(err)
			}
		}
		encoded, err := object.MarshalJSON()
		if err != nil {
			test.Fatal(err)
		}
		rendered.WriteString("---\n")
		rendered.Write(encoded)
		rendered.WriteString("\n")
		if object.GetLabels()["app.kubernetes.io/component"] == "k8s-controller" && (object.GetKind() == "Role" || object.GetKind() == "ClusterRole" || object.GetKind() == "ServiceAccount" || object.GetKind() == "Deployment") {
			bootstrap = append(bootstrap, object)
		}
		component := object.GetLabels()["app.kubernetes.io/component"]
		if component == "resource-webhooks" || component == "webhook" {
			switch object.GetKind() {
			case "Deployment", "Service", "ServiceAccount", "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding", "NetworkPolicy", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration":
				bootstrap = append(bootstrap, object)
			}
		}
	}
	manifest, err := activation.Parse(strings.NewReader(rendered.String()))
	if err != nil {
		test.Fatal(err)
	}
	candidate, err := activation.ParseCandidate([]byte(rendered.String()), "security-test")
	if err != nil {
		test.Fatal(err)
	}
	certificateServer := httptest.NewTLSServer(nil)
	certificate := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw}))
	certificateServer.Close()
	for _, expected := range append(bootstrap, manifest.Objects...) {
		live := expected.DeepCopy()
		if live.GetKind() == "ValidatingWebhookConfiguration" || live.GetKind() == "MutatingWebhookConfiguration" {
			webhooks, _, _ := unstructured.NestedSlice(live.Object, "webhooks")
			for _, webhook := range webhooks {
				if err := unstructured.SetNestedField(webhook.(map[string]interface{}), certificate, "clientConfig", "caBundle"); err != nil {
					test.Fatal(err)
				}
			}
			if err := unstructured.SetNestedSlice(live.Object, webhooks, "webhooks"); err != nil {
				test.Fatal(err)
			}
		}
		if err := admin.Create(ctx, live); err != nil {
			test.Fatalf("create %s %s: %v", live.GetKind(), live.GetName(), err)
		}
		switch live.GetKind() {
		case "Deployment":
			replicas, _, _ := unstructured.NestedInt64(live.Object, "spec", "replicas")
			live.Object["status"] = map[string]interface{}{"observedGeneration": live.GetGeneration(), "replicas": replicas, "updatedReplicas": replicas, "readyReplicas": replicas, "availableReplicas": replicas}
		case "ValidatingAdmissionPolicy":
			live.Object["status"] = map[string]interface{}{"observedGeneration": live.GetGeneration(), "typeChecking": map[string]interface{}{}}
		default:
			continue
		}
		if err := admin.Status().Update(ctx, live); err != nil {
			test.Fatal(err)
		}
	}
	options := activation.ActivationOptions{Approval: client.ObjectKey{Namespace: "authority", Name: "approved"}, Endpoint: config.Host, ServerVersion: "v1.30.3", ChartSHA256: digest, ToolVersion: "test-only", Replicas: 1, WritersFrozen: true}
	makeApproval := func(name string, expired bool, target activation.ActivationTarget) *corev1.ConfigMap {
		test.Helper()
		now := time.Now().UTC().Add(-time.Second)
		if expired {
			now = now.Add(-time.Hour)
		}
		approval := activation.Approval{Version: activation.ApprovalVersion, Approver: "LOCAL-TEST-FIXTURE-NOT-ACCEPTANCE", IssuedAt: now, ExpiresAt: now.Add(20 * time.Minute), Target: target, Gates: map[string]activation.GateEvidence{}}
		targetHash, err := activation.TargetSHA256(target)
		if err != nil {
			test.Fatal(err)
		}
		data := map[string]string{}
		for _, gate := range []string{"SG-03", "SG-07", "SG-08", "SG-09"} {
			report := activation.AcceptanceReport{Version: activation.ApprovalVersion, Gate: gate, TargetSHA256: targetHash, TestVersion: "synthetic-fixture", AcceptedAt: now, Passed: true, Evidence: []string{digest}}
			encoded, err := json.Marshal(report)
			if err != nil {
				test.Fatal(err)
			}
			data[gate+".json"] = string(encoded)
			approval.Gates[gate] = activation.GateEvidence{ReportSHA256: fmt.Sprintf("%x", sha256.Sum256(encoded)), TestVersion: report.TestVersion, AcceptedAt: now}
		}
		encoded, err := json.Marshal(approval)
		if err != nil {
			test.Fatal(err)
		}
		data["approval.json"] = string(encoded)
		record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: name}, Immutable: ptr.To(true), Data: data}
		if err := admin.Create(ctx, record); err != nil {
			test.Fatal(err)
		}
		return record
	}
	prepare := func(name string, expired bool) *corev1.ConfigMap {
		test.Helper()
		options.Approval.Name = name
		target, err := candidate.Snapshot(ctx, admin, options.Endpoint, options.ServerVersion, options.ChartSHA256, options.ToolVersion, "authority", 1)
		if err != nil {
			test.Fatal(err)
		}
		return makeApproval(name, expired, target)
	}
	test.Run("built-command-prepare-and-dry-run", func(test *testing.T) {
		directory := test.TempDir()
		binary := filepath.Join(directory, "activation.exe")
		build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./tools/check-admission-activation")
		build.Dir = "../.."
		if output, err := build.CombinedOutput(); err != nil {
			test.Fatalf("build CLI: %v %s", err, output)
		}
		manifestPath, chartPath := filepath.Join(directory, "bootstrap.yaml"), filepath.Join(directory, "fixture-chart")
		if err := os.WriteFile(manifestPath, []byte(rendered.String()), 0600); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(chartPath, []byte("LOCAL-TEST-FIXTURE-NOT-ACCEPTANCE"), 0600); err != nil {
			test.Fatal(err)
		}
		kubeconfig := filepath.Join(directory, "target.kubeconfig")
		configuration := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"target": {Server: config.Host, CertificateAuthorityData: config.CAData}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"admin": {ClientCertificateData: config.CertData, ClientKeyData: config.KeyData}}, Contexts: map[string]*clientcmdapi.Context{"target": {Cluster: "target", AuthInfo: "admin"}}, CurrentContext: "target"}
		if err := clientcmd.WriteToFile(configuration, kubeconfig); err != nil {
			test.Fatal(err)
		}
		arguments := []string{"--manifest", manifestPath, "--chart", chartPath, "--kubeconfig", kubeconfig, "--release", "security-test", "--authority-namespace", "authority", "--replicas", "1", "--writers-frozen"}
		invoke := func(mode string, extra ...string) []byte {
			test.Helper()
			command := exec.CommandContext(ctx, binary, append(append([]string{mode}, arguments...), extra...)...)
			var diagnostic bytes.Buffer
			command.Stderr = &diagnostic
			output, err := command.Output()
			if err != nil {
				test.Fatalf("CLI %s: %v %s %s", mode, err, output, diagnostic.String())
			}
			return output
		}
		var prepared struct {
			Passed bool
			Result activation.ActivationTarget
		}
		if err := json.Unmarshal(invoke("prepare"), &prepared); err != nil {
			test.Fatal(err)
		}
		executable, err := os.ReadFile(binary)
		if err != nil {
			test.Fatal(err)
		}
		if !prepared.Passed || prepared.Result.ToolVersion != fmt.Sprintf("sha256:%x", sha256.Sum256(executable)) || prepared.Result.Endpoint != config.Host {
			test.Fatalf("incorrect command target: %+v", prepared)
		}
		approval := makeApproval("cli-approved", false, prepared.Result)
		var dryRun struct {
			Passed bool
			Result activation.Audit
		}
		if err := json.Unmarshal(invoke("activate", "--approval", approval.Name, "--dry-run"), &dryRun); err != nil {
			test.Fatal(err)
		}
		if !dryRun.Passed || dryRun.Result.Stage != "dry-run" || dryRun.Result.ApprovalUID != string(approval.UID) {
			test.Fatalf("CLI dry-run: %+v", dryRun)
		}
		maps := &corev1.ConfigMapList{}
		if err := admin.List(ctx, maps, client.InNamespace("authority")); err != nil {
			test.Fatal(err)
		}
		if len(maps.Items) != 1 {
			test.Fatalf("CLI dry-run mutated authority: %d records", len(maps.Items))
		}
		if err := admin.Delete(ctx, approval); err != nil {
			test.Fatal(err)
		}
	})
	rolloutFailure := false
	fault := interceptor.NewClient(admin, interceptor.Funcs{Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := delegate.Patch(ctx, object, patch, opts...); err != nil {
			return err
		}
		if object.GetName() != manifest.Deployment {
			return nil
		}
		live := &unstructured.Unstructured{}
		live.SetAPIVersion("apps/v1")
		live.SetKind("Deployment")
		if err := delegate.Get(ctx, client.ObjectKeyFromObject(object), live); err != nil {
			return err
		}
		replicas, _, _ := unstructured.NestedInt64(live.Object, "spec", "replicas")
		if rolloutFailure && replicas > 0 {
			return errors.New("injected lost scale response")
		}
		live.Object["status"] = map[string]interface{}{"observedGeneration": live.GetGeneration(), "replicas": replicas, "updatedReplicas": replicas, "readyReplicas": replicas, "availableReplicas": replicas}
		return delegate.Status().Update(ctx, live)
	}})
	record := prepare("approved", false)
	test.Run("missing-runtime-gate", func(test *testing.T) {
		missing := record.DeepCopy()
		missing.Name = "missing-runtime"
		missing.UID = ""
		missing.ResourceVersion = ""
		delete(missing.Data, "SG-07.json")
		if err := admin.Create(ctx, missing); err != nil {
			test.Fatal(err)
		}
		invalid := options
		invalid.Approval.Name = missing.Name
		if _, err := candidate.Activate(ctx, admin, config, invalid); err == nil {
			test.Fatal("missing runtime report authorized activation")
		}
		if err := admin.Delete(ctx, missing); err != nil {
			test.Fatal(err)
		}
	})
	test.Run("dry-run-does-not-consume", func(test *testing.T) {
		options.DryRun = true
		audit, err := candidate.Activate(ctx, admin, config, options)
		options.DryRun = false
		if err != nil || audit.Stage != "dry-run" {
			test.Fatalf("dry-run: %+v %v", audit, err)
		}
		maps := &corev1.ConfigMapList{}
		if err := admin.List(ctx, maps, client.InNamespace("authority")); err != nil {
			test.Fatal(err)
		}
		if len(maps.Items) != 1 {
			test.Fatalf("dry-run persisted records: %d", len(maps.Items))
		}
	})
	test.Run("activate-and-repeat-read-only", func(test *testing.T) {
		audit, err := candidate.Activate(ctx, fault, config, options)
		if err != nil || audit.Stage != "active" {
			test.Fatalf("activate: %+v %v", audit, err)
		}
		readOnly := interceptor.NewClient(admin, interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				test.Error("repeat patched state")
				return errors.New("write forbidden")
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				test.Error("repeat updated state")
				return errors.New("write forbidden")
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				test.Error("repeat deleted state")
				return errors.New("write forbidden")
			},
			Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				if _, ok := object.(*authv1.SubjectAccessReview); !ok {
					test.Errorf("repeat created %T", object)
					return errors.New("write forbidden")
				}
				return delegate.Create(ctx, object, opts...)
			},
		})
		repeated, err := candidate.Activate(ctx, readOnly, config, options)
		if err != nil || repeated.Stage != "active" || repeated.ApprovalUID != audit.ApprovalUID {
			test.Fatalf("repeat: %+v %v", repeated, err)
		}
		for _, drift := range []string{"policy-status", "guard-ready", "controller-ready", "binding-uid", "extra-grant", "object-uid", "object-annotation"} {
			test.Run(drift, func(test *testing.T) {
				injected := false
				changed := interceptor.NewClient(readOnly, interceptor.Funcs{
					Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if err := delegate.Get(ctx, key, object, opts...); err != nil {
							return err
						}
						switch live := object.(type) {
						case *unstructured.Unstructured:
							if drift == "policy-status" && live.GetKind() == "ValidatingAdmissionPolicy" {
								delete(live.Object, "status")
								injected = true
							}
							if drift == "object-uid" && live.GetKind() == "Deployment" {
								live.SetUID("recreated")
								injected = true
							}
							if drift == "object-annotation" && live.GetKind() == "Deployment" {
								live.SetAnnotations(map[string]string{"changed": "true"})
								injected = true
							}
						case *appsv1.Deployment:
							if (drift == "guard-ready" && key.Name == manifest.Guard) || (drift == "controller-ready" && key.Name == manifest.Deployment) {
								live.Status.ReadyReplicas = 0
								injected = true
							}
						case *rbacv1.RoleBinding:
							if drift == "binding-uid" {
								live.UID = "recreated"
								injected = true
							}
						}
						return nil
					},
					List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if err := delegate.List(ctx, list, opts...); err != nil {
							return err
						}
						if bindings, ok := list.(*rbacv1.ClusterRoleBindingList); ok && drift == "extra-grant" {
							bindings.Items = append(bindings.Items, rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "unexpected", UID: "unexpected"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:authenticated"}}})
							injected = true
						}
						return nil
					},
				})
				if _, err := candidate.Activate(ctx, changed, config, options); err == nil || !injected {
					test.Fatalf("repeat accepted %s: injected=%t error=%v", drift, injected, err)
				}
			})
		}
	})
	if test.Failed() {
		return
	}
	test.Run("recover-and-replay", func(test *testing.T) {
		substitute := record.DeepCopy()
		substitute.Name = "substitute"
		substitute.UID = ""
		substitute.ResourceVersion = ""
		if err := admin.Create(ctx, substitute); err != nil {
			test.Fatal(err)
		}
		invalid := options
		invalid.Approval.Name = substitute.Name
		if _, err := candidate.Recover(ctx, fault, invalid, true); err == nil || !strings.Contains(err.Error(), "does not match the journal") {
			test.Fatalf("substituted approval: %v", err)
		}
		if _, err := candidate.Recover(ctx, fault, options, false); err == nil {
			test.Fatal("recovery without stopped executors")
		}
		audit, err := candidate.Recover(ctx, fault, options, true)
		if err != nil || audit.Stage != "recovered-inactive" {
			test.Fatalf("recover: %+v %v", audit, err)
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatal(err)
		}
		if _, err := candidate.Activate(ctx, fault, config, options); err == nil {
			test.Fatal("replayed recovered approval")
		}
	})
	if test.Failed() {
		return
	}
	test.Run("lost-scale-response-compensates", func(test *testing.T) {
		prepare("after-recovery", false)
		rolloutFailure = true
		audit, err := candidate.Activate(ctx, fault, config, options)
		rolloutFailure = false
		if err == nil || audit.Stage != "failed-inactive" || !strings.Contains(audit.Failure, "injected lost scale response") {
			test.Fatalf("failure: %+v %v", audit, err)
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatal(err)
		}
		maps := &corev1.ConfigMapList{}
		if err := admin.List(ctx, maps, client.InNamespace("authority")); err != nil {
			test.Fatal(err)
		}
		archives := 0
		for _, record := range maps.Items {
			if strings.HasPrefix(record.Name, "activation-history-") && record.Immutable != nil && *record.Immutable {
				archives++
			}
		}
		if archives != 1 {
			test.Fatalf("immutable prior history missing: %d", archives)
		}
	})
	if test.Failed() {
		return
	}
	for _, failureMode := range []string{"rbac-create", "lost-rbac-response", "concurrent-drift", "guard-rollout", "rollout-read", "active-journal", "cleanup-response"} {
		test.Run(failureMode, func(test *testing.T) {
			if _, err := candidate.Recover(ctx, fault, options, true); err != nil {
				test.Fatal(err)
			}
			prepare("fault-"+failureMode, false)
			scaled := false
			granted := false
			injected := false
			broken := interceptor.NewClient(fault, interceptor.Funcs{
				Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					_, binding := object.(*rbacv1.RoleBinding)
					binding = binding && !strings.HasPrefix(object.GetName(), "kognition-admission-probe-")
					if binding {
						granted = true
					}
					if binding && !injected && (failureMode == "rbac-create" || failureMode == "lost-rbac-response") {
						injected = true
						if failureMode == "lost-rbac-response" {
							if err := delegate.Create(ctx, object, opts...); err != nil {
								return err
							}
						} else {
							return apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "rolebindings"}, object.GetName(), errors.New("injected rbac-create"))
						}
						return errors.New("injected " + failureMode)
					}
					return delegate.Create(ctx, object, opts...)
				},
				Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					err := delegate.Patch(ctx, object, patch, opts...)
					if object.GetName() == manifest.Deployment {
						if scaled && failureMode == "cleanup-response" {
							return errors.Join(err, errors.New("injected cleanup response"))
						}
						scaled = true
					}
					return err
				},
				Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if scaled && !injected && (failureMode == "rollout-read" || failureMode == "cleanup-response") && key.Name == manifest.Deployment {
						injected = true
						return errors.New("injected rollout read")
					}
					if err := delegate.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if granted && !injected && failureMode == "concurrent-drift" {
						if live, ok := object.(*unstructured.Unstructured); ok {
							live.SetAnnotations(map[string]string{"concurrent-writer": "changed"})
							injected = true
						}
					}
					if scaled && failureMode == "guard-rollout" && key.Name == manifest.Guard {
						if live, ok := object.(*appsv1.Deployment); ok {
							live.Status.ReadyReplicas = 0
							injected = true
						}
					}
					return nil
				},
				Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if record, ok := object.(*corev1.ConfigMap); ok && failureMode == "active-journal" && !injected && record.Namespace == "authority" {
						var audit activation.Audit
						if err := json.Unmarshal([]byte(record.Data["audit.json"]), &audit); err != nil {
							return err
						}
						if audit.Stage == "active" {
							injected = true
							return errors.New("injected active journal write")
						}
					}
					return delegate.Update(ctx, object, opts...)
				},
			})
			audit, err := candidate.Activate(ctx, broken, config, options)
			stage := "failed-inactive"
			if failureMode == "cleanup-response" {
				stage = "recovery-required"
			}
			if !injected || err == nil || audit.Stage != stage || audit.Failure == "" {
				test.Fatalf("fault not safely handled: injected=%t stage=%s error=%v", injected, audit.Stage, err)
			}
			if failureMode == "concurrent-drift" && !strings.Contains(audit.Failure, "inventory drift") {
				test.Fatalf("wrong drift failure: %s", audit.Failure)
			}
			if failureMode == "cleanup-response" && !strings.Contains(audit.Failure, "injected cleanup response") {
				test.Fatal("audit omitted cleanup uncertainty")
			}
			if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
				test.Fatal(err)
			}
		})
		if test.Failed() {
			return
		}
	}
	test.Run("late-grant-create", func(test *testing.T) {
		if _, err := candidate.Recover(ctx, fault, options, true); err != nil {
			test.Fatal(err)
		}
		prepare("late-grant", false)
		var delayed client.Object
		broken := interceptor.NewClient(fault, interceptor.Funcs{Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if _, binding := object.(*rbacv1.RoleBinding); binding && !strings.HasPrefix(object.GetName(), "kognition-admission-probe-") && delayed == nil {
				var ok bool
				delayed, ok = object.DeepCopyObject().(client.Object)
				if !ok {
					test.Fatal("invalid grant copy")
				}
				return errors.New("injected request still in flight")
			}
			return delegate.Create(ctx, object, opts...)
		}})
		audit, err := candidate.Activate(ctx, broken, config, options)
		if err == nil || audit.Stage != "recovery-required" || delayed == nil {
			test.Fatalf("uncertain Create falsely completed: %s %v", audit.Stage, err)
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatal(err)
		}
		audit, err = candidate.Recover(ctx, fault, options, true)
		if err == nil || audit.Stage != "recovery-required" || !strings.Contains(err.Error(), "unresolved Create") {
			test.Fatalf("NotFound resolved a pending request: %s %v", audit.Stage, err)
		}
		if err := admin.Create(ctx, delayed); err != nil {
			test.Fatal(err)
		}
		audit, err = candidate.Recover(ctx, fault, options, true)
		if err != nil || audit.Stage != "recovered-inactive" {
			test.Fatalf("late persistence recovery: %s %v", audit.Stage, err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(delayed), delayed); !apierrors.IsNotFound(err) {
			test.Fatalf("late grant survived recovery: %v", err)
		}
	})
	if test.Failed() {
		return
	}
	for _, interruptedStage := range []string{"protection", "protection-binding", "fixture", "role", "rolebinding"} {
		test.Run("interrupted-probe-"+interruptedStage, func(test *testing.T) {
			if _, err := candidate.Recover(ctx, fault, options, true); err != nil {
				test.Fatal(err)
			}
			prepare("interrupted-probe-"+interruptedStage, false)
			interruption := errors.New("injected executor interruption")
			var interrupted bool
			broken := interceptor.NewClient(fault, interceptor.Funcs{
				Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if err := delegate.Create(ctx, object, opts...); err != nil {
						return err
					}
					var stage string
					switch object.(type) {
					case *admissionv1.ValidatingAdmissionPolicy:
						stage = "protection"
					case *admissionv1.ValidatingAdmissionPolicyBinding:
						stage = "protection-binding"
					case *batchv1.Job:
						stage = "fixture"
					case *rbacv1.Role:
						stage = "role"
					case *rbacv1.RoleBinding:
						stage = "rolebinding"
					}
					if stage == interruptedStage {
						panic(interruption)
					}
					return nil
				},
				Delete: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					if object.GetAnnotations()["kognition.dev/activation-transaction"] != "" {
						return errors.New("interrupted executor cannot clean up")
					}
					return delegate.Delete(ctx, object, opts...)
				},
				Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if record, ok := object.(*corev1.ConfigMap); ok && record.Namespace == "authority" {
						var audit activation.Audit
						if err := json.Unmarshal([]byte(record.Data["audit.json"]), &audit); err != nil {
							return err
						}
						if len(audit.Resources) > 0 && audit.Resources[len(audit.Resources)-1].UID != "" {
							last := audit.Resources[len(audit.Resources)-1]
							if last.Kind == "ValidatingAdmissionPolicy" && interruptedStage == "protection" || last.Kind == "ValidatingAdmissionPolicyBinding" && interruptedStage == "protection-binding" || last.Kind == "Job" && interruptedStage == "fixture" || last.Kind == "Role" && interruptedStage == "role" || last.Kind == "RoleBinding" && interruptedStage == "rolebinding" {
								return errors.New("interrupted executor cannot persist returned UID")
							}
						}
					}
					return delegate.Update(ctx, object, opts...)
				},
			})
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						recoveredErr, ok := recovered.(error)
						if !ok || !errors.Is(recoveredErr, interruption) {
							panic(recovered)
						}
						interrupted = true
					}
				}()
				if _, err := candidate.Activate(ctx, broken, config, options); err != nil {
					test.Fatalf("did not reach interruption: %v", err)
				}
			}()
			if !interrupted {
				test.Fatal("probe stage was not interrupted")
			}
			fresh, err := client.New(config, client.Options{Scheme: scheme.Scheme})
			if err != nil {
				test.Fatal(err)
			}
			audit, err := candidate.Recover(ctx, fresh, options, true)
			if err != nil || audit.Stage != "recovered-inactive" || len(audit.Resources) == 0 {
				test.Fatalf("fresh executor recovery failed: %s %v", audit.Stage, err)
			}
			for _, resource := range audit.Resources {
				object := &unstructured.Unstructured{}
				object.SetGroupVersionKind(schema.FromAPIVersionAndKind(resource.APIVersion, resource.Kind))
				if err := fresh.Get(ctx, client.ObjectKey{Namespace: resource.Namespace, Name: resource.Name}, object); !apierrors.IsNotFound(err) {
					test.Fatalf("probe survived recovery: %s %s: %v", resource.Kind, resource.Name, err)
				}
			}
		})
		if test.Failed() {
			return
		}
	}
	test.Run("expired-interrupted-recovery", func(test *testing.T) {
		if _, err := candidate.Recover(ctx, fault, options, true); err != nil {
			test.Fatal(err)
		}
		expired := prepare("expired-interrupted", true)
		if _, err := candidate.Activate(ctx, fault, config, options); err == nil {
			test.Fatal("expired approval activated")
		}
		maps := &corev1.ConfigMapList{}
		if err := admin.List(ctx, maps, client.InNamespace("authority")); err != nil {
			test.Fatal(err)
		}
		for _, journal := range maps.Items {
			if !strings.HasPrefix(journal.Name, "activation-") || strings.HasPrefix(journal.Name, "activation-history-") {
				continue
			}
			var approval activation.Approval
			if err := json.Unmarshal([]byte(expired.Data["approval.json"]), &approval); err != nil {
				test.Fatal(err)
			}
			audit := activation.Audit{ApprovalUID: string(expired.UID), Approval: approval, Stage: "behavior-verification", Started: approval.IssuedAt, Updated: approval.IssuedAt, ResourceJournalVersion: 1}
			encoded, err := json.Marshal(audit)
			if err != nil {
				test.Fatal(err)
			}
			journal.Data["audit.json"] = string(encoded)
			if err := admin.Update(ctx, &journal); err != nil {
				test.Fatal(err)
			}
		}
		audit, err := candidate.Recover(ctx, fault, options, true)
		if err != nil || audit.Stage != "recovered-inactive" {
			test.Fatalf("expired interrupted recovery: %+v %v", audit, err)
		}
	})
	test.Log("Real local API defaulting, persistence, authorization and policy probes; synthetic acceptance and Deployment readiness only. No deployed acceptance gate is satisfied.")
}
