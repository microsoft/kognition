// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const StudioResources = (() => {
  const resources = new Map();
  const legacy = new Set(["Agent", "Workflow", "Evaluation", "EvaluationDataset", "Tool", "ToolClass",
    "ModelGrant", "MemoryGrant", "CrossNamespaceGrant", "ToolGrant", "KnowledgeBase", "KnowledgeBaseClaim"]);
  let allFieldsKind = "";
  const parents = [];
  let pendingPins = 0;
  const commonKinds = ["Agent", "Workflow", "Tool", "KnowledgeBase", "Prompt", "Model", "MemoryClaim", "Evaluation"];
  const createDescriptions = {
    Agent: "Configure an agent's model, instructions and capabilities.",
    AgentAutoscaler: "Configure automatic agent replica scaling.",
    AgentCommunicationPolicy: "Set rules for communication between agents.",
    AgentIdentity: "Configure an agent's workload identity.",
    AgentInstance: "Start a single agent execution.",
    AgentJob: "Run bounded, one-shot agent work.",
    AgentPolicy: "Configure runtime rules for an agent.",
    AgentRole: "Define a reusable set of agent permissions.",
    AgentRoleBinding: "Assign a role to an agent.",
    AgentService: "Expose agents through a service endpoint.",
    AutonomyClass: "Define autonomy and approval requirements.",
    CognitiveAdmissionPolicy: "Validate resources at admission time.",
    CognitiveLimitRange: "Set resource defaults and limits.",
    CognitiveQuota: "Set namespace resource and usage budgets.",
    CrossNamespaceGrant: "Share resources across namespace boundaries.",
    Evaluation: "Evaluate a target against a dataset and graders.",
    EvaluationDataset: "Author reusable evaluation cases.",
    EvaluationRun: "Start a specific evaluation execution.",
    ExternalAgent: "Register a remote A2A agent that workflow teams can call.",
    ExternalAgentGrant: "Authorize a workflow to send data to a remote agent.",
    IntentGateway: "Route requests to agent services by intent.",
    KnowledgeBase: "Configure a corpus for knowledge retrieval.",
    KnowledgeBaseClaim: "Reference knowledge for a workload.",
    KnowledgeBaseClass: "Define knowledge provisioning and storage.",
    KnowledgeBaseIndexRun: "Start a knowledge ingestion attempt.",
    KnowledgeBaseIndexSchedule: "Configure recurring knowledge indexing.",
    Memory: "Configure a first-class memory resource.",
    MemoryClaim: "Request memory storage for a workload.",
    MemoryClass: "Define memory provisioning and lifecycle.",
    MemoryGrant: "Authorize access to memory.",
    MemoryVolume: "Configure a memory backing volume.",
    Model: "Configure a model and its provider connection.",
    ModelClass: "Define reusable model provisioning.",
    ModelGrant: "Authorize an agent to use a model.",
    ModelInvocationProfile: "Define model invocation defaults and limits.",
    Prompt: "Author reusable instructions and parameters.",
    SecretRef: "Reference a credential without exposing its value.",
    Solution: "Plan and manage a coordinated workload.",
    Tool: "Connect a tool capability to your workloads.",
    ToolClass: "Define tool provisioning and transport.",
    ToolGrant: "Authorize specific tool operations.",
    TrustProfile: "Configure trust levels and transitions.",
    Workflow: "Orchestrate agents and tools in a graph.",
    WorkflowRun: "Start a workflow execution.",
    Workspace: "Configure a workspace boundary.",
  };

  function title(value) {
    return value.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/^./, c => c.toUpperCase());
  }

  function descriptor(kind) { return resources.get(kind); }
  function namespaced(kind) { return descriptor(kind)?.namespaced !== false; }
  function query(plural, ns) {
    const r = [...resources.values()].find(item => item.plural === plural);
    if (r && !namespaced(r.kind)) return state.mode === "kubernetes" ? "?namespace=" : "?namespace=default";
    return ns !== undefined ? `?namespace=${encodeURIComponent(ns)}` : nsParam();
  }
  function editable(draft = state.draft) {
    const r = descriptor(draft?.kind);
    return !!r && (r.treatment === "author" || (r.treatment === "launch" && !state.existsInCluster))
      && !draft.metadata?.ownerReferences?.some(owner => owner.controller)
      && !(r.kind === "MemoryVolume" && (draft.spec?.claimRef || draft.status?.phase === "Bound"))
      && !(r.kind === "AgentIdentity" && draft.status?.workloadIdentity)
      && !(r.kind === "ModelInvocationProfile" && state.existsInCluster);
  }

  async function load() {
    const response = await fetch("/resource-catalog.json");
    if (!response.ok) throw new Error(`resource catalog: ${response.status}`);
    const catalog = await response.json();
    if (!Array.isArray(catalog) || !catalog.length) throw new Error("resource catalog is empty or invalid");
    for (const r of catalog) {
      if (!r.kind || !r.plural || resources.has(r.kind)) throw new Error("invalid or duplicate resource catalog entry");
      resources.set(r.kind, r);
      if (r.treatment !== "unavailable") KIND_PLURAL[r.kind.toLowerCase()] = r.plural;
      const group = r.group.toLowerCase();
      const nav = document.querySelector(`[aria-labelledby="nav-${group}-label"]`);
      let list = document.getElementById(`list-${r.plural}`);
      if (!list) {
        const details = document.createElement("details");
        const summary = document.createElement("summary");
        summary.textContent = title(r.kind);
        details.appendChild(summary);
        list = document.createElement("ul");
        list.id = `list-${r.plural}`;
        list.className = "resource-list mini";
        details.appendChild(list);
        nav.appendChild(details);
      }
      if (r.treatment === "unavailable") {
        const item = document.createElement("li");
        item.className = "empty";
        item.textContent = r.reason;
        list.appendChild(item);
        continue;
      }
      if (!list.parentElement.querySelector(".refresh")) {
        const refresh = document.createElement("button");
        refresh.className = "refresh";
        refresh.type = "button";
        refresh.textContent = "Refresh";
        refresh.setAttribute("aria-label", `Refresh ${title(r.kind)}`);
        refresh.addEventListener("click", () => refreshOne(r.plural));
        list.before(refresh);
      }
      if (!["author", "launch"].includes(r.treatment)) continue;
      if (!createDescriptions[r.kind]) throw new Error(`Missing Create description for ${r.kind}`);
      let button = document.getElementById(`btn-new-${r.kind.toLowerCase()}`);
      if (!button) {
        button = document.createElement("button");
        button.id = `btn-new-${r.kind.toLowerCase()}`;
        button.type = "button";
        button.setAttribute("role", "menuitem");
        button.addEventListener("click", () => create(r.kind));
      }
      button.dataset.kind = r.kind;
      button.dataset.category = r.group;
      button.dataset.common = String(commonKinds.includes(r.kind));
      const heading = document.createElement("span");
      heading.className = "create-item-heading";
      const name = document.createElement("span");
      name.className = "create-item-name";
      name.textContent = r.kind === "CrossNamespaceGrant" ? "Namespace grant" :
        title(r.kind).replace(/\S+/g, (word, index) => index === 0 ? word : word.toLowerCase());
      const category = document.createElement("small");
      category.className = "create-item-category";
      category.textContent = r.treatment === "launch" ? `${r.group} / Run` : r.group;
      heading.append(name, category);
      const description = document.createElement("small");
      description.className = "create-item-description";
      description.textContent = createDescriptions[r.kind];
      button.replaceChildren(heading, description);
      document.getElementById("create-results").appendChild(button);
    }
    const results = document.getElementById("create-results");
    const rank = button => commonKinds.includes(button.dataset.kind) ? commonKinds.indexOf(button.dataset.kind) : commonKinds.length;
    [...results.children].sort((a, b) => rank(a) - rank(b) ||
      a.dataset.kind.localeCompare(b.dataset.kind)).forEach(button => results.appendChild(button));
    filterCreateResources();
  }

  function targets() { return [...resources.values()].filter(r => r.treatment !== "unavailable").map(r => r.plural); }
  function kind(plural) { return [...resources.values()].find(r => r.plural === plural)?.kind; }

  function create(kind, preserveParent = false) {
    if (!preserveParent && !confirmDiscard()) return false;
    const r = descriptor(kind);
    if (!r || !["author", "launch"].includes(r.treatment)) {
      setStatus("This resource is not authorable through Studio.", "error");
      return false;
    }
    stopViewerPolling();
    state.draft = { apiVersion: API_VERSION, kind, metadata: { name: "" } };
    const definition = state.mode === "kubernetes" ? r.kubernetesSchema : r.schema;
    if (definition?.properties?.spec) state.draft.spec = {};
    if (namespaced(kind)) state.draft.metadata.namespace = state.namespace || "default";
    state.draftKey = null;
    state.existsInCluster = false;
    state.dirty = true;
    state.view = "builder";
    allFieldsKind = !legacy.has(kind) ? kind : "";
    closeInspector();
    switchView("builder");
    refreshSidebarActive();
    return true;
  }

  function schema() {
    const r = descriptor(state.draft.kind);
    return state.mode === "kubernetes" ? r?.kubernetesSchema : r?.schema;
  }

  function set(path, value) {
    if (path.some(key => ["__proto__", "prototype", "constructor"].includes(String(key)))) {
      setStatus("Reserved object keys cannot be edited.", "error");
      return;
    }
    let node = state.draft;
    for (const key of path.slice(0, -1)) {
      if (!node[key] || typeof node[key] !== "object") node[key] = {};
      node = node[key];
    }
    const key = path.at(-1);
    if (value === undefined) delete node[key];
    else node[key] = value;
    if (state.draft.kind === "Model" && path.join(".") === "spec.invocationProfileRef.name") {
      delete node.specDigest;
      const digest = document.getElementById("resource-spec-invocationProfileRef-specDigest");
      if (digest) digest.value = "";
    }
    markDirty();
    updateTitle();
  }

  function get(path) { return path.reduce((value, key) => value?.[key], state.draft); }

  function reference(path) {
    const parent = path.at(-2);
    const name = path.at(-1);
    const refs = {
      modelRef: "models", embeddingModelRef: "models", embeddingModel: "models", agentRef: "agents",
      autonomyClassRef: "autonomyclasses", memoryClaimRef: "memoryclaims", workflowRef: "workflows",
      credentialRef: "secretrefs", invocationProfileRef: "modelinvocationprofiles", roleRef: "agentroles",
      knowledgeBaseRef: "knowledgebases", datasetRef: "evaluationdatasets",
    };
    if (name === "name" && refs[parent]) return refs[parent];
    if (name === "className") {
      return { Model: "modelclasses", Tool: "toolclasses", MemoryClaim: "memoryclasses", KnowledgeBase: "knowledgebaseclasses" }[state.draft.kind];
    }
    return {
      memoryClassName: "memoryclasses", knowledgeBaseClassName: "knowledgebaseclasses",
      initialClass: "autonomyclasses", maxClass: "autonomyclasses", targetClass: "autonomyclasses",
    }[name];
  }

  function empty(s) {
    if (s.type === "object" || s.properties) return {};
    if (s.type === "array") return [];
    if (s.type === "boolean") return false;
    if (s.type === "integer" || s.type === "number") return 0;
    return "";
  }

  function input(s, path, required = false) {
    const value = get(path);
    const label = path.at(-1) === "name" && path[0] !== "metadata" && path.length > 2
      ? `${title(String(path.at(-2)))} Name` : title(String(path.at(-1)));
    if (s.type === "object" || s.properties || s.additionalProperties) {
      const box = document.createElement("details");
      box.className = "schema-object";
      box.open = required || value !== undefined;
      const heading = document.createElement("summary");
      heading.textContent = label;
      box.appendChild(heading);
      const body = document.createElement("div");
      body.className = "schema-children";
      box.appendChild(body);
      let built = false;
      const build = () => {
        if (built) return;
        built = true;
        if (s.properties) {
          for (const [name, child] of Object.entries(s.properties)) body.appendChild(input(child, [...path, name], s.required?.includes(name)));
        } else {
          mapFields(body, s.additionalProperties === true ? {} : s.additionalProperties || {}, path);
        }
        if (s.readOnly) body.querySelectorAll("input, select, textarea, button").forEach(control => { control.disabled = true; });
      };
      box.addEventListener("toggle", () => { if (box.open) build(); });
      if (box.open) build();
      if (s.description) box.title = s.description;
      if (s.readOnly) box.setAttribute("data-read-only", "");
      return box;
    }
    if (s.type === "array") {
      const box = document.createElement("div");
      box.className = "schema-array field";
      const heading = document.createElement("strong");
      heading.textContent = label;
      box.appendChild(heading);
      const rows = document.createElement("div");
      box.appendChild(rows);
      const render = () => {
        rows.replaceChildren();
        const items = get(path) || [];
        items.forEach((item, index) => {
          const row = document.createElement("div");
          row.className = "schema-item";
          row.appendChild(input(s.items || {}, [...path, index], true));
          const remove = document.createElement("button");
          remove.type = "button";
          remove.className = "action";
          remove.textContent = `Remove ${label} item ${index + 1}`;
          remove.addEventListener("click", () => { const next = [...get(path)]; next.splice(index, 1); set(path, next); render(); });
          row.appendChild(remove);
          rows.appendChild(row);
        });
      };
      const add = document.createElement("button");
      add.type = "button";
      add.className = "action";
      add.textContent = `Add ${label} item`;
      add.addEventListener("click", () => { set(path, [...(get(path) || []), empty(s.items || {})]); render(); });
      box.appendChild(add);
      render();
      return box;
    }
    let control;
    if (s.enum || s.type === "boolean") {
      control = document.createElement("select");
      const options = s.enum || [true, false];
      control.appendChild(new Option("Not set", ""));
      for (const option of options) control.appendChild(new Option(String(option), String(option)));
      if (value !== undefined && !options.some(option => String(option) === String(value))) {
        control.appendChild(new Option(`${value} (imported)`, String(value)));
      }
      control.value = value === undefined ? "" : String(value);
      control.addEventListener("change", () => set(path, control.value === "" ? undefined : s.type === "boolean" ? control.value === "true" : control.value));
    } else if (s.type === "integer" || s.type === "number") {
      control = document.createElement("input");
      control.type = "number";
      control.step = s.type === "integer" ? "1" : "any";
      if (s.minimum !== undefined) control.min = s.minimum;
      if (s.maximum !== undefined) control.max = s.maximum;
      control.value = value ?? "";
      control.addEventListener("input", () => {
        const number = Number(control.value);
        control.setCustomValidity(control.value !== "" && (!Number.isFinite(number) || (s.type === "integer" && !Number.isSafeInteger(number)))
          ? "Enter a safely representable number." : "");
        if (control.checkValidity()) set(path, control.value === "" ? undefined : number);
      });
    } else if (!s.type && !s.enum) {
      control = document.createElement("textarea");
      control.value = value === undefined ? "" : JSON.stringify(value, null, 2);
      control.addEventListener("input", () => {
        try {
          const parsed = control.value.trim() ? JSON.parse(control.value) : undefined;
          control.setCustomValidity("");
          set(path, parsed);
        } catch (error) {
          control.setCustomValidity(`Invalid JSON payload: ${error.message}`);
        }
      });
    } else {
      const multiline = /content|instructions|template|rules|prompt|expression|^data$/i.test(String(path.at(-1))) ||
        (path.at(-2) === "instructions" && path.at(-1) === "inline");
      control = document.createElement(multiline ? "textarea" : "input");
      if (!multiline) control.type = "text";
      control.value = value ?? "";
      if (s.pattern) control.pattern = s.pattern;
      const ref = reference(path);
      if (ref && !multiline) {
        const list = document.createElement("datalist");
        list.id = `reference-${path.join("-")}`;
        for (const item of state.lists[ref] || []) {
          const ns = item.metadata?.namespace;
          if (ns && ns !== state.draft.metadata.namespace && namespaced(kind(ref))) continue;
          list.appendChild(new Option(item.metadata?.name || item.name));
        }
        control.setAttribute("list", list.id);
        control._referenceList = list;
      }
      control.addEventListener("input", () => set(path, control.value === "" ? undefined : control.value));
    }
    control.id = `resource-${path.join("-")}`;
    control.required = !!required;
    control.dataset.required = String(!!required);
    control.disabled = !!s.readOnly;
    control.dataset.path = path.join(".");
    const wrapper = field(label, control, { required, hint: s.description });
    if (control._referenceList) wrapper.appendChild(control._referenceList);
    const ref = reference(path);
    const targetKind = kind(ref);
    if (ref && targetKind && !s.readOnly) {
      const createReference = document.createElement("button");
      createReference.type = "button";
      createReference.className = "action";
      createReference.textContent = `Create referenced ${title(targetKind)}`;
      createReference.addEventListener("click", () => {
        parents.push({ draft: clone(state.draft), draftKey: state.draftKey, existsInCluster: state.existsInCluster,
          dirty: state.dirty, allFieldsKind, path });
        const ns = state.draft.metadata.namespace;
        create(targetKind, true);
        if (namespaced(targetKind) && ns) { state.draft.metadata.namespace = ns; renderEditor(); }
      });
      wrapper.appendChild(createReference);
    }
    if (state.draft.kind === "Model" && path.join(".") === "spec.invocationProfileRef.name") {
      const pin = document.createElement("button");
      pin.type = "button";
      pin.className = "action";
      pin.textContent = "Pin selected profile digest";
      pin.addEventListener("click", async () => {
        const draft = state.draft;
        const selected = control.value;
        pin.disabled = true;
        pendingPins++;
        try {
          if (!selected) throw new Error("Select a profile first.");
          const profile = await apiGet("modelinvocationprofiles", selected, draft.metadata.namespace);
          const response = await studioFetch("/api/studio/profile-digest", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(profile) });
          const result = await response.json();
          if (!response.ok || typeof result.digest !== "string") throw new Error(result.error || "Profile digest is unavailable.");
          if (state.draft === draft && get(path) === selected) {
            set(["spec", "invocationProfileRef", "specDigest"], result.digest);
            renderEditor();
          }
        } catch (error) {
          setStatus(`Profile pin failed: ${error.message}`, "error");
        } finally {
          pendingPins--;
          pin.disabled = false;
        }
      });
      wrapper.appendChild(pin);
    }
    return wrapper;
  }

  function mapFields(body, itemSchema, path) {
    const keys = Object.keys(get(path) || {});
    if (!keys.length) {
      const hint = document.createElement("div");
      hint.className = "hint";
      hint.textContent = `No ${title(String(path.at(-1))).toLowerCase()} configured. Add a key, then enter its value.`;
      body.appendChild(hint);
    }
    for (const key of keys) {
      const row = document.createElement("div");
      row.className = "schema-item";
      row.appendChild(input(itemSchema, [...path, key]));
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "action";
      remove.textContent = `Remove ${key}`;
      remove.addEventListener("click", () => { set([...path, key], undefined); body.replaceChildren(); mapFields(body, itemSchema, path); });
      row.appendChild(remove);
      body.appendChild(row);
    }
    const row = document.createElement("div");
    row.className = "schema-map-add";
    const key = document.createElement("input");
    key.type = "text";
    key.setAttribute("aria-label", `${title(String(path.at(-1)))} entry key`);
    key.placeholder = path.at(-1) === "labels" ? "e.g. app.kubernetes.io/part-of" :
      path.at(-1) === "annotations" ? "e.g. tutorial.kognition.dev/description" : "Enter a unique key";
    const add = document.createElement("button");
    add.type = "button";
    add.className = "action";
    add.textContent = "Add entry";
    add.addEventListener("click", () => {
      const name = key.value.trim();
      if (!name || ["__proto__", "prototype", "constructor"].includes(name) || Object.hasOwn(get(path) || {}, name)) {
        key.setCustomValidity("Enter a nonempty, unique, safe key.");
        key.reportValidity();
        return;
      }
      key.setCustomValidity("");
      set([...path, name], empty(itemSchema));
      body.replaceChildren();
      mapFields(body, itemSchema, path);
    });
    row.append(field("New entry key", key), add);
    body.appendChild(row);
  }

  function render(host) {
    const r = descriptor(state.draft?.kind);
    if (!r) return false;
    const writable = editable();
    if (["WorkflowRun", "EvaluationRun"].includes(r.kind) && state.existsInCluster) return false;
    const useSchema = allFieldsKind === r.kind || !legacy.has(r.kind) || !writable;
    const toolbar = document.createElement("div");
    toolbar.className = "resource-toolbar";
    if (parents.length) {
      const parent = parents.at(-1);
      const back = document.createElement("button");
      back.type = "button";
      back.className = "action";
      back.textContent = `Return to ${parent.draft.kind} draft`;
      back.addEventListener("click", () => {
        const name = !state.dirty && state.existsInCluster ? state.draft.metadata.name : "";
        if (!confirmDiscard()) return;
        parents.pop();
        Object.assign(state, { draft: parent.draft, draftKey: parent.draftKey, existsInCluster: parent.existsInCluster, dirty: parent.dirty });
        allFieldsKind = parent.allFieldsKind;
        if (name) set(parent.path, name);
        renderEditor();
        refreshSidebarActive();
      });
      toolbar.appendChild(back);
    }
    if (legacy.has(r.kind) && writable) {
      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "action";
      toggle.textContent = useSchema ? "Guided builder" : "All API fields";
      toggle.addEventListener("click", () => { allFieldsKind = useSchema ? "" : r.kind; renderEditor(); });
      toolbar.appendChild(toggle);
    }
    host.appendChild(toolbar);
    if (!useSchema) return false;
    if (!writable || r.reason || r.kind === "SecretRef" || r.kind === "AutonomyClass") {
      const banner = document.createElement("div");
      banner.className = "banner info";
      banner.textContent = !writable ? (r.reason || "This resource is immutable or controller-owned. Its fields and status are read-only.")
        : r.kind === "SecretRef" ? "References only. Studio never resolves secret values. Supported backends: internal and env."
          : r.kind === "AutonomyClass" ? "Supported Tool, Memory and Communication policies only. Model approval/cost-threshold enforcement is not added by this builder." : r.reason;
      host.appendChild(banner);
    }
    if (r.kind === "ModelInvocationProfile" && state.existsInCluster) {
      const revision = document.createElement("button");
      revision.type = "button";
      revision.className = "action";
      revision.textContent = "Create new profile revision";
      revision.addEventListener("click", () => {
        const spec = clone(state.draft.spec);
        const ns = state.draft.metadata.namespace;
        if (!create(r.kind)) return;
        state.draft.spec = spec;
        if (ns) state.draft.metadata.namespace = ns;
        renderEditor();
      });
      host.appendChild(revision);
    }
    const fields = document.createElement("fieldset");
    fields.className = "resource-fields";
    fields.disabled = !writable;
    host.appendChild(fields);
    fields.appendChild(section("Identity", namespaced(r.kind) ? "Resource metadata." : "Cluster-scoped resource; no namespace is sent.", body => {
      body.appendChild(input({ type: "string" }, ["metadata", "name"], true));
      if (namespaced(r.kind)) body.appendChild(input({ type: "string" }, ["metadata", "namespace"], true));
      body.appendChild(input({ type: "object", additionalProperties: { type: "string" } }, ["metadata", "labels"]));
      body.appendChild(input({ type: "object", additionalProperties: { type: "string" } }, ["metadata", "annotations"]));
    }, true));
    const definition = schema();
    if (definition) {
      for (const [name, child] of Object.entries(definition.properties || {})) fields.appendChild(input(child, [name], true));
    }
    if (state.draft.status) {
      host.appendChild(section("Observed status", "Server-owned state, never editable.", body => {
        const pre = document.createElement("pre");
        pre.textContent = JSON.stringify(state.draft.status, null, 2);
        body.appendChild(pre);
      }, true));
    }
    return true;
  }

  function validate() {
    if (!editable()) { setStatus("This resource is read-only or unavailable through Studio.", "error"); return false; }
    if (pendingPins) { setStatus("Wait for profile digest binding to finish before deploying.", "error"); return false; }
    const unsafeNumber = value => {
      if (typeof value === "number") return !Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value));
      return value && typeof value === "object" && Object.values(value).some(unsafeNumber);
    };
    if (unsafeNumber(state.draft)) {
      setStatus("This manifest contains numbers outside the browser's safe range. Use the CLI to preserve their exact values.", "error");
      return false;
    }
    document.querySelectorAll("#builder-host [data-path]").forEach(control => {
      const path = control.dataset.path.split(".");
      control.required = control.dataset.required === "true" && get(path.slice(0, -1)) !== undefined;
    });
    const invalid = document.querySelector("#builder-host :invalid");
    if (invalid) {
      invalid.closest("details")?.setAttribute("open", "");
      invalid.reportValidity();
      setStatus("Correct the highlighted field before deploying.", "error");
      return false;
    }
    return true;
  }

  function executionChange() {
    return descriptor(state.draft?.kind)?.treatment === "launch"
      || ["Solution", "KnowledgeBase", "KnowledgeBaseIndexSchedule"].includes(state.draft?.kind);
  }

  return { load, targets, kind, descriptor, namespaced, query, editable, create, render, validate, executionChange };
})();
