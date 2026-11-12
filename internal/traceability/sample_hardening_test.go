// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestBasePlatformDoesNotInstallSampleMCPServers(test *testing.T) {
	for _, mode := range []string{"standalone", "kubernetes"} {
		test.Run(mode, func(test *testing.T) {
			for _, document := range renderControllerChart(test, "--set", "mode="+mode) {
				for _, sample := range []string{"samples-tool-sandbox", "tool-sandbox-src", "sandbox-mcp", "sandbox-openapi"} {
					if strings.Contains(string(document.Raw), sample) {
						test.Errorf("base %s chart includes sample server resource %q", mode, sample)
					}
				}
			}
		})
	}
}

func TestUmbrellaPlatformChartMatchesSource(test *testing.T) {
	archive, err := os.Open("../../samples/helm-umbrella/charts/kognition-0.1.0.tgz")
	if err != nil {
		test.Fatal(err)
	}
	defer archive.Close()
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		test.Fatal(err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	sourceRoot := "../../deploy/helm/kognition"
	expected := map[string]bool{}
	for _, name := range []string{"LICENSE.TXT", "NOTICE", "PRIVACY"} {
		expected[name] = false
	}
	err = filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		expected[filepath.ToSlash(relative)] = false
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			test.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		path, err := filepath.Rel("kognition", header.Name)
		if err != nil {
			test.Fatal(err)
		}
		path = filepath.ToSlash(path)
		found, known := expected[path]
		if !known || found || header.Typeflag != tar.TypeReg {
			test.Fatalf("unexpected, duplicate or nonregular archive member %q", header.Name)
		}
		for path := range expected {
			if header.Name != "kognition/"+path {
				continue
			}
			bundled, err := io.ReadAll(reader)
			if err != nil {
				test.Fatal(err)
			}
			sourcePath := filepath.Join(sourceRoot, filepath.FromSlash(path))
			if path == "LICENSE.TXT" || path == "NOTICE" || path == "PRIVACY" {
				sourcePath = filepath.Join("../..", path)
			}
			source, err := os.ReadFile(sourcePath)
			if err != nil {
				test.Fatal(err)
			}
			if path == "Chart.yaml" {
				bundled, err = yaml.ToJSON(bundled)
				if err != nil {
					test.Fatal(err)
				}
				source, err = yaml.ToJSON(source)
				if err != nil {
					test.Fatal(err)
				}
			}
			if !bytes.Equal(bytes.ReplaceAll(bundled, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))) {
				test.Errorf("bundled %s differs from source; run helm package deploy/helm/kognition --destination samples/helm-umbrella/charts", path)
			}
			expected[path] = true
		}
	}
	for path, found := range expected {
		if !found {
			test.Errorf("bundled chart is missing %s", path)
		}
	}
}

func TestSampleDeploymentsUseRestrictedSecurityContexts(t *testing.T) {
	for _, path := range []string{
		"../../deploy/observability/jaeger.yaml",
		"../../samples/knowledge-base-blob-azure/azurite.yaml",
		"../../samples/knowledge-base-blob-s3/s3-store.yaml",
		"../../samples/knowledge-base-kognition-docs/docs-server.yaml",
		"../../samples/workflows/tool-sandbox.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
			found := false
			for {
				var document runtime.RawExtension
				if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if len(document.Raw) == 0 {
					continue
				}
				var metadata metav1.TypeMeta
				if err := json.Unmarshal(document.Raw, &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata.Kind != "Deployment" {
					continue
				}
				found = true
				var deployment appsv1.Deployment
				if err := json.Unmarshal(document.Raw, &deployment); err != nil {
					t.Fatal(err)
				}
				pod := deployment.Spec.Template.Spec
				security := pod.SecurityContext
				if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.RunAsUser == nil || *security.RunAsUser <= 0 {
					t.Fatal("sample must run as an explicit non-root user")
				}
				if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					t.Fatal("sample must use the runtime default seccomp profile")
				}
				for _, container := range append(pod.InitContainers, pod.Containers...) {
					security := container.SecurityContext
					if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
						t.Fatalf("%s must use a read-only filesystem without privilege escalation", container.Name)
					}
					if security.Capabilities == nil || !slices.Contains(security.Capabilities.Drop, corev1.Capability("ALL")) {
						t.Fatalf("%s must drop all capabilities", container.Name)
					}
					for _, port := range container.Ports {
						if port.ContainerPort < 1024 {
							t.Fatalf("%s must not require a privileged port", container.Name)
						}
					}
				}
			}
			if !found {
				t.Fatal("sample contains no Deployment")
			}
		})
	}
}
