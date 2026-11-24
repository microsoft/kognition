// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
