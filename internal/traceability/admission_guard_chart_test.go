// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeapproval/conformancetest"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemapruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	kubejson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func runtimeApprovalSchemaCheck(test *testing.T, path string) (*structuralschema.Structural, func(map[string]any) field.ErrorList) {
	test.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var definition apiextensionsv1.CustomResourceDefinition
	for definition.Name == "" {
		if err := decoder.Decode(&definition); err != nil {
			test.Fatal(err)
		}
	}
	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(definition.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil); err != nil {
		test.Fatal(err)
	}
	structure, err := structuralschema.NewStructural(internal)
	if err != nil {
		test.Fatal(err)
	}
	openapi, _, err := schemavalidation.NewSchemaValidator(internal)
	if err != nil {
		test.Fatal(err)
	}
	cel := schemacel.NewValidator(structure, true, 1000000)
	validate := func(object map[string]any) field.ErrorList {
		errors := schemavalidation.ValidateCustomResource(field.NewPath("root"), object, openapi)
		if cel != nil {
			additional, _ := cel.Validate(context.Background(), field.NewPath("root"), structure, object, nil, 10000000)
			errors = append(errors, additional...)
		}
		return errors
	}
	return structure, validate
}

func TestRuntimeApprovalExecutorSchemaDiscrimination(test *testing.T) {
	structure, validate := runtimeApprovalSchemaCheck(test, "../../deploy/helm/kognition/crds/kognition.dev_runtimeapprovals.yaml")
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{Version: 1,
		Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		test.Fatal(err)
	}
	approval, _ := conformancetest.ExecutorApprovalProposal(test, keys, "executor-schema")
	approval.Spec.CreatedAt = time.Now().UTC().Truncate(time.Second)
	approval.Spec.ExpiresAt = approval.Spec.ExecutorAction.ExpiresAt
	encoded, err := json.Marshal(convert.RuntimeApprovalToK8s(&approval))
	if err != nil {
		test.Fatal(err)
	}
	var executor map[string]any
	if err := kubejson.Unmarshal(encoded, &executor); err != nil {
		test.Fatal(err)
	}
	if errors := validate(executor); len(errors) != 0 {
		test.Fatalf("valid executor schema rejected: %v", errors)
	}
	for name, change := range map[string]func(map[string]any){
		"mixed Agent": func(spec map[string]any) { spec["agentRef"] = spec["executorAction"].(map[string]any)["target"] },
		"wrong actor": func(spec map[string]any) { spec["actorKind"] = "Agent" },
		"legacy domain": func(spec map[string]any) {
			spec["binding"].(map[string]any)["domain"] = runtimeapproval.BindingDomainV1
		},
		"changed activation": func(spec map[string]any) { spec["workflowContext"].(map[string]any)["ownerEpoch"] = int64(2) },
		"extended authority": func(spec map[string]any) {
			spec["expiresAt"] = approval.Spec.ExpiresAt.Add(time.Hour).Format(time.RFC3339)
		},
		"wrong service":     func(spec map[string]any) { spec["targetRef"].(map[string]any)["uid"] = "replacement" },
		"wildcard revision": func(spec map[string]any) { spec["executorAction"].(map[string]any)["workflowRevisionDigest"] = "*" },
	} {
		test.Run(name, func(test *testing.T) {
			var changed map[string]any
			if err := kubejson.Unmarshal(encoded, &changed); err != nil {
				test.Fatal(err)
			}
			change(changed["spec"].(map[string]any))
			if errors := validate(changed); len(errors) == 0 {
				test.Fatalf("invalid %s passed schema/CEL validation", name)
			}
		})
	}
	executor["spec"].(map[string]any)["executorAction"].(map[string]any)["skipPolicy"] = true
	schemapruning.Prune(executor, structure, true)
	if _, retained := executor["spec"].(map[string]any)["executorAction"].(map[string]any)["skipPolicy"]; !retained {
		test.Fatal("schema pruned unknown governance before strict admission could reject it")
	}
	reference := func(kind string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind,
			Scope: "Namespaced", Namespace: "team-a", Name: "fixture", UID: "fixture-uid", ResourceVersion: 1}
	}
	legacy := api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: "legacy",
		InitialDecision: api.RuntimeApprovalDecisionPending, Plane: api.RuntimeApprovalPlaneTool, Operation: "call",
		AgentRef: reference("Agent"), AgentIdentityRef: reference("AgentIdentity"), AgentInstanceRef: reference("AgentInstance"),
		SessionRef: "session", TargetRef: reference("Tool"), AutonomyClassRef: api.RuntimeApprovalReference{Kind: "AutonomyClass",
			Scope: "Cluster", Name: "supervised", UID: "class-uid", ResourceVersion: 1},
		Binding: api.RuntimeActionBinding{Format: runtimeapproval.BindingFormatV1, Domain: runtimeapproval.BindingDomainV1,
			Algorithm: runtimeapproval.BindingAlgorithmV1, Epoch: 1, KeyVersion: 1, Tag: strings.Repeat("A", 43)},
		CreatedAt: approval.Spec.CreatedAt, ExpiresAt: approval.Spec.ExpiresAt}, Status: runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionPending)}
	legacyData, err := json.Marshal(convert.RuntimeApprovalToK8s(&legacy))
	if err != nil {
		test.Fatal(err)
	}
	var legacyObject map[string]any
	if err := kubejson.Unmarshal(legacyData, &legacyObject); err != nil {
		test.Fatal(err)
	}
	if errors := validate(legacyObject); len(errors) != 0 {
		test.Fatalf("legacy Agent schema regressed: %v", errors)
	}
}

