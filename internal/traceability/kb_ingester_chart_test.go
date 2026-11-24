// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func kbIngesterEnvironment(test *testing.T, extra ...string) map[string]string {
	test.Helper()
	for _, document := range renderControllerChart(test, append([]string{"--set", "mode=standalone", "--set", "kbIngester.enabled=true"}, extra...)...) {
		var meta struct{ Kind string }
		if err := json.Unmarshal(document.Raw, &meta); err != nil {
			test.Fatal(err)
		}
		if meta.Kind != "Deployment" {
			continue
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(document.Raw, &deployment); err != nil {
			test.Fatal(err)
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name != "kb-ingester" {
				continue
			}
			environment := map[string]string{}
			for _, variable := range container.Env {
				environment[variable.Name] = variable.Value
			}
			return environment
		}
	}
	test.Fatal("kb-ingester container was not rendered")
	return nil
}

func TestKBIngesterTrustsNoEmbeddingHostsByDefault(test *testing.T) {
	environment := kbIngesterEnvironment(test)
	if value, found := environment["KOGNITION_INGEST_TRUSTED_EMBEDDING_HOSTS"]; found {
		test.Fatalf("default chart must not trust extra embedding hosts, got %q", value)
	}
	if environment["KOGNITION_INGEST_LISTEN"] != "127.0.0.1:8082" {
		test.Fatalf("ingester must listen on loopback only, got %q", environment["KOGNITION_INGEST_LISTEN"])
	}
}

func TestKBIngesterTrustedEmbeddingHostsAreConfigurable(test *testing.T) {
	environment := kbIngesterEnvironment(test,
		"--set", "kbIngester.trustedEmbeddingHosts[0]=embed.internal",
		"--set", "kbIngester.trustedEmbeddingHosts[1]=apim.example.com",
	)
	if got := environment["KOGNITION_INGEST_TRUSTED_EMBEDDING_HOSTS"]; got != "embed.internal,apim.example.com" {
		test.Fatalf("trusted embedding hosts = %q", got)
	}
}

func kbIngesterContainers(test *testing.T, extra ...string) map[string]corev1.Container {
	test.Helper()
	for _, document := range renderControllerChart(test, append([]string{"--set", "mode=standalone", "--set", "kbIngester.enabled=true"}, extra...)...) {
		var meta struct{ Kind string }
		if err := json.Unmarshal(document.Raw, &meta); err != nil {
			test.Fatal(err)
		}
		if meta.Kind != "Deployment" {
			continue
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(document.Raw, &deployment); err != nil {
			test.Fatal(err)
		}
		containers := map[string]corev1.Container{}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			containers[container.Name] = container
		}
		if _, found := containers["kb-ingester"]; found {
			return containers
		}
	}
	test.Fatal("kb-ingester container was not rendered")
	return nil
}

func secretEnvironmentReference(container corev1.Container, name string) (string, string, bool) {
	for _, variable := range container.Env {
		if variable.Name == name && variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
			reference := variable.ValueFrom.SecretKeyRef
			return reference.Name, reference.Key, true
		}
	}
	return "", "", false
}

func TestKBIngesterRequiresNoTokenAndNoStrictModeByDefault(test *testing.T) {
	containers := kbIngesterContainers(test)
	for container, names := range map[string][]string{
		"kb-ingester":        {"KOGNITION_INGEST_AUTH_TOKEN", "KOGNITION_INGEST_STRICT_AZURE_HOSTS"},
		"controller-manager": {"KOGNITION_KB_INGESTER_AUTH_TOKEN"},
	} {
		for _, variable := range containers[container].Env {
			for _, name := range names {
				if variable.Name == name {
					test.Fatalf("%s must not set %s by default", container, name)
				}
			}
		}
	}
}

func TestKBIngesterAuthSecretIsInjectedIntoBothContainers(test *testing.T) {
	containers := kbIngesterContainers(test,
		"--set", "kbIngester.auth.existingSecret=ingester-token",
		"--set", "kbIngester.auth.key=bearer",
		"--set", "kbIngester.strictAzureHosts=true",
	)
	for container, name := range map[string]string{
		"kb-ingester":        "KOGNITION_INGEST_AUTH_TOKEN",
		"controller-manager": "KOGNITION_KB_INGESTER_AUTH_TOKEN",
	} {
		secret, key, found := secretEnvironmentReference(containers[container], name)
		if !found || secret != "ingester-token" || key != "bearer" {
			test.Fatalf("%s %s secret reference = (%q, %q, %v)", container, name, secret, key, found)
		}
	}
	strict := ""
	for _, variable := range containers["kb-ingester"].Env {
		if variable.Name == "KOGNITION_INGEST_STRICT_AZURE_HOSTS" {
			strict = variable.Value
		}
	}
	if strict != "true" {
		test.Fatalf("strict Azure hosts = %q, want true", strict)
	}
}
