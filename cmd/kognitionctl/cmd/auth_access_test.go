// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// useFakeAccessClient puts the CLI in Kubernetes mode against a fake cluster whose
// authorizer allows only the actions the callback accepts.
func useFakeAccessClient(t *testing.T, allow func(authorizationv1.ResourceAttributes) bool, rules *authorizationv1.SubjectRulesReviewStatus) {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = allow(*review.Spec.ResourceAttributes)
		return true, review, nil
	})
	client.PrependReactor("create", "selfsubjectrulesreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
		if rules != nil {
			review.Status = *rules.DeepCopy()
		}
		return true, review, nil
	})
	previousClient, previousMode, previousNamespace, previousFormat := k8sCoreClient, k8sMode, namespace, outputFormat
	k8sCoreClient, k8sMode, namespace, outputFormat = client, true, "team-a", ""
	t.Cleanup(func() {
		k8sCoreClient, k8sMode, namespace, outputFormat = previousClient, previousMode, previousNamespace, previousFormat
	})
}

func runAuth(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	subcommands := map[string]*cobra.Command{"can-i": authCanICmd, "list": authListCmd}
	command := subcommands[args[0]]
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	if err := command.ParseFlags(args[1:]); err != nil {
		return "", "", err
	}
	positional := command.Flags().Args()
	// Cobra validates positional arguments before RunE; do the same here.
	if command.Args != nil {
		if err := command.Args(command, positional); err != nil {
			return "", "", err
		}
	}
	err := command.RunE(command, positional)
	// Flags persist between runs on the shared command, so reset them.
	command.Flags().VisitAll(func(flag *pflag.Flag) { _ = flag.Value.Set(flag.DefValue) })
	return stdout.String(), stderr.String(), err
}

func TestAuthCanIPrintsYesOrNoAndSignalsDenialWithoutNoise(t *testing.T) {
	useFakeAccessClient(t, func(attributes authorizationv1.ResourceAttributes) bool {
		return attributes.Verb == "use" && attributes.Name == "flow" && attributes.Namespace == "team-a"
	}, nil)

	stdout, _, err := runAuth(t, "can-i", "use", "workflow/flow")
	if err != nil || strings.TrimSpace(stdout) != "yes" {
		t.Fatalf("stdout = %q, err = %v; want yes", stdout, err)
	}

	stdout, _, err = runAuth(t, "can-i", "use", "workflow/other")
	if !errors.Is(err, ErrAccessDenied) || strings.TrimSpace(stdout) != "no" {
		t.Fatalf("stdout = %q, err = %v; want no and ErrAccessDenied (exit 1 without an error message)", stdout, err)
	}
}

func TestAuthCanIParsesKindNameAndSubresource(t *testing.T) {
	var seen authorizationv1.ResourceAttributes
	useFakeAccessClient(t, func(attributes authorizationv1.ResourceAttributes) bool { seen = attributes; return true }, nil)

	if _, _, err := runAuth(t, "can-i", "approve", "RuntimeApproval/ra-1", "--subresource", "decision"); err != nil {
		t.Fatal(err)
	}
	if seen.Resource != "runtimeapprovals" || seen.Name != "ra-1" || seen.Subresource != "decision" || seen.Verb != "approve" || seen.Group != "kognition.dev" {
		t.Fatalf("attributes = %+v", seen)
	}
	if _, _, err := runAuth(t, "can-i", "create", "agentjobs"); err != nil || seen.Name != "" || seen.Resource != "agentjobs" {
		t.Fatalf("a kind without a name must not send one: %+v, %v", seen, err)
	}
	for _, args := range [][]string{{"can-i", "get", "pods"}, {"can-i", "get", "agent/a/b"}, {"can-i", "get"}} {
		if _, _, err := runAuth(t, args...); err == nil || errors.Is(err, ErrAccessDenied) {
			t.Errorf("args %v: err = %v, want a usage error", args, err)
		}
	}
}

func TestAuthCanIJSON(t *testing.T) {
	useFakeAccessClient(t, func(authorizationv1.ResourceAttributes) bool { return true }, nil)
	outputFormat = "json"
	stdout, _, err := runAuth(t, "can-i", "get", "agent")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || !decoded.Allowed {
		t.Fatalf("stdout = %q, err = %v", stdout, err)
	}
}

func TestAuthListShowsOnlyKognitionRulesAndFlagsAdvisoryLimits(t *testing.T) {
	useFakeAccessClient(t, nil, &authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{
			{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflows"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflows"}, Verbs: []string{"use"}, ResourceNames: []string{"approved"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
		},
		Incomplete: true, EvaluationError: "authorizer unavailable",
	})
	stdout, stderr, err := runAuth(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "workflows") || !strings.Contains(stdout, "get,list") || !strings.Contains(stdout, "approved") {
		t.Fatalf("stdout missing rules:\n%s", stdout)
	}
	if strings.Contains(stdout, "secrets") {
		t.Fatalf("a non-Kognition rule was shown:\n%s", stdout)
	}
	combined := stdout + stderr
	for _, want := range []string{"team-a", "advisory", "incomplete", "authorizer unavailable", "cluster-scoped"} {
		if !strings.Contains(strings.ToLower(combined), strings.ToLower(want)) {
			t.Errorf("output does not mention %q:\n%s", want, combined)
		}
	}
}

func TestAuthListJSON(t *testing.T) {
	useFakeAccessClient(t, nil, &authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{{APIGroups: []string{"kognition.dev"}, Resources: []string{"agents"}, Verbs: []string{"get"}}},
	})
	outputFormat = "json"
	stdout, _, err := runAuth(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Namespace string `json:"namespace"`
		Rules     []struct {
			Resource string   `json:"resource"`
			Verbs    []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || decoded.Namespace != "team-a" || len(decoded.Rules) != 1 || decoded.Rules[0].Resource != "agents" {
		t.Fatalf("stdout = %q, decoded = %+v, err = %v", stdout, decoded, err)
	}
}

func TestAuthIsUnsupportedInStandaloneMode(t *testing.T) {
	previousMode := k8sMode
	k8sMode = false
	t.Cleanup(func() { k8sMode = previousMode })
	for _, args := range [][]string{{"can-i", "get", "agent"}, {"list"}} {
		_, _, err := runAuth(t, args...)
		if err == nil || errors.Is(err, ErrAccessDenied) || !strings.Contains(err.Error(), "Kubernetes mode") {
			t.Fatalf("args %v: err = %v, want a clear unsupported message", args, err)
		}
	}
}
