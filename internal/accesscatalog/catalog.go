// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package accesscatalog is the single source of truth for who may do what with
// Kognition resources in Kubernetes mode.
//
// It classifies every shipped CRD kind by the consequence of granting access and
// defines the human ClusterRoles in terms of those classes. The chart's human
// roles, the documentation tables and the authorization tests are all derived
// from this catalog, so a kind cannot ship without an access decision and the
// roles cannot drift from the design.
//
// The catalog has no Kubernetes client dependency. Kubernetes only evaluates the
// rbac.authorization.k8s.io objects generated from it.
package accesscatalog

// APIGroup is the API group of every Kognition resource.
const APIGroup = "kognition.dev"

const rbacAPIGroup = "rbac.authorization.k8s.io"

// Class groups kinds by the consequence of granting access to them.
type Class string

const (
	// ClassConfig is author-managed definitions.
	ClassConfig Class = "config"
	// ClassKnowledge is curated knowledge and indexing.
	ClassKnowledge Class = "knowledge"
	// ClassSubmit is requests that start work.
	ClassSubmit Class = "submit"
	// ClassRuntime is controller-owned execution records.
	ClassRuntime Class = "runtime"
	// ClassData is stored content.
	ClassData Class = "data"
	// ClassGrant is delegation and credential binding.
	ClassGrant Class = "grant"
	// ClassGovernance is policy, quota and trust configuration.
	ClassGovernance Class = "governance"
	// ClassDecision is human decisions and their records.
	ClassDecision Class = "decision"
	// ClassPlatform is platform-owned namespaced kinds.
	ClassPlatform Class = "platform"
	// ClassClass is cluster-scoped classes.
	ClassClass Class = "class"
	// ClassSystem is internal controller state with no human write access.
	ClassSystem Class = "system"
)

// Scope is whether a kind is namespaced or cluster-scoped.
type Scope string

const (
	// ScopeNamespaced kinds live in a workload namespace.
	ScopeNamespaced Scope = "Namespaced"
	// ScopeCluster kinds are cluster-wide.
	ScopeCluster Scope = "Cluster"
)

// Binding is how a role is meant to be bound.
type Binding string

const (
	// BindingNamespace roles are bound with a RoleBinding in a workload namespace.
	BindingNamespace Binding = "RoleBinding"
	// BindingCluster roles are bound with a ClusterRoleBinding.
	BindingCluster Binding = "ClusterRoleBinding"
)

// Kind is one shipped CRD kind and its access class.
type Kind struct {
	Name     string
	Resource string
	Scope    Scope
	Class    Class
}

// Grant gives verbs on resources. Resources come from classes, named kinds, or
// an explicit list for API groups outside Kognition.
type Grant struct {
	Classes       []Class
	Kinds         []string
	Subresource   string
	Verbs         []string
	APIGroup      string
	Resources     []string
	ResourceNames []string
}

// Role is one human ClusterRole.
type Role struct {
	Name        string
	Summary     string
	Binding     Binding
	AggregateTo string
	Grants      []Grant
}

// Catalog is the classification of kinds and the roles built from it.
type Catalog struct {
	Kinds []Kind
	Roles []Role
}

var (
	verbsRead  = []string{"get", "list", "watch"}
	verbsWrite = []string{"get", "list", "watch", "create", "update", "patch", "delete"}
)

func namespaced(name, resource string, class Class) Kind {
	return Kind{Name: name, Resource: resource, Scope: ScopeNamespaced, Class: class}
}

func clusterScoped(name, resource string, class Class) Kind {
	return Kind{Name: name, Resource: resource, Scope: ScopeCluster, Class: class}
}