func TestRuntimeApprovalExecutorArtifactSchemas(test *testing.T) {
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{Version: 1,
		Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		test.Fatal(err)
	}
	approval, _ := conformancetest.ExecutorApprovalProposal(test, keys, "executor-artifact-schema")
	now := time.Now().UTC().Truncate(time.Second)
	approval.Spec.CreatedAt, approval.Spec.ExpiresAt = now, approval.Spec.ExecutorAction.ExpiresAt
	native := convert.RuntimeApprovalToK8s(&approval)
	executor := native.Spec.ExecutorAction.Executor
	ticket := k8sapi.RuntimeApprovalQuotaTicket{
		TypeMeta: metav1.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApprovalQuotaTicket"},
		Spec: k8sapi.RuntimeApprovalQuotaTicketSpec{ActorKind: api.RuntimeApprovalActorExecutor, ExecutorRef: &executor,
			Workspace: approval.Spec.Workspace, InvocationID: approval.Spec.InvocationID, Binding: native.Spec.Binding,
			Phase: k8sapi.RuntimeApprovalQuotaTicketPhaseLive, ChargedBytes: 1024, CreatedAt: metav1.NewTime(now)},
	}
	tombstone := k8sapi.RuntimeApprovalTombstone{
		TypeMeta: metav1.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApprovalTombstone"},
		Spec: k8sapi.RuntimeApprovalTombstoneSpec{ActorKind: api.RuntimeApprovalActorExecutor, ExecutorAction: native.Spec.ExecutorAction,
			WorkflowContext: native.Spec.WorkflowContext, Workspace: approval.Spec.Workspace, InvocationID: approval.Spec.InvocationID,
			Plane: api.RuntimeApprovalPlaneWorkflow, Operation: runtimeapproval.WorkflowExecutorInitiate, RuntimeApprovalUID: "approval-uid",
			InitialDecision: api.RuntimeApprovalDecisionPending, Binding: native.Spec.Binding, DecisionState: api.RuntimeApprovalDecisionApproved,
			ExecutionState: api.RuntimeApprovalExecutionCompleted, ExecutionDisposition: runtimeapproval.ExecutionDispositionSucceeded,
			CreatedAt: native.Spec.CreatedAt, ExpiresAt: native.Spec.ExpiresAt, CompactedAt: metav1.NewTime(now.Add(time.Minute)),
			RetentionDeadline: metav1.NewTime(now.Add(90 * 24 * time.Hour))},
	}
	for _, scenario := range []struct {
		name, file string
		object     any
	}{
		{"quota", "kognition.dev_runtimeapprovalquotatickets.yaml", ticket},
		{"tombstone", "kognition.dev_runtimeapprovaltombstones.yaml", tombstone},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			_, validate := runtimeApprovalSchemaCheck(test, "../../deploy/helm/kognition/crds/"+scenario.file)
			encoded, err := json.Marshal(scenario.object)
			if err != nil {
				test.Fatal(err)
			}
			var object map[string]any
			if err := kubejson.Unmarshal(encoded, &object); err != nil {
				test.Fatal(err)
			}
			if errors := validate(object); len(errors) != 0 {
				test.Fatalf("valid executor %s rejected: %v", scenario.name, errors)
			}
			object["spec"].(map[string]any)["agentRef"] = map[string]any{"kind": "Agent", "scope": "Namespaced", "namespace": "team-a", "name": "forged", "uid": "forged-uid", "resourceVersion": int64(1)}
			if errors := validate(object); len(errors) == 0 {
				test.Fatalf("mixed executor/Agent %s accepted", scenario.name)
			}
		})
	}
}

func TestResourceWebhooksSupportExplicitReviewedCA(test *testing.T) {
	certificate := []byte("reviewed CA fixture; deployed acceptance validates the actual TLS chain")
	bundle := base64.StdEncoding.EncodeToString(certificate)
	found := 0
	hooks := 0
	for _, document := range renderControllerChart(test, "--set", "webhook.certManager.enabled=false", "--set", "webhook.caBundle="+bundle) {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Webhooks []struct {
				ClientConfig admissionregistrationv1.WebhookClientConfig `json:"clientConfig"`
			} `json:"webhooks"`
		}
		if err := json.Unmarshal(document.Raw, &object); err != nil {
			test.Fatal(err)
		}
		if object.Metadata.Name != "security-test-kognition-mutating" && object.Metadata.Name != "security-test-kognition-validating" {
			continue
		}
		found++
		for _, hook := range object.Webhooks {
			hooks++
			if string(hook.ClientConfig.CABundle) != string(certificate) {
				test.Error("the immutable GitOps render lost the explicitly reviewed resource-webhook CA")
			}
		}
	}
	if found != 2 || hooks != 4 {
		test.Fatal("all mutation, validation, audit and workflow-record webhook trust surfaces are required")
	}
}

