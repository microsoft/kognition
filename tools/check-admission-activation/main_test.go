// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestCommandRequiresExplicitInputs(test *testing.T) {
	for _, arguments := range [][]string{nil, {"--manifest", "bootstrap.yaml"}, {"--kubeconfig", "local.config"}, {"--unknown"}, {"extra"}} {
		var output bytes.Buffer
		if code := run(arguments, &output, &output); code != 2 {
			test.Fatalf("arguments %v returned %d", arguments, code)
		}
	}
}

func TestPolicyCommandRequiresFreezeAndManifest(test *testing.T) {
	for _, arguments := range [][]string{
		{"--policy-behavior", "--manifest", "bootstrap.yaml", "--kubeconfig", "explicit"},
		{"--writers-frozen", "--manifest", "bootstrap.yaml", "--kubeconfig", "explicit"},
		{"--policy-behavior", "--writers-frozen", "--kubeconfig", "explicit"},
		{"--drain-only", "--policy-behavior", "--writers-frozen", "--kubeconfig", "explicit", "--namespace", "control", "--controller-service-account", "controller", "--controller-deployment", "controller", "--workload-namespaces", "tenant"},
	} {
		var output bytes.Buffer
		if code := run(arguments, &output, &output); code != 2 {
			test.Fatalf("arguments %v returned %d: %s", arguments, code, output.String())
		}
	}
}

func TestDrainCommandModes(test *testing.T) {
	complete := []string{"--drain-only", "--kubeconfig", "explicit", "--namespace", "control", "--controller-service-account", "controller", "--controller-deployment", "controller", "--workload-namespaces", "tenant"}
	for _, args := range [][]string{
		{"--drain-only", "--kubeconfig", "explicit"},
		append(append([]string{}, complete...), "--manifest", "bootstrap.yaml"),
		{"--manifest", "bootstrap.yaml", "--kubeconfig", "explicit", "--namespace", "control"},
		complete[:len(complete)-2],
	} {
		var output bytes.Buffer
		if code := run(args, &output, &output); code != 2 {
			test.Fatalf("args %v returned %d: %s", args, code, output.String())
		}
	}
}

func TestDrainCommandReadOnly(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		pod       bool
		forbidden bool
		code      int
		message   string
	}{
		{name: "fresh", message: "Drain verified"},
		{name: "retained-pod", pod: true, code: 1, message: "drain workload Pod tenant/retained"},
		{name: "list-forbidden", forbidden: true, code: 1, message: "forbidden"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			resources := map[string]string{"pods": "Pod", "replicationcontrollers": "ReplicationController", "deployments": "Deployment", "replicasets": "ReplicaSet", "statefulsets": "StatefulSet", "daemonsets": "DaemonSet", "jobs": "Job", "cronjobs": "CronJob", "rolebindings": "RoleBinding", "clusterrolebindings": "ClusterRoleBinding"}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				if request.Method != http.MethodGet {
					test.Errorf("unexpected mutation %s %s", request.Method, request.URL.Path)
					response.WriteHeader(500)
					return
				}
				path := request.URL.Path
				var result interface{}
				switch path {
				case "/api":
					result = map[string]interface{}{"kind": "APIVersions", "apiVersion": "v1", "versions": []string{"v1"}}
				case "/apis":
					groups := []interface{}{}
					for _, name := range []string{"apps", "batch", "rbac.authorization.k8s.io"} {
						version := map[string]string{"groupVersion": name + "/v1", "version": "v1"}
						groups = append(groups, map[string]interface{}{"name": name, "versions": []interface{}{version}, "preferredVersion": version})
					}
					result = map[string]interface{}{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups}
				case "/api/v1", "/apis/apps/v1", "/apis/batch/v1", "/apis/rbac.authorization.k8s.io/v1":
					groupVersion := strings.TrimPrefix(strings.TrimPrefix(path, "/apis/"), "/api/")
					entries := []interface{}{}
					for name, kind := range resources {
						entries = append(entries, map[string]interface{}{"name": name, "kind": kind, "namespaced": name != "clusterrolebindings", "verbs": []string{"get", "list"}})
					}
					result = map[string]interface{}{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": groupVersion, "resources": entries}
				default:
					if strings.HasSuffix(path, "/deployments/controller") {
						response.WriteHeader(404)
						result = map[string]interface{}{"kind": "Status", "apiVersion": "v1", "reason": "NotFound", "code": 404}
						break
					}
					resource := path[strings.LastIndex(path, "/")+1:]
					kind, exists := resources[resource]
					if !exists {
						test.Errorf("unexpected read %s", path)
						response.WriteHeader(500)
						return
					}
					if scenario.forbidden && resource == "pods" {
						response.WriteHeader(403)
						result = map[string]interface{}{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Forbidden", "message": "pods forbidden", "code": 403}
						break
					}
					version := "v1"
					if strings.HasPrefix(path, "/apis/") {
						pieces := strings.Split(path, "/")
						version = pieces[2] + "/" + pieces[3]
					}
					items := []interface{}{}
					if scenario.pod && resource == "pods" {
						items = append(items, map[string]interface{}{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]interface{}{"name": "retained", "namespace": "tenant"}})
					}
					result = map[string]interface{}{"kind": kind + "List", "apiVersion": version, "items": items}
				}
				if err := json.NewEncoder(response).Encode(result); err != nil {
					test.Error(err)
				}
			}))
			defer server.Close()
			path := filepath.Join(test.TempDir(), "target.kubeconfig")
			configuration := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"target": {Server: server.URL}}, Contexts: map[string]*clientcmdapi.Context{"target": {Cluster: "target"}}, CurrentContext: "target"}
			if err := clientcmd.WriteToFile(configuration, path); err != nil {
				test.Fatal(err)
			}
			test.Setenv("KUBECONFIG", filepath.Join(test.TempDir(), "must-not-be-used"))
			var output bytes.Buffer
			code := run([]string{"--drain-only", "--kubeconfig", path, "--namespace", "control", "--controller-service-account", "controller", "--controller-deployment", "controller", "--workload-namespaces", "tenant"}, &output, &output)
			if code != scenario.code || !strings.Contains(output.String(), scenario.message) {
				test.Fatalf("code=%d, output=%s", code, output.String())
			}
		})
	}
}