func shippedKinds() []Kind {
	return []Kind{
		namespaced("Agent", "agents", ClassConfig),
		namespaced("AgentAutoscaler", "agentautoscalers", ClassConfig),
		namespaced("AgentService", "agentservices", ClassConfig),
		namespaced("Evaluation", "evaluations", ClassConfig),
		namespaced("EvaluationDataset", "evaluationdatasets", ClassConfig),
		namespaced("IntentGateway", "intentgateways", ClassConfig),
		namespaced("KnowledgeBaseClaim", "knowledgebaseclaims", ClassConfig),
		namespaced("MemoryClaim", "memoryclaims", ClassConfig),
		namespaced("Model", "models", ClassConfig),
		namespaced("ModelAutoscaler", "modelautoscalers", ClassConfig),
		namespaced("ModelInvocationProfile", "modelinvocationprofiles", ClassConfig),
		namespaced("Prompt", "prompts", ClassConfig),
		namespaced("Solution", "solutions", ClassConfig),
		namespaced("Tool", "tools", ClassConfig),
		namespaced("Workflow", "workflows", ClassConfig),

		namespaced("KnowledgeBase", "knowledgebases", ClassKnowledge),
		namespaced("KnowledgeBaseIndexRun", "knowledgebaseindexruns", ClassKnowledge),
		namespaced("KnowledgeBaseIndexSchedule", "knowledgebaseindexschedules", ClassKnowledge),

		namespaced("AgentJob", "agentjobs", ClassSubmit),
		namespaced("EvaluationRun", "evaluationruns", ClassSubmit),
		namespaced("WorkflowRun", "workflowruns", ClassSubmit),

		namespaced("AgentInstance", "agentinstances", ClassRuntime),
		namespaced("Memory", "memories", ClassRuntime),
		namespaced("MemoryVolume", "memoryvolumes", ClassRuntime),
		namespaced("ToolInvocationRun", "toolinvocationruns", ClassRuntime),

		namespaced("MemoryRecord", "memoryrecords", ClassData),

		namespaced("AgentRole", "agentroles", ClassGrant),
		namespaced("AgentRoleBinding", "agentrolebindings", ClassGrant),
		namespaced("CrossNamespaceGrant", "crossnamespacegrants", ClassGrant),
		namespaced("ExternalAgentGrant", "externalagentgrants", ClassGrant),
		namespaced("MemoryGrant", "memorygrants", ClassGrant),
		namespaced("ModelGrant", "modelgrants", ClassGrant),
		namespaced("SecretRef", "secretrefs", ClassGrant),
		namespaced("ToolGrant", "toolgrants", ClassGrant),

		namespaced("AgentCommunicationPolicy", "agentcommunicationpolicies", ClassGovernance),
		namespaced("AgentPolicy", "agentpolicies", ClassGovernance),
		namespaced("CognitiveAdmissionPolicy", "cognitiveadmissionpolicies", ClassGovernance),
		namespaced("CognitiveLimitRange", "cognitivelimitranges", ClassGovernance),
		namespaced("CognitiveQuota", "cognitivequotas", ClassGovernance),
		namespaced("TrustProfile", "trustprofiles", ClassGovernance),

		namespaced("ApprovalDecision", "approvaldecisions", ClassDecision),
		namespaced("AutonomyReview", "autonomyreviews", ClassDecision),
		namespaced("RuntimeApproval", "runtimeapprovals", ClassDecision),

		namespaced("ExternalAgent", "externalagents", ClassPlatform),
		namespaced("Workspace", "workspaces", ClassPlatform),

		clusterScoped("AutonomyClass", "autonomyclasses", ClassClass),
		clusterScoped("KnowledgeBaseClass", "knowledgebaseclasses", ClassClass),
		clusterScoped("MemoryClass", "memoryclasses", ClassClass),
		clusterScoped("ModelClass", "modelclasses", ClassClass),
		clusterScoped("ToolClass", "toolclasses", ClassClass),

		namespaced("AgentIdentity", "agentidentities", ClassSystem),
		clusterScoped("KnowledgeBindingRecord", "knowledgebindingrecords", ClassSystem),
		namespaced("RuntimeApprovalQuotaTicket", "runtimeapprovalquotatickets", ClassSystem),
		namespaced("RuntimeApprovalTombstone", "runtimeapprovaltombstones", ClassSystem),
		namespaced("TrustProfileIndex", "trustprofileindices", ClassSystem),
	}
}

// namespaceRoleNames are the roles bound per namespace. kognition-access-admin may
// bind exactly these ClusterRoles. It must not bind the cluster-bound roles: a
// RoleBinding to one still grants its namespaced rules, which would let a
// namespace administrator delegate platform-owned rights.
var namespaceRoleNames = []string{
	"kognition-viewer", "kognition-run-viewer", "kognition-data-reader", "kognition-builder",
	"kognition-curator", "kognition-consumer", "kognition-operator", "kognition-approver",
	"kognition-autonomy-reviewer", "kognition-policy-admin", "kognition-grant-admin",
	"kognition-access-admin", "kognition-auditor",
}