func TestAdmissionBootstrapWithholdsControllerPrivileges(test *testing.T) {
	for _, activated := range []string{"default", "false", "true"} {
		test.Run(activated, func(test *testing.T) {
			grants, guards, policies, controllers := 0, 0, 0, 0
			arguments := []string{"--set", "k8sController.managedNamespaces[0]=tenant"}
			if activated != "default" {
				arguments = append(arguments, "--set", "k8sController.activated="+activated)
			}
			for _, document := range renderControllerChart(test, arguments...) {
				var binding rbacv1.RoleBinding
				if err := json.Unmarshal(document.Raw, &binding); err != nil {
					test.Fatal(err)
				}
				switch binding.Kind {
				case "RoleBinding", "ClusterRoleBinding":
					for _, subject := range binding.Subjects {
						if subject.Kind == "ServiceAccount" && subject.Namespace == "control" && subject.Name == "security-test-kognition-controller" {
							grants++
						}
					}
				case "ValidatingAdmissionPolicy":
					policies++
				case "Deployment":
					var deployment appsv1.Deployment
					if err := json.Unmarshal(document.Raw, &deployment); err != nil {
						test.Fatal(err)
					}
					if deployment.Name == "security-test-kognition-admission-guard" && deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 0 {
						guards++
					}
					if deployment.Spec.Template.Spec.ServiceAccountName == "security-test-kognition-controller" {
						controllers++
						if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
							test.Fatalf("incorrect activation replicas: %+v", deployment.Spec.Replicas)
						}
					}
				}
			}
			if guards != 1 || policies != 10 || controllers != 1 {
				test.Fatalf("bootstrap resources: guard=%d policies=%d controller=%d", guards, policies, controllers)
			}
			if grants != 0 {
				test.Fatalf("unexpected controller grants: %d", grants)
			}
		})
	}
}

func TestResourceWebhookAuditPolicyRespectsEnabledIdentity(test *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		test.Run(enabled, func(test *testing.T) {
			arguments := []string{"--set", "webhook.enabled=" + enabled, "--set", "toolGateway.enabled=false", "--set", "memoryPolicyGateway.enabled=false", "--set", "agentCommunicationGateway.enabled=false", "--set", "modelGateway.enabled=false"}
			if enabled == "false" {
				arguments = append(arguments, "--set", "serviceAccount.name=security-test-kognition-webhook")
			}
			found := false
			for _, document := range renderControllerChart(test, arguments...) {
				var policy admissionregistrationv1.ValidatingAdmissionPolicy
				if err := json.Unmarshal(document.Raw, &policy); err != nil {
					test.Fatal(err)
				}
				if policy.Kind != "ValidatingAdmissionPolicy" || policy.Name != "security-test-kognition-control-state" {
					continue
				}
				found = true
				expectedValidations := 2
				if enabled == "false" {
					expectedValidations = 1
					expected := `request.userInfo.username == "system:serviceaccount:control:security-test-kognition-controller" || (request.namespace == "control" && request.userInfo.username == "system:serviceaccount:control:security-test-kognition-webhook")`
					if len(policy.Spec.MatchConditions) != 1 || strings.Join(strings.Fields(policy.Spec.MatchConditions[0].Expression), " ") != expected {
						test.Fatal("disabled webhook changed the namespace-bound platform identity contract")
					}
				}
				if len(policy.Spec.Validations) != expectedValidations {
					test.Fatalf("webhook enabled=%s has %d state validations, want %d", enabled, len(policy.Spec.Validations), expectedValidations)
				}
			}
			if !found {
				test.Fatal("mandatory state policy missing")
			}
		})
	}
}

func TestGatewayStatePoliciesMatchOnlyEnabledIdentities(test *testing.T) {
	for component, key := range map[string]string{"tool-gateway": "toolGateway", "memory-policy-gateway": "memoryPolicyGateway", "agent-communication-gateway": "agentCommunicationGateway", "model-gateway": "modelGateway"} {
		for _, enabled := range []string{"true", "false"} {
			test.Run(component+"/"+enabled, func(test *testing.T) {
				arguments := []string{"--set", "toolGateway.enabled=false", "--set", "memoryPolicyGateway.enabled=false", "--set", "agentCommunicationGateway.enabled=false", "--set", "modelGateway.enabled=false", "--set", key + ".enabled=" + enabled}
				found := false
				for _, document := range renderControllerChart(test, arguments...) {
					var policy admissionregistrationv1.ValidatingAdmissionPolicy
					if err := json.Unmarshal(document.Raw, &policy); err != nil {
						test.Fatal(err)
					}
					if policy.Kind != "ValidatingAdmissionPolicy" || policy.Name != "security-test-kognition-control-state" {
						continue
					}
					found = true
					username := `"system:serviceaccount:control:security-test-kognition-` + component + `"`
					if len(policy.Spec.MatchConditions) != 1 || strings.Contains(policy.Spec.MatchConditions[0].Expression, username) != (enabled == "true") {
						test.Fatal("gateway state boundary disagrees with enabled identity", component, enabled)
					}
				}
				if !found {
					test.Fatal("state enforcement missing")
				}
			})
		}
	}
}