func TestCommandRejectsMissingManifest(test *testing.T) {
	var output bytes.Buffer
	if code := run([]string{"--manifest", test.TempDir() + "/missing", "--kubeconfig", "unused"}, &output, &output); code != 1 {
		test.Fatalf("returned %d", code)
	}
}

func TestPolicyReportEarlyFailure(test *testing.T) {
	var output, diagnostic bytes.Buffer
	code := run([]string{"--policy-behavior", "--writers-frozen", "--manifest", filepath.Join(test.TempDir(), "missing"), "--kubeconfig", "unused"}, &output, &diagnostic)
	var report policyReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		test.Fatal(err)
	}
	if code != 1 || report.Passed || report.Error == "" || report.Started.IsZero() || report.Finished.Before(report.Started) || len(report.SeparateGates) != 4 || diagnostic.Len() == 0 {
		test.Fatalf("incomplete failure evidence: code=%d report=%+v diagnostic=%s", code, report, diagnostic.String())
	}
}

func TestPolicyReportOutcomes(test *testing.T) {
	for _, failure := range []error{nil, errors.New("probe denied unexpectedly")} {
		report := policyReport{Started: time.Now().UTC(), Endpoint: "https://127.0.0.1:6443", Version: "v1.30.3", Controller: "system:serviceaccount:control:controller", ManifestSHA256: "manifest-hash", SeparateGates: []string{"activation"}, Probes: []activation.PolicyProbeResult{{Policy: "policy", Namespace: "tenant", Name: "probe", UID: "owned-uid", Allowed: true, Denied: true, CleanupConfirmed: true}}}
		var output bytes.Buffer
		if err := writePolicyReport(&output, report, failure); !errors.Is(err, failure) {
			test.Fatalf("verification error lost: %v", err)
		}
		var decoded policyReport
		if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
			test.Fatal(err)
		}
		if decoded.Passed != (failure == nil) || decoded.Endpoint != report.Endpoint || decoded.Version != report.Version || decoded.Controller != report.Controller || decoded.ManifestSHA256 != report.ManifestSHA256 || len(decoded.Probes) != 1 || decoded.Probes[0] != report.Probes[0] || decoded.Finished.Before(decoded.Started) || (decoded.Error != "") != (failure != nil) {
			test.Fatalf("report lost evidence: %+v", decoded)
		}
		writeErr := errors.New("report output unavailable")
		if err := writePolicyReport(policyErrorWriter{writeErr}, report, failure); !errors.Is(err, writeErr) || failure != nil && !errors.Is(err, failure) {
			test.Fatalf("encoding or verification error lost: %v", err)
		}
	}
}

type policyErrorWriter struct{ err error }

func (writer policyErrorWriter) Write([]byte) (int, error) { return 0, writer.err }