func shippedRoles() []Role {
	runViewer := Grant{Classes: []Class{ClassSubmit, ClassRuntime, ClassDecision}, Verbs: verbsRead}
	return []Role{
		{
			Name: "kognition-viewer", Binding: BindingNamespace, AggregateTo: "view",
			Summary: "Read definitions, knowledge, policy, grants and platform resources in a namespace.",
			Grants:  []Grant{{Classes: []Class{ClassConfig, ClassKnowledge, ClassGovernance, ClassGrant, ClassPlatform}, Verbs: verbsRead}},
		},
		{
			Name: "kognition-run-viewer", Binding: BindingNamespace,
			Summary: "Read runs, execution records and approval decisions, which can contain prompts and results.",
			Grants:  []Grant{runViewer},
		},
		{
			Name: "kognition-data-reader", Binding: BindingNamespace,
			Summary: "Read stored memory content.",
			Grants:  []Grant{{Classes: []Class{ClassData}, Verbs: verbsRead}},
		},
		{
			Name: "kognition-builder", Binding: BindingNamespace, AggregateTo: "edit",
			Summary: "Create and change agent, workflow, prompt, model and tool definitions, and run evaluations.",
			Grants: []Grant{
				{Classes: []Class{ClassConfig}, Verbs: verbsWrite},
				{Kinds: []string{"EvaluationRun"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			},
		},
		{
			Name: "kognition-curator", Binding: BindingNamespace, AggregateTo: "edit",
			Summary: "Manage knowledge bases and their indexing.",
			Grants:  []Grant{{Classes: []Class{ClassKnowledge}, Verbs: verbsWrite}},
		},
		{
			Name: "kognition-consumer", Binding: BindingNamespace,
			Summary: "Submit workflow runs and agent jobs and use authorized workflows and agents.",
			Grants: []Grant{
				{Kinds: []string{"WorkflowRun", "AgentJob"}, Verbs: []string{"create"}},
				{Kinds: []string{"Workflow"}, Verbs: []string{"use"}},
				{Kinds: []string{"Agent"}, Verbs: []string{"invoke"}},
			},
		},
		{
			Name: "kognition-operator", Binding: BindingNamespace,
			Summary: "Inspect runs and reconcile runtime approvals.",
			Grants: []Grant{
				runViewer,
				{Kinds: []string{"RuntimeApproval"}, Subresource: "reconcile", Verbs: []string{"reconcile"}},
			},
		},
		{
			Name: "kognition-approver", Binding: BindingNamespace,
			Summary: "Approve or reject runtime approvals and vote on workflow approval gates.",
			Grants: []Grant{
				{Kinds: []string{"RuntimeApproval"}, Verbs: verbsRead},
				{Kinds: []string{"ApprovalDecision"}, Verbs: []string{"get", "list", "watch", "create"}},
				{Kinds: []string{"RuntimeApproval"}, Subresource: "decision", Verbs: []string{"approve", "reject"}},
			},
		},
		{
			Name: "kognition-autonomy-reviewer", Binding: BindingNamespace,
			Summary: "Decide autonomy reviews.",
			Grants: []Grant{
				{Kinds: []string{"AutonomyReview", "TrustProfile"}, Verbs: verbsRead},
				{Kinds: []string{"AutonomyReview"}, Subresource: "status", Verbs: []string{"update", "patch"}},
			},
		},
		{
			Name: "kognition-policy-admin", Binding: BindingNamespace,
			Summary: "Manage policy, quota, limit and trust configuration.",
			Grants:  []Grant{{Classes: []Class{ClassGovernance}, Verbs: verbsWrite}},
		},
		{
			Name: "kognition-grant-admin", Binding: BindingNamespace,
			Summary: "Manage grants, agent roles and secret references.",
			Grants:  []Grant{{Classes: []Class{ClassGrant}, Verbs: verbsWrite}},
		},
		{
			Name: "kognition-access-admin", Binding: BindingNamespace,
			Summary: "Bind the Kognition roles to users, groups and service accounts in a namespace.",
			Grants: []Grant{
				{APIGroup: rbacAPIGroup, Resources: []string{"roles", "rolebindings"}, Verbs: verbsRead},
				{APIGroup: rbacAPIGroup, Resources: []string{"rolebindings"}, Verbs: []string{"create", "update", "patch", "delete"}},
				{APIGroup: rbacAPIGroup, Resources: []string{"clusterroles"}, Verbs: []string{"bind"}, ResourceNames: namespaceRoleNames},
			},
		},
		{
			Name: "kognition-auditor", Binding: BindingNamespace,
			Summary: "Read all namespaced resources except stored memory content.",
			Grants: []Grant{{
				Classes: []Class{ClassConfig, ClassKnowledge, ClassSubmit, ClassRuntime, ClassGrant, ClassGovernance, ClassDecision, ClassPlatform, ClassSystem},
				Verbs:   verbsRead,
			}},
		},
		{
			Name: "kognition-class-viewer", Binding: BindingCluster,
			Summary: "Read the cluster-scoped classes that definitions refer to.",
			Grants:  []Grant{{Classes: []Class{ClassClass}, Verbs: verbsRead}},
		},
		{
			Name: "kognition-platform-admin", Binding: BindingCluster,
			Summary: "Manage cluster-scoped classes and platform resources, and read internal controller state.",
			Grants: []Grant{
				{Classes: []Class{ClassClass, ClassPlatform}, Verbs: verbsWrite},
				{Classes: []Class{ClassSystem}, Verbs: verbsRead},
			},
		},
	}
}

// Default returns the shipped catalog.
func Default() Catalog {
	return Catalog{Kinds: shippedKinds(), Roles: shippedRoles()}
}