func TestArtifactSignerTrustMountsAdmissionProcesses(test *testing.T) {
	for _, mode := range []string{"standalone", "kubernetes"} {
		test.Run(mode, func(test *testing.T) {
			expected := map[string]bool{"apiserver": mode == "standalone", "k8s-controller": mode == "kubernetes", "resource-webhooks": mode == "kubernetes"}
			seen := map[string]bool{}
			for _, document := range renderControllerChart(test, "--set", "mode="+mode, "--set", "artifactSigning.existingSecret=operator-signers") {
				var deployment appsv1.Deployment
				if err := json.Unmarshal(document.Raw, &deployment); err != nil {
					test.Fatal(err)
				}
				component := deployment.Labels["app.kubernetes.io/component"]
				if deployment.Kind != "Deployment" || !expected[component] {
					continue
				}
				seen[component] = true
				container := deployment.Spec.Template.Spec.Containers[0]
				envFound, mountFound, volumeFound := false, false, false
				for _, environment := range container.Env {
					if environment.Name == "KOGNITION_ARTIFACT_SIGNING_KEYS_FILE" && environment.Value == "/etc/kognition/artifact-signers/public-keys.json" {
						envFound = true
					}
				}
				for _, mount := range container.VolumeMounts {
					if mount.Name == "artifact-signers" && mount.ReadOnly && mount.MountPath == "/etc/kognition/artifact-signers" && mount.SubPath == "" {
						mountFound = true
					}
				}
				for _, volume := range deployment.Spec.Template.Spec.Volumes {
					if volume.Name == "artifact-signers" && volume.Secret != nil && volume.Secret.SecretName == "operator-signers" {
						volumeFound = true
					}
				}
				if !envFound || !mountFound || !volumeFound {
					test.Fatalf("%s does not receive refreshable operator trust: env=%t mount=%t volume=%t", component, envFound, mountFound, volumeFound)
				}
			}
			for component, required := range expected {
				if required && !seen[component] {
					test.Fatalf("missing admission process %s", component)
				}
			}
		})
	}
}

func TestActivationParsesBootstrapChart(test *testing.T) {
	var manifest strings.Builder
	for _, document := range renderControllerChart(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		manifest.WriteString("---\n")
		manifest.Write(document.Raw)
		manifest.WriteString("\n")
	}
	parsed, err := activation.Parse(strings.NewReader(manifest.String()))
	if err != nil {
		test.Fatal(err)
	}
	if parsed.Controller != "security-test-kognition-controller" || parsed.Guard != "security-test-kognition-admission-guard" || parsed.Namespace != "control" || len(parsed.Objects) != 27 {
		test.Fatalf("unexpected bootstrap inventory: %+v", parsed)
	}
	if _, err := activation.ParseCandidate([]byte(manifest.String()), "security-test"); err != nil {
		test.Fatalf("bootstrap activation candidate: %v", err)
	}
}

func TestActivationChartPinsControllerAndGuardImages(test *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, registry := range []string{"", "registry.example.com/kognition"} {
		test.Run(registry, func(test *testing.T) {
			images := map[string]string{}
			for _, document := range renderControllerChart(test, "--set", "k8sController.managedNamespaces[0]=tenant", "--set", "k8sController.image.digest="+digest, "--set", "global.imageRegistry="+registry) {
				var deployment appsv1.Deployment
				if err := json.Unmarshal(document.Raw, &deployment); err != nil {
					test.Fatal(err)
				}
				if deployment.Kind != "Deployment" || (deployment.Name != "security-test-kognition-k8s-controller" && deployment.Name != "security-test-kognition-admission-guard") {
					continue
				}
				images[deployment.Name] = deployment.Spec.Template.Spec.Containers[0].Image
			}
			repository := "kognition-k8s-controller"
			if registry != "" {
				repository = registry + "/" + repository
			}
			for _, name := range []string{"security-test-kognition-k8s-controller", "security-test-kognition-admission-guard"} {
				if images[name] != repository+"@"+digest {
					test.Errorf("%s image = %q, want digest-pinned image", name, images[name])
				}
			}
		})
	}
}

