// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestArgoApplicationPreservesGuardedActivation(test *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		test.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "deploy", "argocd-application.yaml"))
	if err != nil {
		test.Fatal(err)
	}
	var application struct {
		Metadata struct {
			Name        string            `json:"name"`
			Finalizers  []string          `json:"finalizers"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Project string `json:"project"`
			Source  struct {
				RepoURL        string `json:"repoURL"`
				TargetRevision string `json:"targetRevision"`
				Helm           struct {
					ReleaseName string `json:"releaseName"`
					SkipCRDs    bool   `json:"skipCrds"`
					Parameters  []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"parameters"`
				} `json:"helm"`
			} `json:"source"`
			Destination struct {
				Namespace string `json:"namespace"`
			} `json:"destination"`
			SyncPolicy struct {
				Automated   map[string]any `json:"automated"`
				SyncOptions []string       `json:"syncOptions"`
			} `json:"syncPolicy"`
			IgnoreDifferences []struct {
				Group        string   `json:"group"`
				Kind         string   `json:"kind"`
				Name         string   `json:"name"`
				Namespace    string   `json:"namespace"`
				JSONPointers []string `json:"jsonPointers"`
			} `json:"ignoreDifferences"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(body, &application); err != nil {
		test.Fatal(err)
	}
	if application.Spec.Project != "kognition-installation" || len(application.Metadata.Finalizers) != 0 {
		test.Error("GitOps must use a reviewed installation project without cascading deletion")
	}
	if application.Spec.Source.TargetRevision != "REPLACE_WITH_REVIEWED_COMMIT_SHA" {
		test.Error("the example must require an explicitly reviewed immutable source revision")
	}
	if application.Spec.Source.RepoURL != "https://github.com/microsoft/kognition.git" {
		test.Error("the example must identify the official Microsoft source repository")
	}
	if application.Spec.SyncPolicy.Automated != nil {
		test.Error("automatic prune/selfHeal can race the required external writer freeze")
	}
	if application.Metadata.Annotations["argocd.argoproj.io/compare-options"] != "ServerSideDiff=true" {
		test.Error("GitOps must compare API defaults through dry-run rather than ignoring desired fields")
	}
	options := "|" + strings.Join(application.Spec.SyncPolicy.SyncOptions, "|") + "|"
	for _, required := range []string{"Prune=false", "Delete=false", "CreateNamespace=false", "ServerSideApply=true", "FailOnSharedResource=true", "RespectIgnoreDifferences=true"} {
		if !strings.Contains(options, "|"+required+"|") {
			test.Errorf("guarded Argo sync is missing %s", required)
		}
	}
	parameters := map[string]string{}
	for _, parameter := range application.Spec.Source.Helm.Parameters {
		parameters[parameter.Name] = parameter.Value
	}
	for name, required := range map[string]string{"mode": "kubernetes", "k8sController.replicas": "0", "k8sController.activated": "false", "webhook.enabled": "true", "webhook.failurePolicy": "Fail", "migrations.retainCompletedJobs": "true", "networkPolicy.ciliumApiServerEgress": "true"} {
		if parameters[name] != required {
			test.Errorf("GitOps must retain inactive fail-closed %s=%s", name, required)
		}
	}
	if !application.Spec.Source.Helm.SkipCRDs || application.Spec.Source.Helm.ReleaseName != application.Metadata.Name {
		test.Error("CRD enrollment must be separate, with an exact release/tracking identity")
	}
	if len(application.Spec.IgnoreDifferences) != 1 {
		test.Fatal("only the exact activation-owned controller replicas field may be ignored")
	}
	ignored := application.Spec.IgnoreDifferences[0]
	if ignored.Group != "apps" || ignored.Kind != "Deployment" || ignored.Name != application.Metadata.Name+"-k8s-controller" ||
		ignored.Namespace != application.Spec.Destination.Namespace || !reflect.DeepEqual(ignored.JSONPointers, []string{"/spec/replicas"}) {
		test.Error("GitOps ignoreDifferences must not mask security, RBAC, image or admission drift")
	}
}