func TestBootstrapGatewaySecretsCanBeProvisionedExternally(test *testing.T) {
	arguments := []string{"--set", "k8sController.managedNamespaces[0]=tenant", "--set", "modelGateway.enabled=true", "--set", "agentCommunicationGateway.enabled=true"}
	components := map[string]string{
		"memoryPolicyGateway":       "memory-policy-gateway",
		"toolGateway":               "tool-gateway",
		"modelGateway":              "model-gateway",
		"agentCommunicationGateway": "agent-communication-gateway",
	}
	for _, external := range []bool{false, true} {
		name := "generated"
		settings := append([]string{}, arguments...)
		if external {
			name = "external"
			for component := range components {
				settings = append(settings, "--set", component+".secret.create=false")
			}
		}
		test.Run(name, func(test *testing.T) {
			documents := renderControllerChart(test, settings...)
			for _, suffix := range components {
				secretName := "security-test-kognition-" + suffix
				secrets, consumers := 0, 0
				for _, document := range documents {
					var object metav1.PartialObjectMetadata
					if err := json.Unmarshal(document.Raw, &object); err != nil {
						test.Fatal(err)
					}
					if object.Kind == "Secret" && object.Name == secretName {
						secrets++
						var secret corev1.Secret
						if err := json.Unmarshal(document.Raw, &secret); err != nil {
							test.Fatal(err)
						}
						if len(secret.Data["capability-hmac-key"]) == 0 {
							test.Fatalf("%s lacks capability key", secretName)
						}
					}
					if object.Kind != "Deployment" || object.Name != secretName {
						continue
					}
					var deployment appsv1.Deployment
					if err := json.Unmarshal(document.Raw, &deployment); err != nil {
						test.Fatal(err)
					}
					for _, container := range deployment.Spec.Template.Spec.Containers {
						for _, variable := range container.Env {
							if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil && variable.ValueFrom.SecretKeyRef.Name == secretName {
								consumers++
							}
						}
					}
				}
				if consumers == 0 || (external && secrets != 0) || (!external && secrets != 1) {
					test.Errorf("%s: secrets=%d consumers=%d external=%t", secretName, secrets, consumers, external)
				}
			}
			if external && !reflect.DeepEqual(documents, renderControllerChart(test, settings...)) {
				test.Error("external gateway Secrets must allow deterministic bootstrap renders")
			}
		})
	}
}

func TestResourceWebhooksAvailableBeforeControllerActivation(test *testing.T) {
	const webhookName = "security-test-kognition-webhook"
	const controllerName = "security-test-kognition-k8s-controller"
	var webhookDeployment appsv1.Deployment
	var webhookService corev1.Service
	var controllerDeployment appsv1.Deployment
	policyReads := false
	claimantReads := false
	for _, document := range renderControllerChart(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		var metadata metav1.PartialObjectMetadata
		if err := json.Unmarshal(document.Raw, &metadata); err != nil {
			test.Fatal(err)
		}
		if metadata.Labels["app.kubernetes.io/component"] == "resource-webhooks" && (metadata.Kind == "Role" || metadata.Kind == "ClusterRole") {
			var role rbacv1.ClusterRole
			if err := json.Unmarshal(document.Raw, &role); err != nil {
				test.Fatal(err)
			}
			for _, rule := range role.Rules {
				for _, resource := range rule.Resources {
					if resource == "pods" {
						if metadata.Kind != "Role" || metadata.Namespace != "control" || !reflect.DeepEqual(rule.APIGroups, []string{""}) || !reflect.DeepEqual(rule.Verbs, []string{"get", "list"}) {
							test.Fatalf("claimant verification has excess Pod authority: %+v", rule)
						}
						claimantReads = true
					}
					if resource == "secrets" || resource == "deployments" || resource == "jobs" || resource == "roles" || resource == "rolebindings" || resource == "*" {
						test.Fatalf("resource admission has unrelated authority: %+v", rule)
					}
					if resource == "cognitiveadmissionpolicies" && metadata.Kind == "ClusterRole" {
						policyReads = reflect.DeepEqual(rule.Verbs, []string{"get", "list"})
					}
				}
			}
		}
		switch {
		case metadata.Kind == "Deployment" && metadata.Name == webhookName:
			if err := json.Unmarshal(document.Raw, &webhookDeployment); err != nil {
				test.Fatal(err)
			}
		case metadata.Kind == "Deployment" && metadata.Name == controllerName:
			if err := json.Unmarshal(document.Raw, &controllerDeployment); err != nil {
				test.Fatal(err)
			}
		case metadata.Kind == "Service" && metadata.Name == webhookName:
			if err := json.Unmarshal(document.Raw, &webhookService); err != nil {
				test.Fatal(err)
			}
		}
	}
	if webhookDeployment.Name == "" || webhookDeployment.Spec.Replicas == nil || *webhookDeployment.Spec.Replicas < 1 {
		test.Fatal("resource webhook must run independently during inactive bootstrap")
	}
	if !policyReads {
		test.Fatal("cluster-scoped class admission requires policy reads without write authority")
	}
	if !claimantReads {
		test.Fatal("independent claimant verification lacks scoped Pod reads")
	}
	if controllerDeployment.Spec.Replicas == nil || *controllerDeployment.Spec.Replicas != 0 {
		test.Fatal("independent resource admission must not activate reconciliation")
	}
	if webhookDeployment.Spec.Template.Spec.ServiceAccountName == controllerDeployment.Spec.Template.Spec.ServiceAccountName {
		test.Fatal("resource admission must not use the controller identity")
	}
	for label, value := range webhookService.Spec.Selector {
		if webhookDeployment.Spec.Template.Labels[label] != value {
			test.Fatalf("webhook Service selector %s does not select the independent server", label)
		}
	}
	if len(webhookDeployment.Spec.Template.Spec.Containers) != 1 || webhookDeployment.Spec.Template.Spec.Containers[0].Args[0] != "resource-webhooks" {
		test.Fatal("resource admission Deployment must use the webhook-only command")
	}
	for _, deployment := range []appsv1.Deployment{webhookDeployment, controllerDeployment} {
		found := false
		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			if variable.Name == "KOGNITION_RUNTIME_APPROVAL_DECISION_SERVICE_ACCOUNT" {
				found = true
				if variable.Value != webhookDeployment.Spec.Template.Spec.ServiceAccountName {
					test.Fatal("human decision authority differs from the dedicated webhook identity")
				}
			}
		}
		if !found {
			test.Fatal("human decision authority not configured", deployment.Name)
		}
	}
}

func TestGatewayExecutionPlaneAccountsAreExplicit(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		value := "false"
		if enabled {
			value = "true"
		}
		seen := 0
		for _, document := range renderControllerChart(test, "--set", "toolGateway.enabled="+value, "--set", "memoryService.enabled="+value, "--set", "memoryPolicyGateway.enabled="+value, "--set", "agentCommunicationGateway.enabled="+value) {
			var deployment appsv1.Deployment
			if err := json.Unmarshal(document.Raw, &deployment); err != nil {
				test.Fatal(err)
			}
			component := deployment.Labels["app.kubernetes.io/component"]
			if deployment.Kind != "Deployment" || (component != "resource-webhooks" && component != "k8s-controller") {
				continue
			}
			seen++
			for key, suffix := range map[string]string{"KOGNITION_RUNTIME_APPROVAL_TOOL_SERVICE_ACCOUNT": "tool-gateway", "KOGNITION_RUNTIME_APPROVAL_MEMORY_SERVICE_ACCOUNT": "memory-policy-gateway", "KOGNITION_RUNTIME_APPROVAL_COMMUNICATION_SERVICE_ACCOUNT": "agent-communication-gateway"} {
				found := false
				for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
					if variable.Name == key {
						found = true
						want := ""
						if enabled {
							want = "security-test-kognition-" + suffix
						}
						if variable.Value != want {
							test.Fatal("execution plane identity differs", component, key, variable.Value)
						}
					}
				}
				if !found {
					test.Fatal("explicit execution plane identity missing", component, key)
				}
			}
		}
		if seen != 2 {
			test.Fatal("expected both admission identity assemblies", seen)
		}
	}
}

func TestRuntimeApprovalStatusUsesIndependentValidatingWebhook(test *testing.T) {
	found := false
	for _, document := range renderControllerChart(test) {
		var configuration admissionregistrationv1.ValidatingWebhookConfiguration
		if err := json.Unmarshal(document.Raw, &configuration); err != nil {
			test.Fatal(err)
		}
		if configuration.Kind != "ValidatingWebhookConfiguration" || configuration.Name != "security-test-kognition-validating" {
			continue
		}
		for _, hook := range configuration.Webhooks {
			if hook.Name != "validate.kognition.dev" {
				continue
			}
			if hook.ClientConfig.Service == nil || hook.ClientConfig.Service.Name != "security-test-kognition-webhook" || hook.ClientConfig.Service.Namespace != "control" || hook.FailurePolicy == nil || *hook.FailurePolicy != admissionregistrationv1.Fail {
				test.Fatal("approval status admission is not the fail-closed dedicated webhook")
			}
			for _, rule := range hook.Rules {
				for _, resource := range rule.Resources {
					if resource == "runtimeapprovals/status" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		test.Fatal("direct RuntimeApproval status writes bypass validation")
	}
}

func TestAdmissionGuardChart(test *testing.T) {
	const name = "security-test-kognition-admission-guard"
	for _, mode := range []string{"kubernetes", "standalone"} {
		test.Run(mode, func(test *testing.T) {
			counts := map[string]int{}
			for _, document := range renderControllerRBAC(test, "--set", "mode="+mode, "--set", "webhook.enabled=false", "--set", "k8sController.managedNamespaces[0]=tenant") {
				var metadata struct {
					metav1.TypeMeta `json:",inline"`
					Metadata        metav1.ObjectMeta `json:"metadata"`
				}
				if err := json.Unmarshal(document.Raw, &metadata); err != nil {
					test.Fatal(err)
				}
				if metadata.Metadata.Name != name {
					continue
				}
				counts[metadata.Kind]++
				switch metadata.Kind {
				case "Role":
					var role rbacv1.Role
					if err := json.Unmarshal(document.Raw, &role); err != nil {
						test.Fatal(err)
					}
					expected := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}, {APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get"}}, {APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get"}}}
					if role.Namespace != "tenant" || !reflect.DeepEqual(role.Rules, expected) {
						test.Fatalf("unexpected guard permissions: %+v", role)
					}
				case "Deployment":
					var deployment appsv1.Deployment
					if err := json.Unmarshal(document.Raw, &deployment); err != nil {
						test.Fatal(err)
					}
					pod := deployment.Spec.Template.Spec
					if pod.ServiceAccountName != name || len(pod.Containers) != 1 || len(pod.Containers[0].Args) == 0 || pod.Containers[0].Args[0] != "admission-guard" || deployment.Namespace != "control" {
						test.Fatalf("guard is not independent: %+v", deployment)
					}
					expectedArgs := []string{"admission-guard", "--release-name=security-test", "--installation-namespace=control", "--controller-service-account=security-test-kognition-controller", "--workload-namespaces=tenant", "--baseline-policy-name=security-test-kognition-stdio-baseline"}
					if !reflect.DeepEqual(pod.Containers[0].Args, expectedArgs) {
						test.Fatalf("guard ownership or baseline configuration differs: %v", pod.Containers[0].Args)
					}
				case "ValidatingWebhookConfiguration":
					var configuration admissionregistrationv1.ValidatingWebhookConfiguration
					if err := json.Unmarshal(document.Raw, &configuration); err != nil {
						test.Fatal(err)
					}
					if len(configuration.Webhooks) != 2 {
						test.Fatal("both guard webhooks are required")
					}
					for _, webhook := range configuration.Webhooks {
						if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregistrationv1.Fail || webhook.ClientConfig.Service == nil || webhook.ClientConfig.Service.Name != name {
							test.Fatalf("guard must fail closed independently: %+v", webhook)
						}
					}
				}
			}
			if mode == "standalone" {
				if len(counts) != 0 {
					test.Fatalf("standalone rendered guard: %v", counts)
				}
			} else {
				for _, kind := range []string{"ServiceAccount", "Deployment", "Service", "Role", "RoleBinding", "Issuer", "Certificate", "ValidatingWebhookConfiguration"} {
					if counts[kind] != 1 {
						test.Errorf("%s count = %d", kind, counts[kind])
					}
				}
			}
		})
	}
}

func TestAdmissionGuardChartRejectsUnsafeConfiguration(test *testing.T) {
	for _, scenario := range []struct {
		name, message string
		settings      []string
	}{
		{"unsupported-isolation", "stdioIsolationProfile must be", []string{"admissionGuard.stdioIsolationProfile=unknown"}},
		{"invalid-controller-digest", "first-party image digest must be a lowercase sha256 digest", []string{"k8sController.image.digest=sha256:invalid"}},
		{"controller-identity", "distinct from controller", []string{"k8sController.serviceAccount.name=security-test-kognition-admission-guard"}},
		{"platform-identity", "distinct from controller", []string{"serviceAccount.name=security-test-kognition-admission-guard"}},
		{"external-ca-missing", "caBundle is required", []string{"admissionGuard.tls.existingSecret=external-guard"}},
		{"runtime-auth-collision", "must not be readable", []string{"runtimeAuth.existingSecret=security-test-kognition-admission-guard-tls"}},
		{"external-tls-collision", "must not be readable", []string{"admissionGuard.tls.existingSecret=security-test-kognition-runtime-auth", "admissionGuard.tls.caBundle=Y2E="}},
		{"knowledge-tls-collision", "must not be readable", []string{"knowledgeGateway.enabled=true", "knowledgeGateway.tls.secretName=security-test-kognition-admission-guard-tls", "knowledgeGateway.runtimeTLS.secretName=runtime-client", "knowledgeGateway.runtimeTLS.serverName=gateway.local", "knowledgeGateway.provider.name=fixture", "knowledgeGateway.provider.address=provider.local:9443", "knowledgeGateway.provider.serverName=provider.local", "knowledgeGateway.provider.peerURI=spiffe://kognition/provider", "knowledgeGateway.provider.configuration.apiGroup=knowledge.kognition.dev", "knowledgeGateway.provider.configuration.apiVersion=v1alpha1", "knowledgeGateway.provider.configuration.kind=AzureSearchConfiguration", "knowledgeGateway.provider.configuration.resource=azuresearchconfigurations"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args := []string{"template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--kube-version", "1.30.3", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12"}
			for _, setting := range scenario.settings {
				args = append(args, "--set", setting)
			}
			output, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
			if err == nil || !strings.Contains(string(output), scenario.message) {
				test.Fatalf("unsafe configuration not rejected: %v %s", err, output)
			}
		})
	}
}

func TestAdmissionGuardSandboxBaseline(test *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		test.Run(enabled, func(test *testing.T) {
			found := map[string]bool{}
			for _, document := range renderControllerRBAC(test, "--set", "networkPolicy.enabled="+enabled, "--set", "k8sController.managedNamespaces[0]=tenant", "--set", "k8sController.managedNamespaces[1]=second") {
				var policy networkingv1.NetworkPolicy
				if err := json.Unmarshal(document.Raw, &policy); err != nil {
					test.Fatal(err)
				}
				if policy.Kind != "NetworkPolicy" || policy.Name != "security-test-kognition-stdio-baseline" {
					continue
				}
				expected := metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "kognition.dev/stdio-invocation", Operator: metav1.LabelSelectorOpExists}}}
				if !reflect.DeepEqual(policy.Spec.PodSelector, expected) || !reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) || len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 0 || len(policy.OwnerReferences) != 0 || policy.Annotations["meta.helm.sh/release-name"] != "security-test" || policy.Annotations["meta.helm.sh/release-namespace"] != "control" {
					test.Fatalf("unsafe sandbox baseline: %+v", policy)
				}
				found[policy.Namespace] = true
			}
			if !reflect.DeepEqual(found, map[string]bool{"tenant": true, "second": true}) {
				test.Fatalf("missing workload baselines: %v", found)
			}
		})
	}
}

func TestAdmissionGuardNetworkPolicy(test *testing.T) {
	for _, document := range renderControllerRBAC(test, "--set", "networkPolicy.enabled=true") {
		var policy networkingv1.NetworkPolicy
		if err := json.Unmarshal(document.Raw, &policy); err != nil {
			test.Fatal(err)
		}
		if policy.Kind != "NetworkPolicy" || policy.Name != "security-test-kognition-admission-guard" {
			continue
		}
		if policy.Namespace != "control" || !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, map[string]string{"app.kubernetes.io/component": "admission-guard", "app.kubernetes.io/instance": "security-test"}) || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
			test.Fatalf("guard selector mismatch: %+v", policy.Spec.PodSelector)
		}
		if !reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].Ports) != 1 || policy.Spec.Ingress[0].Ports[0].Port.IntVal != 9443 {
			test.Fatalf("unexpected guard ingress: %+v", policy.Spec)
		}
		ports := map[string]int{}
		for _, rule := range policy.Spec.Egress {
			if len(rule.Ports) == 0 {
				test.Fatal("unrestricted guard egress")
			}
			for _, port := range rule.Ports {
				if port.Port == nil || port.Protocol == nil || port.EndPort != nil {
					test.Fatal("unrestricted guard port")
				}
				ports[string(*port.Protocol)+":"+port.Port.String()]++
			}
		}
		if !reflect.DeepEqual(ports, map[string]int{string(corev1.ProtocolTCP) + ":53": 1, string(corev1.ProtocolUDP) + ":53": 1, string(corev1.ProtocolTCP) + ":443": 1, string(corev1.ProtocolTCP) + ":6443": 1}) {
			test.Fatalf("unexpected guard egress: %v", ports)
		}
		return
	}
	test.Fatal("guard NetworkPolicy missing")
}

func TestActivationCiliumIsolationInventory(test *testing.T) {
	documents := renderControllerChart(test, "--set", "admissionGuard.stdioIsolationProfile=cilium", "--set", "k8sController.managedNamespaces[0]=tenant", "--set", "k8sController.managedNamespaces[1]=second")
	for _, scenario := range []string{"valid", "missing-all", "missing-one", "allow-instead", "extra-specs", "wrong-api", "owned", "wrong-release", "no-profile"} {
		test.Run(scenario, func(test *testing.T) {
			var content strings.Builder
			for _, document := range documents {
				object := &unstructured.Unstructured{}
				if err := json.Unmarshal(document.Raw, object); err != nil {
					test.Fatal(err)
				}
				if object.GetKind() == "CiliumNetworkPolicy" {
					switch scenario {
					case "missing-all":
						continue
					case "missing-one":
						if object.GetNamespace() == "second" {
							continue
						}
					case "allow-instead":
						object.Object["spec"].(map[string]interface{})["egressDeny"] = []interface{}{}
					case "extra-specs":
						object.Object["specs"] = []interface{}{object.Object["spec"]}
					case "wrong-api":
						object.SetAPIVersion("other.io/v2")
					case "owned":
						object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "owner", UID: "owner"}})
					case "wrong-release":
						object.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "other", "meta.helm.sh/release-namespace": "control"})
					}
				}
				if scenario == "no-profile" && object.GetKind() == "Deployment" {
					unstructured.RemoveNestedField(object.Object, "spec", "template", "metadata", "annotations", "kognition.dev/stdio-isolation-profile")
				}
				encoded, err := object.MarshalJSON()
				if err != nil {
					test.Fatal(err)
				}
				content.WriteString("---\n")
				content.Write(encoded)
				content.WriteString("\n")
			}
			parsed, err := activation.Parse(strings.NewReader(content.String()))
			if scenario != "valid" {
				if err == nil {
					test.Fatal("unsafe Cilium inventory accepted")
				}
				return
			}
			if err != nil || parsed.StdioIsolationProfile != "cilium" || len(parsed.Objects) != 32 {
				test.Fatalf("Cilium inventory: profile=%s count=%d err=%v", parsed.StdioIsolationProfile, len(parsed.Objects), err)
			}
			if _, err := activation.ParseCandidate([]byte(content.String()), "security-test"); err != nil {
				test.Fatalf("Cilium activation candidate: %v", err)
			}
		})
	}
}
