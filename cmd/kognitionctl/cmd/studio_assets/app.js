// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

/* Kognition Studio — visual agent + workflow builder */
"use strict";

const KIND_PLURAL = {
  agent: "agents",
  workflow: "workflows",
  model: "models",
  modelinvocationprofile: "modelinvocationprofiles",
  modelgrant: "modelgrants",
  memorygrant: "memorygrants",
  crossnamespacegrant: "crossnamespacegrants",
  toolgrant: "toolgrants",
  tool: "tools",
  toolclass: "toolclasses",
  secretref: "secretrefs",
  prompt: "prompts",
  memoryclaim: "memoryclaims",
  autonomyclass: "autonomyclasses",
  knowledgebase: "knowledgebases",
  knowledgebaseclass: "knowledgebaseclasses",
  knowledgebaseclaim: "knowledgebaseclaims",
  evaluation: "evaluations",
  evaluationdataset: "evaluationdatasets",
  evaluationrun: "evaluationruns",
};
const API_VERSION = "kognition.dev/v1alpha1";

// ============================================================================
// State
// ============================================================================
const state = {
  mode: "standalone",
  namespace: "",
  namespaces: [],
  lists: {}, // plural -> [{name, namespace, ...}]
  draft: null, // { apiVersion, kind, metadata, spec }
  draftKey: null, // `${kind}/${ns}/${name}` for current draft
  existsInCluster: false,
  dirty: false,
  view: "builder", // builder | yaml
  selectedStep: null, // workflow: primary single-selected step name (for inspector)
  selectedEdge: null, // workflow: primary single-selected edge { from, to }
  selectedSteps: new Set(), // workflow: all currently-selected step names
  selectedEdges: [], // workflow: all currently-selected edges [{from,to}]
  linkFrom: null, // workflow: step being linked from
  linkPreview: null, // workflow: { path, startX, startY } live SVG preview while linking
  linkSnapshot: null, // workflow: pre-link state used by undo
  marquee: null, // workflow: in-progress marquee { startX, startY, el, moved }
  canvasPositions: {}, // step name -> {x,y}
  canvasZoom: 1,
  canvasViewport: { left: 0, top: 0 },
  canvasSpacePanning: false,
  workflowPanel: "outline",
  workflowHistory: { undo: [], redo: [] },
  // Preview tab state, keyed by `${kind}/${ns}/${name}`.
  // chats: { sessionID, history: [{role,content,toolCalls?,error?}], pending: bool }
  // runs:  { runName, ns, status?, error?, polling: bool, input, parameters }
  // openBlocks: persists <details> open state across run status changes.
  preview: { chats: {}, runs: {}, tools: {}, openBlocks: {} },
  previewPollers: {}, // key -> intervalId
  viewerPoller: null, // interval id used by renderWorkflowRunViewer to keep state.draft.status fresh
};

// ============================================================================
// API helpers
// ============================================================================
function studioFetch(path, options) {
  return StudioTutorial.isPractice() ? StudioTutorial.request(path, options) : fetch(path, options);
}

function nsParam() {
  // An empty namespace means "all namespaces" in kubernetes mode; omitting the param would fall back to the server default.
  if (state.mode === "kubernetes") return `?namespace=${encodeURIComponent(state.namespace || "")}`;
  return state.namespace ? `?namespace=${encodeURIComponent(state.namespace)}` : "";
}
async function apiMeta() {
  const r = await studioFetch("/api/meta");
  if (!r.ok) throw new Error(`metadata: ${r.status}`);
  return r.json();
}
async function apiNamespaces() {
  const r = await studioFetch("/api/namespaces");
  if (!r.ok) throw new Error(`namespaces: ${r.status}`);
  return r.json();
}
async function apiList(plural) {
  const r = await studioFetch(`/api/resources/${plural}${StudioResources.query(plural)}`);
  if (!r.ok) throw new Error(`list ${plural}: ${r.status}`);
  const data = await r.json();
  return data.items || [];
}
async function apiGet(plural, name, ns) {
  const q = StudioResources.query(plural, ns);
  const r = await studioFetch(`/api/resources/${plural}/${encodeURIComponent(name)}${q}`);
  if (!r.ok) throw new Error(`get ${plural}/${name}: ${r.status}`);
  return r.json();
}
async function apiCreate(plural, body, ns) {
  const q = StudioResources.query(plural, ns);
  const r = await studioFetch(`/api/resources/${plural}${q}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    const txt = await r.text();
    throw new Error(safeErr(txt) || `create failed (${r.status})`);
  }
  return r.json();
}
async function apiUpdate(plural, name, body, ns) {
  const q = StudioResources.query(plural, ns);
  const r = await studioFetch(`/api/resources/${plural}/${encodeURIComponent(name)}${q}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    const txt = await r.text();
    throw new Error(safeErr(txt) || `update failed (${r.status})`);
  }
  return r.json();
}
async function apiDelete(plural, name, ns) {
  const q = StudioResources.query(plural, ns);
  const r = await studioFetch(`/api/resources/${plural}/${encodeURIComponent(name)}${q}`, {
    method: "DELETE",
  });
  if (!r.ok) {
    const txt = await r.text();
    throw new Error(safeErr(txt) || `delete failed (${r.status})`);
  }
  return r.json();
}
async function apiInvokeAgent(name, body, ns) {
  const q = ns ? `?namespace=${encodeURIComponent(ns)}` : nsParam();
  const r = await studioFetch(`/api/invoke/agent/${encodeURIComponent(name)}${q}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  if (!r.ok) throw new Error(safeErr(text) || `invoke failed (${r.status})`);
  try { return JSON.parse(text); } catch (_) { return { response: text }; }
}
async function apiInvokeWorkflow(name, body, ns) {
  const q = ns ? `?namespace=${encodeURIComponent(ns)}` : nsParam();
  const r = await studioFetch(`/api/invoke/workflow/${encodeURIComponent(name)}${q}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  if (!r.ok) throw new Error(safeErr(text) || `run failed (${r.status})`);
  try { return JSON.parse(text); } catch (_) { return null; }
}
async function apiInvokeTool(name, body, ns) {
  const q = ns ? `?namespace=${encodeURIComponent(ns)}` : nsParam();
  const r = await studioFetch(`/api/invoke/tool/${encodeURIComponent(name)}${q}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  // Tool-invoke returns structured JSON on both success and failure; we surface
  // both to the caller so the tester can render error codes / retryable flags.
  let parsed = null;
  try { parsed = JSON.parse(text); } catch (_) { parsed = { error: text }; }
  if (!r.ok) {
    const err = new Error(parsed?.error || `invoke failed (${r.status})`);
    err.detail = parsed;
    throw err;
  }
  return parsed;
}
async function apiToolSchema(name, ns) {
  const q = ns ? `?namespace=${encodeURIComponent(ns)}` : nsParam();
  const r = await studioFetch(`/api/tools/${encodeURIComponent(name)}/schema${q}`);
  const text = await r.text();
  let parsed = null;
  try { parsed = JSON.parse(text); } catch (_) { parsed = { error: text }; }
  if (!r.ok) {
    const err = new Error(parsed?.error || `schema fetch failed (${r.status})`);
    err.detail = parsed;
    throw err;
  }
  return parsed;
}
async function apiParseYAML(yaml) {
  const r = await studioFetch("/api/parse-yaml", {
    method: "POST",
    headers: { "Content-Type": "application/yaml" },
    body: yaml,
  });
  if (!r.ok) {
    const txt = await r.text();
    throw new Error(safeErr(txt) || `parse failed (${r.status})`);
  }
  return r.json();
}
async function apiGalleryList() {
  const r = await studioFetch("/api/gallery");
  if (!r.ok) throw new Error(`gallery list failed (${r.status})`);
  return r.json();
}
async function apiGalleryImport(id, ns) {
  const q = ns != null ? `?namespace=${encodeURIComponent(ns)}` : "";
  const r = await studioFetch(`/api/gallery/${encodeURIComponent(id)}/import${q}`, { method: "POST" });
  const body = await r.json().catch(() => ({}));
  if (!r.ok && r.status !== 207) {
    throw new Error(safeErr(body.error || JSON.stringify(body)) || `import failed (${r.status})`);
  }
  return body;
}
function safeErr(text) {
  try {
    const o = JSON.parse(text);
    return o.error || o.message || text;
  } catch (_) {
    return text;
  }
}

// ============================================================================
// YAML serializer (mirror of dashboard's toYaml)
// ============================================================================
function toYaml(value, indent = 0) {
  const pad = "  ".repeat(indent);
  if (value === null || value === undefined) return "null";
  if (typeof value === "string") {
    if (value === "") return '""';
    if (value.includes("\n")) {
      const trailing = value.endsWith("\n") ? "" : "-";
      const lines = value.replace(/\n$/, "").split("\n").map((l) => "  ".repeat(indent + 1) + l);
      return `|${trailing}\n${lines.join("\n")}`;
    }
    if (/^[\s\-?:,\[\]\{\}#&*!|>'"%@`]/.test(value) || /[:#]\s/.test(value) || /^\s|\s$/.test(value) || /^(true|false|null|yes|no|on|off|~|-?\d)/i.test(value)) {
      return JSON.stringify(value);
    }
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value
      .map((v) => {
        if (v && typeof v === "object" && !Array.isArray(v)) {
          const inner = toYaml(v, indent + 1);
          const lines = inner.split("\n");
          const first = lines.shift().replace(/^\s+/, "");
          return `${pad}- ${first}` + (lines.length ? "\n" + lines.join("\n") : "");
        }
        return `${pad}- ${toYaml(v, indent + 1)}`;
      })
      .join("\n");
  }
  if (typeof value === "object") {
    const keys = Object.keys(value);
    if (keys.length === 0) return "{}";
    return keys
      .map((k) => {
        const v = value[k];
        const key = /^[A-Za-z_][A-Za-z0-9_./-]*$/.test(k) && !/^(true|false|null|yes|no|on|off|y|n)$/i.test(k) ? k : JSON.stringify(k);
        if (v === undefined || v === null) return `${pad}${key}: null`;
        if (typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0) return `${pad}${key}: {}`;
        if (Array.isArray(v) && v.length === 0) return `${pad}${key}: []`;
        if (typeof v === "object") return `${pad}${key}:\n${toYaml(v, indent + 1)}`;
        return `${pad}${key}: ${toYaml(v, indent + 1)}`;
      })
      .join("\n");
  }
  return JSON.stringify(value);
}

// Deep clone (drops undefined). Used so updates don't share references.
function clone(o) {
  return o === undefined ? undefined : JSON.parse(JSON.stringify(o));
}

function setOrDel(obj, key, value) {
  const isEmptyObj = value && typeof value === "object" && !Array.isArray(value) && Object.keys(value).length === 0;
  const isEmptyArr = Array.isArray(value) && value.length === 0;
  if (value === undefined || value === null || value === "" || isEmptyObj || isEmptyArr) delete obj[key];
  else obj[key] = value;
}

// Strip empty/optional fields so saved YAML stays minimal.
function prune(o) {
  if (Array.isArray(o)) {
    const arr = o.map(prune).filter((v) => v !== undefined);
    return arr.length ? arr : undefined;
  }
  if (o && typeof o === "object") {
    const out = {};
    let any = false;
    for (const k of Object.keys(o)) {
      const v = prune(o[k]);
      if (v !== undefined && v !== "" && !(Array.isArray(v) && v.length === 0)) {
        out[k] = v;
        any = true;
      }
    }
    return any ? out : undefined;
  }
  if (o === "" || o === null || o === undefined) return undefined;
  return o;
}

// ============================================================================
// Boot
// ============================================================================
async function boot() {
  setStatus("loading…", "info");
  try {
    const meta = await apiMeta();
    state.mode = meta.mode;
    state.namespace = meta.namespace || "";
    document.getElementById("meta-mode").textContent = meta.mode;
    await StudioTutorial.configure(meta);
    await StudioResources.load();

    let namespaceWarning = "";
    if (state.mode === "kubernetes") {
      try {
        const ns = await apiNamespaces();
        state.namespaces = Array.isArray(ns) ? ns : [];
      } catch (error) {
        state.namespaces = [];
        namespaceWarning = `Namespace discovery failed: ${error.message}. Using the explicitly selected namespace.`;
      }
    }
    populateNamespacePicker();
    const refreshed = await refreshAll();
    if (refreshed) setStatus(namespaceWarning || "ready", namespaceWarning ? "error" : "");
    else setStatus("Some resource lists failed. See the explorer for details.", "error");
    await StudioTutorial.restore();
  } catch (e) {
    setStatus(`init failed: ${e.message}`, "error");
  }
}

function setStatus(msg, level) {
  if (StudioTutorial.isPractice() && msg.startsWith("deployed ")) msg = `Saved in practice: ${msg.slice(9)}`;
  const el = document.getElementById("status");
  el.textContent = msg;
  el.className = "status " + (level || "");
  if (level && level !== "error") setTimeout(() => { if (el.textContent === msg) { el.textContent = ""; el.className = "status"; } }, 4000);
}

function populateNamespacePicker() {
  const sel = document.getElementById("namespace-select");
  sel.innerHTML = "";
  const all = document.createElement("option");
  all.value = "";
  all.textContent = state.mode === "kubernetes" ? "all namespaces" : "(server-side)";
  sel.appendChild(all);
  for (const ns of state.namespaces) {
    const o = document.createElement("option");
    o.value = ns; o.textContent = ns;
    if (ns === state.namespace) o.selected = true;
    sel.appendChild(o);
  }
  if (!state.namespaces.includes(state.namespace) && state.namespace) {
    const o = document.createElement("option");
    o.value = state.namespace; o.textContent = state.namespace; o.selected = true;
    sel.appendChild(o);
  }
  sel.value = state.namespace;
}

async function refreshAll() {
  const results = await Promise.all(StudioResources.targets().map((t) => refreshOne(t)));
  return results.every(Boolean);
}

let namespaceRevision = 0;

async function refreshOne(plural) {
  const revision = namespaceRevision;
  try {
    const items = await apiList(plural);
    if (revision !== namespaceRevision) return false;
    state.lists[plural] = items;
    renderList(plural);
    return true;
  } catch (e) {
    if (revision !== namespaceRevision) return false;
    const list = document.getElementById(`list-${plural}`);
    if (list) {
      list.replaceChildren();
      const error = document.createElement("li");
      error.className = "banner error";
      error.textContent = e.message;
      list.appendChild(error);
    }
    setStatus(`refresh failed: ${e.message}`, "error");
    return false;
  }
}

function renderList(plural) {
  const el = document.getElementById(`list-${plural}`);
  if (!el) return;
  const items = state.lists[plural] || [];
  const count = document.getElementById(`count-${plural}`);
  if (count) count.textContent = items.length ? `(${items.length})` : "";

  el.innerHTML = "";
  if (items.length === 0) {
    const li = document.createElement("li");
    li.className = "empty";
    li.textContent = "— none —";
    el.appendChild(li);
    return;
  }
  for (const it of items) {
    const li = document.createElement("li");
    const name = (it.metadata && it.metadata.name) || it.name || "?";
    const ns = (it.metadata && it.metadata.namespace) || it.namespace || "";
    const nameEl = document.createElement("span");
    nameEl.textContent = name;
    const nsEl = document.createElement("span");
    nsEl.className = "ns";
    nsEl.textContent = ns;
    li.appendChild(nameEl);
    li.appendChild(nsEl);
    li.addEventListener("click", () => selectResource(plural, name, ns));
    if (state.draft && state.draftKey === keyOf(plural, name, ns)) li.classList.add("active");
    el.appendChild(li);
  }
}

function keyOf(plural, name, ns) {
  return `${plural}/${ns || ""}/${name}`;
}

function singularOf(plural) {
  for (const [s, p] of Object.entries(KIND_PLURAL)) if (p === plural) return s;
  return plural.replace(/s$/, "");
}

function kindOfPlural(plural) {
  const known = StudioResources.kind(plural);
  if (known) return known;
  const map = { agents: "Agent", workflows: "Workflow", models: "Model", modelinvocationprofiles: "ModelInvocationProfile", modelgrants: "ModelGrant", memorygrants: "MemoryGrant", crossnamespacegrants: "CrossNamespaceGrant", toolgrants: "ToolGrant", tools: "Tool", toolclasses: "ToolClass", secretrefs: "SecretRef", prompts: "Prompt", memoryclaims: "MemoryClaim", autonomyclasses: "AutonomyClass", knowledgebases: "KnowledgeBase", knowledgebaseclasses: "KnowledgeBaseClass", knowledgebaseclaims: "KnowledgeBaseClaim", evaluations: "Evaluation", evaluationdatasets: "EvaluationDataset", evaluationruns: "EvaluationRun" };
  return map[plural] || plural;
}

const WORKFLOW_LAYOUT_STORAGE_PREFIX = "kognition.studio.workflow-layout.v1";

function workflowLayoutStorageKey(wf) {
  if (StudioTutorial.isPractice()) return "";
  if (!wf || wf.kind !== "Workflow" || !wf.metadata?.name) return "";
  const namespace = wf.metadata.namespace || state.namespace || "default";
  return `${WORKFLOW_LAYOUT_STORAGE_PREFIX}:${state.mode}:${namespace}:${wf.metadata.name}`;
}

function loadWorkflowLayout(wf) {
  const fallback = autoLayout(wf);
  const key = workflowLayoutStorageKey(wf);
  if (!key) return fallback;
  try {
    const stored = JSON.parse(localStorage.getItem(key) || "null");
    if (!stored || typeof stored !== "object" || Array.isArray(stored)) return fallback;
    const positions = {};
    for (const step of wf.spec?.steps || []) {
      const point = stored[step.name];
      positions[step.name] = point && Number.isFinite(point.x) && Number.isFinite(point.y)
        ? { x: Math.max(0, point.x), y: Math.max(0, point.y) }
        : fallback[step.name];
    }
    return positions;
  } catch (_) {
    return fallback;
  }
}

function saveWorkflowLayout(wf = state.draft) {
  const key = workflowLayoutStorageKey(wf);
  if (!key) return;
  try {
    localStorage.setItem(key, JSON.stringify(state.canvasPositions));
  } catch (_) { /* Browser storage can be unavailable; layout remains session-local. */ }
}

function workflowSnapshot() {
  return {
    spec: clone(state.draft?.spec || {}),
    positions: clone(state.canvasPositions || {}),
    dirty: state.dirty,
  };
}

function resetWorkflowHistory() {
  state.workflowHistory = { undo: [], redo: [], coalesced: null };
}

function syncWorkflowHistoryControls() {
  const undo = document.getElementById("workflow-undo");
  const redo = document.getElementById("workflow-redo");
  if (undo) undo.disabled = state.workflowHistory.undo.length === 0;
  if (redo) redo.disabled = state.workflowHistory.redo.length === 0;
}

function recordWorkflowHistory(label, snapshot = workflowSnapshot(), coalesceKey = null) {
  if (!state.draft || state.draft.kind !== "Workflow") return;
  const now = Date.now();
  if (coalesceKey && state.workflowHistory.coalesced?.key === coalesceKey
      && now - state.workflowHistory.coalesced.at < 1000) {
    state.workflowHistory.coalesced.at = now;
    state.workflowHistory.redo = [];
    syncWorkflowHistoryControls();
    return;
  }
  state.workflowHistory.undo.push({ label, snapshot });
  if (state.workflowHistory.undo.length > 50) state.workflowHistory.undo.shift();
  state.workflowHistory.redo = [];
  state.workflowHistory.coalesced = coalesceKey ? { key: coalesceKey, at: now } : null;
  syncWorkflowHistoryControls();
}

function editWorkflow(label, mutate, options = {}) {
  recordWorkflowHistory(label, workflowSnapshot(), options.coalesceKey || null);
  mutate();
  if (options.saveLayout) saveWorkflowLayout();
  markDirty();
  if (options.render) renderEditor();
}

function restoreWorkflowSnapshot(snapshot) {
  if (!state.draft || state.draft.kind !== "Workflow") return;
  state.draft.spec = clone(snapshot.spec);
  state.canvasPositions = clone(snapshot.positions);
  state.dirty = snapshot.dirty;
  state.selectedSteps.clear();
  state.selectedEdges = [];
  state.selectedStep = null;
  state.selectedEdge = null;
  saveWorkflowLayout();
  closeInspector();
  renderEditor();
}

function undoWorkflowCommand() {
  const command = state.workflowHistory.undo.pop();
  if (!command) return;
  state.workflowHistory.coalesced = null;
  state.workflowHistory.redo.push({ label: command.label, snapshot: workflowSnapshot() });
  restoreWorkflowSnapshot(command.snapshot);
  setStatus(`Undid ${command.label}`, "info");
}

function redoWorkflowCommand() {
  const command = state.workflowHistory.redo.pop();
  if (!command) return;
  state.workflowHistory.coalesced = null;
  state.workflowHistory.undo.push({ label: command.label, snapshot: workflowSnapshot() });
  restoreWorkflowSnapshot(command.snapshot);
  setStatus(`Redid ${command.label}`, "info");
}

function confirmDiscard() {
  return !state.dirty || confirm("Discard unsaved changes?");
}

// ============================================================================
// Selecting & loading resources
// ============================================================================
async function selectResource(plural, name, ns, skipDirtyCheck = false) {
  if (!skipDirtyCheck && !confirmDiscard()) return;
  const revision = namespaceRevision;
  setStatus("loading…", "info");
  try {
    const obj = await apiGet(plural, name, ns);
    if (revision !== namespaceRevision) return;
    state.draft = obj;
    state.draftKey = keyOf(plural, name, ns);
    state.existsInCluster = true;
    state.dirty = false;
    state.selectedStep = null;
    state.selectedEdge = null;
    state.canvasPositions = obj.kind === "Workflow" ? loadWorkflowLayout(obj) : {};
    state.canvasZoom = 1;
    state.canvasViewport = { left: 0, top: 0 };
    resetWorkflowHistory();
    closeInspector();
    closeExplorer();
    renderEditor();
    refreshSidebarActive();
    setStatus("", "");
    // Run resources need continuous refresh (phase, active step, approvals/results).
    // Every other kind is user-edited and shouldn't be silently overwritten.
    stopViewerPolling();
    if (obj.kind === "WorkflowRun" || obj.kind === "EvaluationRun") startViewerPolling(plural, name, ns);
  } catch (e) {
    if (revision === namespaceRevision) setStatus(e.message, "error");
  }
}

function startViewerPolling(plural, name, ns) {
  const tick = async () => {
    // Bail if the user navigated away or started editing.
    if (!state.draft || state.draftKey !== keyOf(plural, name, ns)) { stopViewerPolling(); return; }
    if (state.dirty) return;
    try {
      const obj = await apiGet(plural, name, ns);
      if (!state.draft || state.draftKey !== keyOf(plural, name, ns)) { stopViewerPolling(); return; }
      const presentationChanged = runPresentationKey(state.draft) !== runPresentationKey(obj);
      const resourceChanged = JSON.stringify(state.draft) !== JSON.stringify(obj);
      state.draft = obj;
      if (state.view === "builder" && presentationChanged) renderEditor();
      else if (state.view === "yaml" && resourceChanged) {
        const yamlArea = document.getElementById("yaml-area");
        if (yamlArea && !yamlArea.matches(":focus")) yamlArea.value = toYaml(prune(clone(state.draft)) || state.draft);
      }
    } catch (_) { /* transient; keep polling */ }
  };
  state.viewerPoller = setInterval(tick, 1500);
}

function stopViewerPolling() {
  if (state.viewerPoller) { clearInterval(state.viewerPoller); state.viewerPoller = null; }
}

function newAgent() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "Agent",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      modelRef: { name: "" },
      instructions: { inline: "" },
      runtime: { class: "default" },
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newWorkflow() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "Workflow",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      steps: [],
      edges: [],
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  state.selectedStep = null;
  state.selectedEdge = null;
  state.canvasPositions = {};
  state.canvasZoom = 1;
  state.canvasViewport = { left: 0, top: 0 };
  resetWorkflowHistory();
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newTool() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "Tool",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      protocol: "MCP",
      className: "",
      endpoint: "",
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newToolClass() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "ToolClass",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      provisioner: "",
      executionTier: "process",
      transport: "http",
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newModelGrant() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "ModelGrant",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      agentRef: { name: "" },
      modelRef: { name: "" },
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newMemoryGrant() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "MemoryGrant",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      agentRef: { name: "" },
      memoryClaimRef: { name: "" },
      accessMode: "ReadOnly",
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newCrossNamespaceGrant() {
  if (!confirmDiscard()) return;
  const namespace = state.namespace || "default";
  state.draft = {
    apiVersion: API_VERSION,
    kind: "CrossNamespaceGrant",
    metadata: { name: "", namespace },
    spec: {
      from: namespace,
      to: "",
      resources: [{ kind: "", name: "" }],
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newToolGrant() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "ToolGrant",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      agentRef: { name: "" },
      toolRef: { name: "" },
      verbs: ["invoke"],
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newKnowledgeBase() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "KnowledgeBase",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      knowledgeBaseClassName: "",
      sources: [],
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newKnowledgeBaseClaim() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "KnowledgeBaseClaim",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      knowledgeBaseName: "",
      accessMode: "ReadOnly",
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function newEvaluationDataset() {
  if (!confirmDiscard()) return;
  state.draft = {
    apiVersion: API_VERSION,
    kind: "EvaluationDataset",
    metadata: { name: "", namespace: state.namespace || "default" },
    spec: {
      version: "v1",
      cases: [],
    },
  };
  state.draftKey = null;
  state.existsInCluster = false;
  state.dirty = true;
  closeInspector();
  switchView("builder");
  refreshSidebarActive();
}

function refreshSidebarActive() {
  document.querySelectorAll(".resource-list li").forEach((li) => li.classList.remove("active"));
  if (!state.draftKey) return;
  // Match by data attribute or rerender — simpler: rerender both agent + workflow lists
  renderList("agents");
  renderList("workflows");
  renderList("tools");
  renderList("toolclasses");
  renderList("evaluations");
  renderList("evaluationdatasets");
  renderList("evaluationruns");
  renderList("knowledgebases");
  renderList("knowledgebaseclaims");
  renderList("modelgrants");
  renderList("memorygrants");
  renderList("crossnamespacegrants");
  renderList("toolgrants");
}

function discardDraft() {
  if (!state.draft) return;
  if (!confirm("Discard changes?")) return;
  if (state.existsInCluster) {
    // Reload from cluster
    const [plural, ns, name] = state.draftKey.split("/");
    selectResource(plural, name, ns, true);
  } else {
    state.draft = null;
    state.draftKey = null;
    state.existsInCluster = false;
    state.dirty = false;
    renderEditor();
  }
}

// ============================================================================
// Editor shell
// ============================================================================
function renderEditor() {
  const titleEl = document.getElementById("editor-title");
  const kindPill = document.getElementById("editor-kind-pill");
  const btnDeploy = document.getElementById("btn-deploy");
  const btnDelete = document.getElementById("btn-delete");
  const btnDiscard = document.getElementById("btn-discard");
  const empty = document.getElementById("empty-state");
  const host = document.getElementById("builder-host");

  if (!state.draft) {
    titleEl.textContent = "Select or create a resource";
    kindPill.textContent = "—";
    btnDeploy.disabled = true;
    btnDeploy.title = "";
    btnDelete.disabled = true;
    btnDiscard.disabled = true;
    empty.style.display = "";
    host.innerHTML = "";
    document.getElementById("yaml-area").value = "";
    return;
  }
  empty.style.display = "none";
  kindPill.textContent = state.draft.kind;
  const name = state.draft.metadata.name || "(unnamed)";
  const ns = StudioResources.namespaced(state.draft.kind) ? state.draft.metadata.namespace || "default" : "cluster";
  titleEl.textContent = `${name}  ·  ${ns}${state.existsInCluster ? "" : "  (draft)"}${state.dirty && state.existsInCluster ? "  *" : ""}`;
  btnDeploy.disabled = !StudioResources.editable() || deployingDrafts.has(state.draft) || teamBlocked(state.draft);
  btnDeploy.title = teamBlocked(state.draft) ? TEAM_UNSUPPORTED_MESSAGE : "";
  btnDeploy.textContent = StudioResources.descriptor(state.draft.kind)?.treatment === "launch" ? "Launch" : "Deploy";
  btnDelete.disabled = !state.existsInCluster || !StudioResources.editable();
  btnDiscard.disabled = !state.dirty;
  document.getElementById("yaml-area").readOnly = !StudioResources.editable();
  document.getElementById("btn-apply-yaml").disabled = !StudioResources.editable();

  // Preview tab is only meaningful for kinds that have an "execute" surface.
  // Agents chat, Workflows run, Tools invoke ad-hoc, Evaluations create runs.
  const previewable = state.draft.kind === "Agent" || state.draft.kind === "Workflow" || state.draft.kind === "Tool" || state.draft.kind === "Evaluation";
  const previewTab = document.getElementById("tab-preview");
  if (previewTab) previewTab.hidden = !previewable;
  if (!previewable && state.view === "preview") {
    switchView("builder");
    return;
  }

  if (state.view === "builder") {
    host.innerHTML = "";
    if (StudioResources.render(host)) { StudioTutorial.onRender(); return; }
    if (state.draft.kind === "Agent") {
      renderAgentBuilder(host);
    } else if (state.draft.kind === "Workflow") {
      renderWorkflowBuilder(host);
    } else if (state.draft.kind === "WorkflowRun") {
      renderWorkflowRunViewer(host);
    } else if (state.draft.kind === "EvaluationRun") {
      renderEvaluationRunViewer(host);
    } else if (state.draft.kind === "Evaluation") {
      renderEvaluationBuilder(host);
    } else if (state.draft.kind === "EvaluationDataset") {
      renderEvaluationDatasetBuilder(host);
    } else if (state.draft.kind === "Tool") {
      renderToolBuilder(host);
    } else if (state.draft.kind === "ToolClass") {
      renderToolClassBuilder(host);
    } else if (state.draft.kind === "ModelGrant") {
      renderModelGrantBuilder(host);
    } else if (state.draft.kind === "MemoryGrant") {
      renderMemoryGrantBuilder(host);
    } else if (state.draft.kind === "CrossNamespaceGrant") {
      renderCrossNamespaceGrantBuilder(host);
    } else if (state.draft.kind === "ToolGrant") {
      renderToolGrantBuilder(host);
    } else if (state.draft.kind === "KnowledgeBase") {
      renderKnowledgeBaseBuilder(host);
    } else if (state.draft.kind === "KnowledgeBaseClaim") {
      renderKnowledgeBaseClaimBuilder(host);
    } else {
      host.innerHTML = `<div class="banner info">No visual builder for kind <strong>${escapeHtml(state.draft.kind)}</strong> yet. Use the YAML tab to edit.</div>`;
    }
  } else if (state.view === "yaml") {
    document.getElementById("yaml-area").value = toYaml(clone(state.draft));
  } else if (state.view === "preview") {
    renderPreview(document.getElementById("preview-host"));
  }
  StudioTutorial.onRender();
}

// ============================================================================
// ModelGrant builder
// ============================================================================
function renderModelGrantBuilder(host) {
  const grant = state.draft;
  grant.metadata = grant.metadata || {};
  grant.spec = grant.spec || {};
  const metadata = grant.metadata;
  const spec = grant.spec;

  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", metadata.name || "", (value) => { metadata.name = value; markDirty(); updateTitle(); }, true, "model-access"),
      textField("Namespace", metadata.namespace || "default", (value) => { metadata.namespace = value; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", metadata.labels || {}, (value) => { setOrDel(metadata, "labels", value); markDirty(); }));
    body.appendChild(kvField("Annotations", metadata.annotations || {}, (value) => { setOrDel(metadata, "annotations", value); markDirty(); }));
  }, true));

  host.appendChild(section("Grant", "Authorize one agent to use one model.", (body) => {
    body.appendChild(fieldGrid([
      refSelect("Agent", spec.agentRef?.name || "", state.lists.agents || [], (value) => { spec.agentRef = { name: value }; markDirty(); }, true),
      refSelect("Model", spec.modelRef?.name || "", state.lists.models || [], (value) => { spec.modelRef = { name: value }; markDirty(); }, true),
    ]));
  }, true));
}

// ============================================================================
// MemoryGrant builder
// ============================================================================
function renderMemoryGrantBuilder(host) {
  const grant = state.draft;
  grant.metadata = grant.metadata || {};
  grant.spec = grant.spec || {};
  const metadata = grant.metadata;
  const spec = grant.spec;

  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", metadata.name || "", (value) => { metadata.name = value; markDirty(); updateTitle(); }, true, "memory-access"),
      textField("Namespace", metadata.namespace || "default", (value) => { metadata.namespace = value; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", metadata.labels || {}, (value) => { setOrDel(metadata, "labels", value); markDirty(); }));
    body.appendChild(kvField("Annotations", metadata.annotations || {}, (value) => { setOrDel(metadata, "annotations", value); markDirty(); }));
  }, true));

  host.appendChild(section("Grant", "Authorize one agent to access one memory claim.", (body) => {
    body.appendChild(fieldGrid([
      refSelect("Agent", spec.agentRef?.name || "", state.lists.agents || [], (value) => { spec.agentRef = { name: value }; markDirty(); }, true),
      refSelect("Memory claim", spec.memoryClaimRef?.name || "", state.lists.memoryclaims || [], (value) => { spec.memoryClaimRef = { name: value }; markDirty(); }, true),
      selectField("Access mode", spec.accessMode || "ReadOnly", ["ReadOnly", "ReadWrite"], (value) => { spec.accessMode = value; markDirty(); }, true),
    ]));
  }, true));
}

// ============================================================================
// CrossNamespaceGrant builder
// ============================================================================
function renderCrossNamespaceGrantBuilder(host) {
  const grant = state.draft;
  grant.metadata = grant.metadata || {};
  grant.spec = grant.spec || {};
  const metadata = grant.metadata;
  const spec = grant.spec;

  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", metadata.name || "", (value) => { metadata.name = value; markDirty(); updateTitle(); }, true, "share-platform-resources"),
      textField("Namespace", metadata.namespace || "default", (value) => { metadata.namespace = value; markDirty(); updateTitle(); }, true),
    ]));
    body.appendChild(kvField("Labels", metadata.labels || {}, (value) => { setOrDel(metadata, "labels", value); markDirty(); }));
    body.appendChild(kvField("Annotations", metadata.annotations || {}, (value) => { setOrDel(metadata, "annotations", value); markDirty(); }));
  }, true));

  host.appendChild(section("Grant", "Share selected resources from one namespace with another namespace.", (body) => {
    body.appendChild(fieldGrid([
      textField("From namespace", spec.from || "", (value) => { spec.from = value; markDirty(); }, true, "platform"),
      textField("To namespace", spec.to || "", (value) => { spec.to = value; markDirty(); }, true, "team-a or *"),
    ]));
    body.appendChild(grantResourcesField(spec.resources || [], (value) => { spec.resources = value; markDirty(); }));
  }, true));
}

function grantResourcesField(resources, onChange) {
  const items = Array.isArray(resources) ? resources.map((resource) => ({ ...resource })) : [];
  const wrap = document.createElement("div");
  wrap.className = "kv-list";

  function refresh() {
    wrap.innerHTML = "";
    items.forEach((resource, index) => {
      const row = document.createElement("div");
      row.className = "kv-row";
      const kind = document.createElement("input");
      kind.type = "text";
      kind.value = resource.kind || "";
      kind.placeholder = "Kind, e.g. Tool";
      const name = document.createElement("input");
      name.type = "text";
      name.value = resource.name || "";
      name.placeholder = "Name or *";
      const remove = document.createElement("button");
      remove.type = "button";
      remove.textContent = "\u2715";
      kind.addEventListener("input", () => { resource.kind = kind.value; onChange(items.map((item) => ({ ...item }))); });
      name.addEventListener("input", () => { resource.name = name.value; onChange(items.map((item) => ({ ...item }))); });
      remove.addEventListener("click", () => { items.splice(index, 1); onChange(items.map((item) => ({ ...item }))); refresh(); });
      row.appendChild(kind);
      row.appendChild(name);
      row.appendChild(remove);
      wrap.appendChild(row);
    });
    const add = document.createElement("button");
    add.type = "button";
    add.className = "add-row";
    add.textContent = "+ Add resource";
    add.addEventListener("click", () => { items.push({ kind: "", name: "" }); onChange(items.map((item) => ({ ...item }))); refresh(); });
    wrap.appendChild(add);
  }

  refresh();
  return field("Resources", wrap, { required: true });
}

// ============================================================================
// ToolGrant builder
// ============================================================================
function renderToolGrantBuilder(host) {
  const grant = state.draft;
  grant.metadata = grant.metadata || {};
  grant.spec = grant.spec || {};
  const metadata = grant.metadata;
  const spec = grant.spec;
  const targeting = spec.agentSelector !== undefined ? "selector" : "reference";

  function updateOptionalObject(key, fieldName, value) {
    const next = { ...(spec[key] || {}) };
    setOrDel(next, fieldName, value);
    setOrDel(spec, key, next);
    markDirty();
  }

  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", metadata.name || "", (value) => { metadata.name = value; markDirty(); updateTitle(); }, true, "researcher-search"),
      textField("Namespace", metadata.namespace || "default", (value) => { metadata.namespace = value; markDirty(); updateTitle(); }, true),
    ]));
    body.appendChild(kvField("Labels", metadata.labels || {}, (value) => { setOrDel(metadata, "labels", value); markDirty(); }));
    body.appendChild(kvField("Annotations", metadata.annotations || {}, (value) => { setOrDel(metadata, "annotations", value); markDirty(); }));
  }, true));

  host.appendChild(section("Subject", "Choose one agent or select agents by labels.", (body) => {
    body.appendChild(selectField("Agent targeting", targeting, [
      { value: "reference", label: "Agent reference" },
      { value: "selector", label: "Agent selector" },
    ], (value) => {
      if (value === "selector") {
        delete spec.agentRef;
        spec.agentSelector = {};
      } else {
        delete spec.agentSelector;
        spec.agentRef = { name: "" };
      }
      markDirty();
      renderEditor();
    }, true));
    if (targeting === "selector") {
      body.appendChild(kvField("Agent selector", spec.agentSelector || {}, (value) => { spec.agentSelector = value; markDirty(); }));
    } else {
      body.appendChild(refSelect("Agent", spec.agentRef?.name || "", state.lists.agents || [], (value) => { spec.agentRef = { name: value }; markDirty(); }, false));
    }
  }, true));

  host.appendChild(section("Tool access", "Select the tool and allowed operations.", (body) => {
    body.appendChild(refSelect("Tool", spec.toolRef?.name || "", state.lists.tools || [], (value) => { spec.toolRef = { name: value }; markDirty(); }, true));
    body.appendChild(toolGrantVerbsField(spec.verbs || [], (value) => { spec.verbs = value; markDirty(); }));
  }, true));

  host.appendChild(section("Argument constraints", "Restrict invocation arguments with a JSON Schema object.", (body) => {
    body.appendChild(jsonMapField("Argument constraints", spec.argumentConstraints, (value) => { setOrDel(spec, "argumentConstraints", value); markDirty(); }, '{ "type": "object", "properties": {} }'));
  }));

  host.appendChild(section("Rate limits", "Optional call caps for this grant.", (body) => {
    body.appendChild(fieldGrid([
      numberField("Calls per minute", spec.rateLimits?.callsPerMinute, (value) => updateOptionalObject("rateLimits", "callsPerMinute", value)),
      numberField("Calls per hour", spec.rateLimits?.callsPerHour, (value) => updateOptionalObject("rateLimits", "callsPerHour", value)),
      numberField("Calls per day", spec.rateLimits?.callsPerDay, (value) => updateOptionalObject("rateLimits", "callsPerDay", value)),
    ]));
  }));

  host.appendChild(section("Conditions", "Optional RFC 3339 validity window and required agent labels.", (body) => {
    body.appendChild(fieldGrid([
      textField("Valid from", spec.conditions?.validFrom || "", (value) => updateOptionalObject("conditions", "validFrom", value), false, "2026-01-01T00:00:00Z"),
      textField("Valid until", spec.conditions?.validUntil || "", (value) => updateOptionalObject("conditions", "validUntil", value), false, "2026-12-31T23:59:59Z"),
    ]));
    body.appendChild(kvField("Required agent labels", spec.conditions?.requiredAgentLabels || {}, (value) => updateOptionalObject("conditions", "requiredAgentLabels", value)));
  }));
}

function toolGrantVerbsField(verbs, onChange) {
  const selected = new Set(Array.isArray(verbs) ? verbs : []);
  const wrap = document.createElement("div");
  wrap.className = "chip-list";
  for (const verb of ["invoke", "discover", "inspect"]) {
    const label = document.createElement("label");
    label.className = "chip";
    const input = document.createElement("input");
    input.type = "checkbox";
    input.checked = selected.has(verb);
    input.addEventListener("change", () => {
      if (input.checked) selected.add(verb);
      else selected.delete(verb);
      onChange(["invoke", "discover", "inspect"].filter((candidate) => selected.has(candidate)));
    });
    label.appendChild(input);
    label.appendChild(document.createTextNode(verb));
    wrap.appendChild(label);
  }
  return field("Verbs", wrap, { required: true });
}

function markDirty() {
  StudioTutorial.changed();
  state.dirty = true;
  document.getElementById("btn-discard").disabled = false;
  const titleEl = document.getElementById("editor-title");
  if (titleEl && !titleEl.textContent.endsWith("*")) {
    if (state.existsInCluster) titleEl.textContent += "  *";
  }
  refreshWorkflowAssistant();
}

function switchView(view) {
  state.view = view;
  StudioTutorial.viewChanged(view);
  document.querySelectorAll(".editor-bar-tabs .tab").forEach((b) => {
    const active = b.dataset.view === view;
    b.classList.toggle("active", active);
    b.setAttribute("aria-selected", String(active));
  });
  document.getElementById("view-builder").classList.toggle("active", view === "builder");
  document.getElementById("view-yaml").classList.toggle("active", view === "yaml");
  document.getElementById("view-preview").classList.toggle("active", view === "preview");
  renderEditor();
}

async function applyYAML() {
  const text = document.getElementById("yaml-area").value;
  try {
    const obj = await apiParseYAML(text);
    if (!obj || typeof obj !== "object" || Array.isArray(obj)) throw new Error("YAML must be a single mapping");
    if (!obj.kind) throw new Error("manifest missing 'kind'");
    if (!obj.metadata || !obj.metadata.name) throw new Error("manifest missing metadata.name");
    state.draft = obj;
    if (state.draft.kind === "Workflow") {
      state.canvasPositions = loadWorkflowLayout(state.draft);
      state.canvasZoom = 1;
      state.canvasViewport = { left: 0, top: 0 };
      resetWorkflowHistory();
      state.selectedStep = null;
      state.selectedEdge = null;
    }
    markDirty();
    switchView("builder");
    setStatus("YAML applied to builder", "info");
  } catch (e) {
    setStatus(`apply yaml: ${e.message}`, "error");
  }
}

// ============================================================================
// Deploy / Delete
// ============================================================================
const deployingDrafts = new WeakSet();

function hasWorkflowTeam(draft) {
  return draft?.kind === "Workflow" && Array.isArray(draft.spec?.steps)
    && draft.spec.steps.some((step) => step?.type === "collaboration" || step?.collaboration != null);
}

const TEAM_UNSUPPORTED_MESSAGE = "Team steps run only on the Kubernetes runtime";

// Team steps execute through the Kubernetes controller; other Studio modes can only draft them.
function teamBlocked(draft) {
  return hasWorkflowTeam(draft) && state.mode !== "kubernetes";
}

async function deploy() {
  const draft = state.draft;
  if (!draft || deployingDrafts.has(draft)) return;
  if (teamBlocked(draft)) {
    setStatus(TEAM_UNSUPPORTED_MESSAGE, "error");
    return;
  }
  if (!StudioResources.validate()) return;
  const submittedDraft = JSON.stringify(draft);
  const body = JSON.parse(submittedDraft);
  if (!body || !body.metadata || !body.metadata.name) {
    setStatus("metadata.name is required", "error");
    return;
  }
  const plural = KIND_PLURAL[state.draft.kind.toLowerCase()] || (state.draft.kind.toLowerCase() + "s");
  const ns = StudioResources.namespaced(state.draft.kind) ? body.metadata.namespace || "default" : "";
  if (ns) body.metadata.namespace = ns;
  else delete body.metadata.namespace;
  body.apiVersion = body.apiVersion || API_VERSION;
  body.kind = state.draft.kind;
  // A rename (metadata.name change) or namespace change against a cluster
  // object means we're creating a new resource — Kubernetes names are
  // immutable, so we cannot PUT to the new name with the old UID. Detect this
  // by comparing the current body against the key the draft was loaded under.
  let treatAsUpdate = state.existsInCluster;
  if (treatAsUpdate && state.draftKey) {
    const [origPlural, origNs, origName] = state.draftKey.split("/");
    if (origPlural !== plural || origNs !== ns || origName !== body.metadata.name) {
      treatAsUpdate = false;
    }
  }
  if (!treatAsUpdate && body.metadata) {
    // Drop server-managed fields so the apiserver doesn't reject the create.
    delete body.metadata.uid;
    delete body.metadata.resourceVersion;
    delete body.metadata.generation;
    delete body.metadata.creationTimestamp;
    delete body.metadata.deletionTimestamp;
    delete body.metadata.deletionGracePeriodSeconds;
    delete body.metadata.selfLink;
    delete body.metadata.managedFields;
    delete body.metadata.ownerReferences;
    delete body.metadata.finalizers;
    delete body.status;
  }
  if (StudioResources.executionChange()
      && !confirm(`Apply ${body.kind} ${body.metadata.name}? This may start execution, indexing or recurring work and use paid providers.`)) return;
  // resourceVersion is required for PUT; backend auto-fills if missing.
  deployingDrafts.add(draft);
  document.getElementById("btn-deploy").disabled = true;
  setStatus("deploying…", "info");
  try {
    let result;
    if (treatAsUpdate) {
      result = await apiUpdate(plural, body.metadata.name, body, ns);
    } else {
      result = await apiCreate(plural, body, ns);
    }
    if (state.draft === draft) {
      const yamlArea = document.getElementById("yaml-area");
      const pendingYAML = state.view === "yaml" && yamlArea.value !== toYaml(clone(draft)) ? yamlArea.value : null;
      state.draftKey = keyOf(plural, result.metadata.name, result.metadata.namespace);
      state.existsInCluster = true;
      if (JSON.stringify(draft) === submittedDraft) {
        state.draft = result;
        state.dirty = false;
      } else {
        draft.metadata.resourceVersion = result.metadata.resourceVersion;
        draft.metadata.uid = result.metadata.uid;
        state.dirty = true;
      }
      deployingDrafts.delete(draft);
      renderEditor();
      if (pendingYAML !== null) yamlArea.value = pendingYAML;
      setStatus(`deployed ${result.kind} ${result.metadata.name}${state.dirty || pendingYAML !== null ? " (newer edits not deployed)" : ""}`, "success");
    }
    await refreshOne(plural);
    refreshSidebarActive();
  } catch (e) {
    if (state.draft === draft) setStatus(`deploy failed: ${e.message}`, "error");
  } finally {
    deployingDrafts.delete(draft);
    if (state.draft === draft) document.getElementById("btn-deploy").disabled = !StudioResources.editable() || teamBlocked(draft);
  }
}

async function deleteResource() {
  if (!state.draft || !state.existsInCluster) return;
  if (!StudioResources.editable()) { setStatus("This resource is read-only.", "error"); return; }
  const name = state.draft.metadata.name;
  if (!confirm(`Delete ${state.draft.kind} ${name}? This cannot be undone.`)) return;
  const plural = KIND_PLURAL[state.draft.kind.toLowerCase()];
  try {
    await apiDelete(plural, name, state.draft.metadata.namespace);
    setStatus(`deleted ${name}`, "success");
    state.draft = null;
    state.draftKey = null;
    state.existsInCluster = false;
    state.dirty = false;
    await refreshOne(plural);
    renderEditor();
  } catch (e) {
    setStatus(`delete failed: ${e.message}`, "error");
  }
}

// ============================================================================
// Evaluation builder
// ============================================================================
function renderEvaluationBuilder(host) {
  const e = state.draft;
  e.metadata = e.metadata || {};
  e.spec = e.spec || {};

  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", e.metadata.name || "", (v) => { e.metadata.name = v; markDirty(); updateTitle(); }, true),
      textField("Namespace", e.metadata.namespace || "default", (v) => { e.metadata.namespace = v || "default"; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", e.metadata.labels || {}, (m) => { e.metadata.labels = m; markDirty(); }));
    body.appendChild(kvField("Annotations", e.metadata.annotations || {}, (m) => { e.metadata.annotations = m; markDirty(); }));
  }, true));

  host.appendChild(section("Target", "Resource that this evaluation invokes and scores.", (body) => {
    e.spec.targetRef = e.spec.targetRef || { kind: "Agent", name: "" };
    body.appendChild(fieldGrid([
      selectField("Kind", e.spec.targetRef.kind || "Agent", ["Agent", "Solution"], (v) => {
        e.spec.targetRef.kind = v || "Agent";
        markDirty();
        renderEditor();
      }, true),
      e.spec.targetRef.kind === "Agent"
        ? refSelect("Name", e.spec.targetRef.name || "", state.lists.agents || [], (v) => { e.spec.targetRef.name = v; markDirty(); }, true)
        : textField("Name", e.spec.targetRef.name || "", (v) => { e.spec.targetRef.name = v; markDirty(); }, true),
      textField("Namespace", e.spec.targetRef.namespace || "", (v) => { setOrDel(e.spec.targetRef, "namespace", v); markDirty(); }, false, e.metadata.namespace || "default"),
      numberField("Revision", e.spec.targetRef.revision, (v) => { setOrDel(e.spec.targetRef, "revision", v); markDirty(); }, "Optional target revision pin."),
    ]));
  }, true));

  host.appendChild(section("Dataset", "Input dataset identity used by each EvaluationRun.", (body) => {
    e.spec.datasetRef = e.spec.datasetRef || { name: "" };
    body.appendChild(fieldGrid([
      textField("Name", e.spec.datasetRef.name || "", (v) => { e.spec.datasetRef.name = v; markDirty(); }, true),
      textField("Version", e.spec.datasetRef.version || "", (v) => { setOrDel(e.spec.datasetRef, "version", v); markDirty(); }, false, "v1"),
      textField("Namespace", e.spec.datasetRef.namespace || "", (v) => { setOrDel(e.spec.datasetRef, "namespace", v); markDirty(); }, false, e.metadata.namespace || "default"),
    ]));
  }, true));

  host.appendChild(section("Graders", "Scoring functions and their parameters.", (body) => {
    body.appendChild(evaluationGradersEditor(e.spec.graders || [], (items) => {
      e.spec.graders = items.length ? items : undefined;
      markDirty();
      renderEditor();
    }));
  }, true));

  host.appendChild(section("Metric thresholds", "Pass/fail bounds applied to emitted metrics.", (body) => {
    body.appendChild(evaluationThresholdsEditor(e.spec.metricThresholds || [], (items) => {
      e.spec.metricThresholds = items.length ? items : undefined;
      markDirty();
      renderEditor();
    }));
  }, true));

  host.appendChild(section("Schedule", "Optional periodic execution.", (body) => {
    const schedule = e.spec.schedule || {};
    body.appendChild(fieldGrid([
      textField("Cron", schedule.cron || "", (v) => { evaluationSetScheduleField(e, "cron", v); markDirty(); }, false, "0 9 * * *"),
      textField("Timezone", schedule.timezone || "", (v) => { evaluationSetScheduleField(e, "timezone", v); markDirty(); }, false, "UTC"),
      selectField("Concurrency", schedule.concurrencyPolicy || "", [
        { value: "", label: "default" },
        "Allow",
        "Forbid",
        "Replace",
      ], (v) => { evaluationSetScheduleField(e, "concurrencyPolicy", v); markDirty(); }),
      numberField("Successful history", schedule.successfulRunHistoryLimit, (v) => { evaluationSetScheduleField(e, "successfulRunHistoryLimit", v); markDirty(); }),
      numberField("Failed history", schedule.failedRunHistoryLimit, (v) => { evaluationSetScheduleField(e, "failedRunHistoryLimit", v); markDirty(); }),
    ]));
    body.appendChild(checkboxField("Suspend schedule", schedule.suspend || false, (v) => { evaluationSetScheduleField(e, "suspend", v); markDirty(); }));
  }));

  host.appendChild(section("Run", "Create an EvaluationRun from the current evaluation config.", (body) => {
    renderEvaluationRunner(body, () => renderEditor());
  }, true));
}

function renderEvaluationDatasetBuilder(host) {
  const dataset = state.draft;
  dataset.metadata = dataset.metadata || {};
  dataset.spec = dataset.spec || {};

  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", dataset.metadata.name || "", (v) => { dataset.metadata.name = v; markDirty(); updateTitle(); }, true),
      textField("Namespace", dataset.metadata.namespace || "default", (v) => { dataset.metadata.namespace = v || "default"; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", dataset.metadata.labels || {}, (m) => { dataset.metadata.labels = m; markDirty(); }));
    body.appendChild(kvField("Annotations", dataset.metadata.annotations || {}, (m) => { dataset.metadata.annotations = m; markDirty(); }));
  }, true));

  host.appendChild(section("Dataset", "Versioned cases used by EvaluationRuns.", (body) => {
    body.appendChild(fieldGrid([
      textField("Version", dataset.spec.version || "", (v) => { dataset.spec.version = v || undefined; markDirty(); }, true, "v1"),
    ]));
    const updateCases = (items) => {
      dataset.spec.cases = items.length ? items : undefined;
      markDirty();
    };
    body.appendChild(evaluationDatasetCasesEditor(dataset.spec.cases || [], updateCases, () => renderEditor()));
  }, true));
}

function evaluationDatasetCasesEditor(items, onChange, onStructureChange) {
  const wrap = document.createElement("div");
  wrap.className = "repeatable-list";
  const cases = clone(items || []);
  if (!cases.length) {
    const empty = document.createElement("div"); empty.className = "hint"; empty.textContent = "No cases yet. Add one input/expected-output pair to start scoring this dataset.";
    wrap.appendChild(empty);
  }
  cases.forEach((testCase, index) => {
    const card = document.createElement("div"); card.className = "repeatable-card";
    card.appendChild(fieldGrid([
      textField("ID", testCase.id || "", (v) => { testCase.id = v; onChange(cases); }, true, `case-${index + 1}`),
      textField("Expected", testCase.expected || "", (v) => { setOrDel(testCase, "expected", v); onChange(cases); }, false, "hello"),
    ]));
    card.appendChild(textareaField("Input", testCase.input || "", (v) => { testCase.input = v; onChange(cases); }, "Prompt or task sent to the target."));
    card.appendChild(textareaField("Reference", testCase.reference || "", (v) => { setOrDel(testCase, "reference", v); onChange(cases); }, "Optional reference answer."));
    card.appendChild(textareaField("Rubric", testCase.rubric || "", (v) => { setOrDel(testCase, "rubric", v); onChange(cases); }, "Optional case-specific grading rubric."));
    card.appendChild(textField("Tags", (testCase.tags || []).join(", "), (v) => {
      setOrDel(testCase, "tags", splitCSV(v));
      onChange(cases);
    }, false, "smoke, greeting"));
    card.appendChild(kvField("Metadata", testCase.metadata || {}, (m) => { setOrDel(testCase, "metadata", m); onChange(cases); }));
    const actions = document.createElement("div"); actions.className = "row-actions";
    const remove = document.createElement("button"); remove.className = "action danger"; remove.textContent = "Remove case";
    remove.addEventListener("click", () => { cases.splice(index, 1); onChange(cases); onStructureChange(); });
    actions.appendChild(remove); card.appendChild(actions); wrap.appendChild(card);
  });
  const add = document.createElement("button"); add.className = "action"; add.textContent = "Add case";
  add.addEventListener("click", () => { cases.push({ id: `case-${cases.length + 1}`, input: "" }); onChange(cases); onStructureChange(); });
  wrap.appendChild(add);
  return wrap;
}

function splitCSV(value) {
  return (value || "").split(",").map((item) => item.trim()).filter(Boolean);
}

function evaluationGradersEditor(items, onChange) {
  const wrap = document.createElement("div");
  wrap.className = "repeatable-list";
  const graders = clone(items || []);
  if (!graders.length) {
    const empty = document.createElement("div"); empty.className = "hint"; empty.textContent = "No explicit graders. The controller will use its default built-ins.";
    wrap.appendChild(empty);
  }
  graders.forEach((grader, index) => {
    const card = document.createElement("div"); card.className = "repeatable-card";
    card.appendChild(fieldGrid([
      textField("Name", grader.name || "", (v) => { grader.name = v; onChange(graders); }, true),
      selectField("Type", grader.type || "task_success", ["builtin", "task_success", "hallucination", "safety", "latency", "cost", "tool_reliability", "memory_quality", "llm_judge"], (v) => { grader.type = v; onChange(graders); }, true),
    ]));
    card.appendChild(kvField("Parameters", grader.parameters || {}, (m) => { grader.parameters = Object.keys(m).length ? m : undefined; onChange(graders); }));
    const actions = document.createElement("div"); actions.className = "row-actions";
    const remove = document.createElement("button"); remove.className = "action danger"; remove.textContent = "Remove grader";
    remove.addEventListener("click", () => { graders.splice(index, 1); onChange(graders); });
    actions.appendChild(remove); card.appendChild(actions); wrap.appendChild(card);
  });
  const add = document.createElement("button"); add.className = "action"; add.textContent = "Add grader";
  add.addEventListener("click", () => { graders.push({ name: `grader-${graders.length + 1}`, type: "task_success" }); onChange(graders); });
  wrap.appendChild(add);
  return wrap;
}

function evaluationThresholdsEditor(items, onChange) {
  const wrap = document.createElement("div");
  wrap.className = "repeatable-list";
  const thresholds = clone(items || []);
  if (!thresholds.length) {
    const empty = document.createElement("div"); empty.className = "hint"; empty.textContent = "No thresholds. Metrics are recorded without changing pass/fail state.";
    wrap.appendChild(empty);
  }
  thresholds.forEach((threshold, index) => {
    const card = document.createElement("div"); card.className = "repeatable-card";
    card.appendChild(fieldGrid([
      textField("Metric", threshold.metric || "", (v) => { threshold.metric = v; onChange(thresholds); }, true),
      numberField("Min", threshold.min, (v) => { setOrDel(threshold, "min", v); onChange(thresholds); }),
      numberField("Max", threshold.max, (v) => { setOrDel(threshold, "max", v); onChange(thresholds); }),
    ]));
    card.appendChild(checkboxField("Required", threshold.required || false, (v) => { threshold.required = v; onChange(thresholds); }));
    const actions = document.createElement("div"); actions.className = "row-actions";
    const remove = document.createElement("button"); remove.className = "action danger"; remove.textContent = "Remove threshold";
    remove.addEventListener("click", () => { thresholds.splice(index, 1); onChange(thresholds); });
    actions.appendChild(remove); card.appendChild(actions); wrap.appendChild(card);
  });
  const add = document.createElement("button"); add.className = "action"; add.textContent = "Add threshold";
  add.addEventListener("click", () => { thresholds.push({ metric: "success", min: 1, required: true }); onChange(thresholds); });
  wrap.appendChild(add);
  return wrap;
}

function evaluationSetScheduleField(evaluation, key, value) {
  evaluation.spec = evaluation.spec || {};
  const schedule = evaluation.spec.schedule || {};
  if (value === "" || value === undefined || value === null || value === false) delete schedule[key];
  else schedule[key] = value;
  evaluation.spec.schedule = Object.keys(schedule).length ? schedule : undefined;
}

// ============================================================================
// Agent builder
// ============================================================================
function renderAgentBuilder(host) {
  const a = state.draft;
  a.spec = a.spec || {};
  a.metadata = a.metadata || {};

  // ---- Identity ----
  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", a.metadata.name || "", (v) => { a.metadata.name = v; markDirty(); updateTitle(); }, true),
      textField("Namespace", a.metadata.namespace || "default", (v) => { a.metadata.namespace = v; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", a.metadata.labels || {}, (m) => { a.metadata.labels = m; markDirty(); }));
    body.appendChild(kvField("Annotations", a.metadata.annotations || {}, (m) => { a.metadata.annotations = m; markDirty(); }));
  }, true));

  // ---- Model ----
  host.appendChild(section("Model", "Which model this agent uses for inference.", (body) => {
    body.appendChild(refSelect("Model", a.spec.modelRef?.name || "", state.lists.models || [], (v) => {
      a.spec.modelRef = v ? { name: v } : undefined;
      markDirty();
    }, true));
  }, true));

  // ---- Instructions ----
  host.appendChild(section("Instructions", "Either inline text or a reference to a Prompt resource. Required.", (body) => {
    body.dataset.tutorialTarget = "instructions";
    a.spec.instructions = a.spec.instructions || { inline: "" };
    const useRef = !!a.spec.instructions.ref;
    const toggle = document.createElement("div");
    toggle.className = "field-row";
    const inlineBtn = document.createElement("button");
    inlineBtn.className = "action" + (useRef ? "" : " primary");
    inlineBtn.textContent = "Inline";
    inlineBtn.addEventListener("click", () => {
      a.spec.instructions = { inline: a.spec.instructions.inline || "" };
      markDirty(); renderEditor();
    });
    const refBtn = document.createElement("button");
    refBtn.className = "action" + (useRef ? " primary" : "");
    refBtn.textContent = "Reference Prompt";
    refBtn.addEventListener("click", () => {
      a.spec.instructions = { ref: a.spec.instructions.ref || { name: "" } };
      markDirty(); renderEditor();
    });
    toggle.appendChild(inlineBtn); toggle.appendChild(refBtn);
    body.appendChild(toggle);

    if (useRef) {
      body.appendChild(refSelect("Prompt", a.spec.instructions.ref?.name || "", state.lists.prompts || [], (v) => {
        a.spec.instructions.ref = v ? { name: v } : { name: "" };
        markDirty();
      }, true));
    } else {
      body.appendChild(textareaField("Inline instructions", a.spec.instructions.inline || "", (v) => {
        a.spec.instructions.inline = v; markDirty();
      }, "You are a helpful assistant.…"));
    }
  }, true));

  // ---- Tools ----
  host.appendChild(section("Tools", "Tools available to the agent. Required tools must be granted at runtime.", (body) => {
    body.appendChild(toolPicker(a.spec.tools || [], state.lists.tools || [], (newList) => {
      a.spec.tools = newList; markDirty();
    }));
  }));

  // ---- Memory ----
  host.appendChild(section("Memory", "MemoryClaims this agent may attach to.", (body) => {
    const claims = (a.spec.memory && a.spec.memory.claims) || [];
    body.appendChild(refMultiSelect("Memory Claims", claims.map((c) => c.name), state.lists.memoryclaims || [], (names) => {
      a.spec.memory = names.length ? { claims: names.map((n) => ({ name: n })) } : undefined;
      markDirty();
    }));
  }));

  // ---- Runtime ----
  host.appendChild(section("Runtime", "Execution settings.", (body) => {
    a.spec.runtime = a.spec.runtime || {};
    body.appendChild(fieldGrid([
      textField("Class", a.spec.runtime.class || "default", (v) => { a.spec.runtime.class = v || "default"; markDirty(); }),
      textField("Timeout", a.spec.runtime.timeout || "", (v) => { a.spec.runtime.timeout = v || undefined; markDirty(); }, false, "60s"),
      numberField("MaxTokens", a.spec.runtime.maxTokens, (v) => { a.spec.runtime.maxTokens = v; markDirty(); }),
    ]));
  }));

  // ---- Companions ----
  host.appendChild(section("Companions", "Other agents that may be invoked from this agent.", (body) => {
    const c = a.spec.companions || [];
    body.appendChild(refMultiSelect("Companion agents", c.map((x) => x.name), state.lists.agents || [], (names) => {
      a.spec.companions = names.length ? names.map((n) => ({ name: n })) : undefined;
      markDirty();
    }));
  }, false, true));

  // ---- Knowledge Base Claims ----
  host.appendChild(section("Knowledge Base Claims", "KnowledgeBaseClaim references binding this agent to shared KnowledgeBases in the same namespace.", (body) => {
    const claims = a.spec.knowledgeBaseClaims || [];
    body.appendChild(kbClaimPicker(claims, state.lists.knowledgebaseclaims || [], (newList) => {
      a.spec.knowledgeBaseClaims = newList.length ? newList : undefined; markDirty();
    }));
    if (!(state.lists.knowledgebaseclaims || []).length) {
      body.appendChild(previewBanner("info", "No <strong>KnowledgeBaseClaim</strong> found in the cluster. Create one from <strong>+ KB Claim</strong> and point it at an existing KnowledgeBase before wiring it here."));
    }
  }, false, true));

  // ---- Autonomy ----
  host.appendChild(section("Autonomy", "Autonomy class that governs this agent.", (body) => {
    body.appendChild(refSelect("Autonomy Class", a.spec.autonomyClassRef?.name || "", state.lists.autonomyclasses || [], (v) => {
      a.spec.autonomyClassRef = v ? { name: v } : undefined; markDirty();
    }));
  }, false, true));

  // ---- Scaling ----
  host.appendChild(section("Scaling", "Replicas served by the AgentService.", (body) => {
    body.appendChild(numberField("Replicas", a.spec.replicas, (v) => { a.spec.replicas = v; markDirty(); }, "default = 1"));
  }, false, true));

  // ---- Probes (advanced) ----
  host.appendChild(section("Probes", "Liveness and readiness probes — see api/v1alpha1/probe.go.", (body) => {
    a.spec.probes = a.spec.probes || {};
    body.appendChild(textareaField("Probes JSON (advanced)", JSON.stringify(a.spec.probes, null, 2), (v) => {
      try { a.spec.probes = v.trim() ? JSON.parse(v) : undefined; markDirty(); }
      catch (_) { /* swallow, user is still typing */ }
    }, "{}"));
  }, false, true));

  // ---- Restart ladder (advanced) ----
  host.appendChild(section("Restart Ladder", "Restart escalation policy — see api/v1alpha1/agent.go.", (body) => {
    a.spec.restartLadder = a.spec.restartLadder || {};
    body.appendChild(textareaField("RestartLadder JSON (advanced)", JSON.stringify(a.spec.restartLadder, null, 2), (v) => {
      try { a.spec.restartLadder = v.trim() ? JSON.parse(v) : undefined; markDirty(); }
      catch (_) {}
    }, "{}"));
  }, false, true));
}

function updateTitle() {
  if (!state.draft) return;
  const name = state.draft.metadata.name || "(unnamed)";
  const ns = StudioResources.namespaced(state.draft.kind) ? state.draft.metadata.namespace || "default" : "cluster";
  document.getElementById("editor-title").textContent = `${name}  ·  ${ns}${state.existsInCluster ? "" : "  (draft)"}${state.dirty && state.existsInCluster ? "  *" : ""}`;
}

// ============================================================================
// Tool builder
// ============================================================================
function renderToolBuilder(host) {
  const t = state.draft;
  t.spec = t.spec || {};
  t.metadata = t.metadata || {};
  const spec = t.spec;
  const m = t.metadata;

  // Set field on spec; delete when the value is "empty" so we don't leak
  // meaningless keys into the YAML round-trip.
  const setOrDel = (obj, key, v) => {
    const isEmptyObj = v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0;
    const isEmptyArr = Array.isArray(v) && v.length === 0;
    if (v === undefined || v === null || v === "" || isEmptyObj || isEmptyArr) delete obj[key];
    else obj[key] = v;
    markDirty();
  };

  // ---- Identity ----
  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", m.name || "", (v) => { m.name = v; markDirty(); updateTitle(); }, true, "my-tool"),
      textField("Namespace", m.namespace || "default", (v) => { m.namespace = v; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", m.labels || {}, (v) => { m.labels = v; markDirty(); }));
    body.appendChild(kvField("Annotations", m.annotations || {}, (v) => { m.annotations = v; markDirty(); }));
  }, true));

  // ---- Protocol & Class ----
  host.appendChild(section("Protocol & Class", "How agents invoke this tool.", (body) => {
    body.appendChild(fieldGrid([
      selectField("Protocol", spec.protocol || "MCP", ["MCP", "OpenAPI", "native", "browser", "shell"], (v) => {
        setOrDel(spec, "protocol", v === "MCP" ? undefined : v);
        renderEditor();
      }, true),
      refSelect("Tool class", spec.className || "", state.lists.toolclasses || [], (v) => setOrDel(spec, "className", v), true),
    ]));
    body.appendChild(textareaField("Description", spec.description || "", (v) => setOrDel(spec, "description", v), "Human-readable docs shown to operators."));
    body.appendChild(textField("Endpoint", spec.endpoint || "", (v) => setOrDel(spec, "endpoint", v), false, "https://…/mcp  or  https://…/openapi"));
    const proto = spec.protocol || "MCP";
    if (proto !== "MCP" && proto !== "OpenAPI") {
      body.appendChild(previewBanner("info", `Studio can only test <strong>MCP</strong> and <strong>OpenAPI</strong> tools directly. <code>${escapeHtml(proto)}</code> tools deploy fine — test them by invoking an Agent that grants this tool.`));
    }
  }, true));

  // ---- Secrets ----
  host.appendChild(section("Secrets", "Bind cluster SecretRefs to runtime env keys.", (body) => {
    body.appendChild(secretRefsField("Secret refs", spec.secretRefs || [], state.lists.secretrefs || [], (v) => setOrDel(spec, "secretRefs", v)));
  }));

  // ---- Schema ----
  host.appendChild(section("Schema", "Read-only view of the declared or introspected schema. Live introspection is available after deployment.", (body) => {
    body.appendChild(renderBuilderSchemaField(spec, setOrDel));
  }, (spec.protocol || "MCP") === "OpenAPI"));

  // ---- Metadata ----
  host.appendChild(section("Metadata", "Free-form key/value hints for the runtime (e.g. builtin=human-approval).", (body) => {
    body.appendChild(kvField("Metadata", spec.metadata || {}, (v) => setOrDel(spec, "metadata", v)));
  }));

  // ---- Sandbox ----
  host.appendChild(section("Sandbox", "Runtime enforces these limits when invoking the tool.", (body) => {
    const sb = spec.sandbox || {};
    const updateSandbox = () => setOrDel(spec, "sandbox", { ...sb });
    body.appendChild(fieldGrid([
      durationField("Timeout", sb.timeout, (v) => { if (v === undefined) delete sb.timeout; else sb.timeout = v; updateSandbox(); }, "e.g. 30s"),
      numberField("CPU (millicores)", sb.cpuMilli, (v) => { if (v === undefined) delete sb.cpuMilli; else sb.cpuMilli = v; updateSandbox(); }, "500 = 0.5 CPU"),
      numberField("Memory (MB)", sb.memoryMB, (v) => { if (v === undefined) delete sb.memoryMB; else sb.memoryMB = v; updateSandbox(); }),
      selectField("Network policy", sb.networkPolicy || "", ["", "none", "egress-only", "full"], (v) => { if (!v) delete sb.networkPolicy; else sb.networkPolicy = v; updateSandbox(); }),
    ]));
  }));

  // ---- Health probe ----
  host.appendChild(section("Health probe", "How the controller determines tool readiness.", (body) => {
    const hp = spec.healthProbe || {};
    const updateProbe = () => setOrDel(spec, "healthProbe", { ...hp });
    body.appendChild(fieldGrid([
      selectField("Type", hp.type || "", ["", "mcp-ping", "http-get", "tcp", "exec"], (v) => { if (!v) delete hp.type; else hp.type = v; updateProbe(); }),
      durationField("Interval", hp.interval, (v) => { if (v === undefined) delete hp.interval; else hp.interval = v; updateProbe(); }),
      durationField("Timeout", hp.timeout, (v) => { if (v === undefined) delete hp.timeout; else hp.timeout = v; updateProbe(); }),
      numberField("Failure threshold", hp.failureThreshold, (v) => { if (v === undefined) delete hp.failureThreshold; else hp.failureThreshold = v; updateProbe(); }),
      numberField("Success threshold", hp.successThreshold, (v) => { if (v === undefined) delete hp.successThreshold; else hp.successThreshold = v; updateProbe(); }),
    ]));
  }));

  // ---- Supply chain ----
  host.appendChild(section("Supply chain", "Digest pinning and image signature verification.", (body) => {
    body.appendChild(textField("Digest pin", spec.digestPin || "", (v) => setOrDel(spec, "digestPin", v), false, "sha256:<hex>"));
    body.appendChild(textareaField("Signature", spec.signature || "", (v) => setOrDel(spec, "signature", v), "cosign-compatible signature"));
    body.appendChild(checkboxField("Trusted — run in-process without sandbox", !!spec.trusted, (v) => setOrDel(spec, "trusted", v ? true : undefined)));
    if (spec.trusted) {
      body.appendChild(previewBanner("warn", "<strong>Trusted</strong> tools execute in the controller process without sandbox isolation. Only enable for tools you fully trust."));
    }
  }, false, true));

  // ---- Legacy ----
  host.appendChild(section("Legacy", "Older fields kept for compatibility.", (body) => {
    body.appendChild(jsonMapField("Input schema (deprecated)", spec.inputSchema, (v) => setOrDel(spec, "inputSchema", v), "prefer the Schema section above"));
  }, false, true));
}

// ============================================================================
// ToolClass builder
// ============================================================================
function renderToolClassBuilder(host) {
  const tc = state.draft;
  tc.spec = tc.spec || {};
  tc.metadata = tc.metadata || {};
  const spec = tc.spec;
  const m = tc.metadata;

  const setOrDel = (obj, key, v) => {
    const isEmptyObj = v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0;
    const isEmptyArr = Array.isArray(v) && v.length === 0;
    if (v === undefined || v === null || v === "" || isEmptyObj || isEmptyArr) delete obj[key];
    else obj[key] = v;
    markDirty();
  };

  // ---- Identity ----
  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", m.name || "", (v) => { m.name = v; markDirty(); updateTitle(); }, true, "my-tool-class"),
      textField("Namespace", m.namespace || "default", (v) => { m.namespace = v; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", m.labels || {}, (v) => { m.labels = v; markDirty(); }));
    body.appendChild(kvField("Annotations", m.annotations || {}, (v) => { m.annotations = v; markDirty(); }));
  }, true));

  // ---- Provisioner ----
  host.appendChild(section("Provisioner", "The provider that materializes tool instances for this class.", (body) => {
    body.appendChild(textField("Provisioner", spec.provisioner || "", (v) => setOrDel(spec, "provisioner", v), true, "e.g. mcp, openapi, native"));
    body.appendChild(kvField("Parameters", spec.parameters || {}, (v) => setOrDel(spec, "parameters", v)));
  }, true));

  // ---- Execution ----
  host.appendChild(section("Execution", "How tool instances of this class are launched.", (body) => {
    body.appendChild(fieldGrid([
      selectField("Execution tier", spec.executionTier || "", ["", "process", "sidecar", "pod", "vm"], (v) => setOrDel(spec, "executionTier", v)),
      selectField("Transport", spec.transport || "", ["", "stdio", "sse", "http"], (v) => setOrDel(spec, "transport", v)),
    ]));
  }, true));

  // ---- Sandbox defaults ----
  host.appendChild(section("Sandbox defaults", "Inherited by tools of this class unless overridden.", (body) => {
    const sb = spec.sandboxDefaults || {};
    const updateSandbox = () => setOrDel(spec, "sandboxDefaults", { ...sb });
    body.appendChild(fieldGrid([
      durationField("Timeout", sb.timeout, (v) => { if (v === undefined) delete sb.timeout; else sb.timeout = v; updateSandbox(); }, "e.g. 30s"),
      numberField("CPU (millicores)", sb.cpuMilli, (v) => { if (v === undefined) delete sb.cpuMilli; else sb.cpuMilli = v; updateSandbox(); }),
      numberField("Memory (MB)", sb.memoryMB, (v) => { if (v === undefined) delete sb.memoryMB; else sb.memoryMB = v; updateSandbox(); }),
      selectField("Network policy", sb.networkPolicy || "", ["", "none", "egress-only", "full"], (v) => { if (!v) delete sb.networkPolicy; else sb.networkPolicy = v; updateSandbox(); }),
    ]));
  }));

  // ---- Resource limits ----
  host.appendChild(section("Resource limits", "Upper bounds; tool instances cannot exceed these.", (body) => {
    const rl = spec.resourceLimits || {};
    const updateLimits = () => setOrDel(spec, "resourceLimits", { ...rl });
    body.appendChild(fieldGrid([
      numberField("Max CPU (millicores)", rl.maxCpuMilli, (v) => { if (v === undefined) delete rl.maxCpuMilli; else rl.maxCpuMilli = v; updateLimits(); }),
      numberField("Max memory (MB)", rl.maxMemoryMB, (v) => { if (v === undefined) delete rl.maxMemoryMB; else rl.maxMemoryMB = v; updateLimits(); }),
      durationField("Max timeout", rl.maxTimeout, (v) => { if (v === undefined) delete rl.maxTimeout; else rl.maxTimeout = v; updateLimits(); }),
    ]));
  }));
}

// ============================================================================
// KnowledgeBase builder
// ============================================================================
function renderKnowledgeBaseBuilder(host) {
  const kb = state.draft;
  kb.spec = kb.spec || {};
  kb.metadata = kb.metadata || {};
  const spec = kb.spec;
  const m = kb.metadata;

  const setOrDel = (obj, key, v) => {
    const isEmptyObj = v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0;
    const isEmptyArr = Array.isArray(v) && v.length === 0;
    if (v === undefined || v === null || v === "" || isEmptyObj || isEmptyArr) delete obj[key];
    else obj[key] = v;
    markDirty();
  };

  // ---- Identity ----
  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", m.name || "", (v) => { m.name = v; markDirty(); updateTitle(); }, true, "my-knowledge-base"),
      textField("Namespace", m.namespace || "default", (v) => { m.namespace = v; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", m.labels || {}, (v) => { m.labels = v; markDirty(); }));
    body.appendChild(kvField("Annotations", m.annotations || {}, (v) => { m.annotations = v; markDirty(); }));
  }, true));

  // ---- Class ----
  host.appendChild(section("Class", "Which KnowledgeBaseClass provisions and indexes this KB.", (body) => {
    body.appendChild(refSelect("Knowledge base class", spec.knowledgeBaseClassName || "", state.lists.knowledgebaseclasses || [], (v) => setOrDel(spec, "knowledgeBaseClassName", v), true));
    if (!(state.lists.knowledgebaseclasses || []).length) {
      body.appendChild(previewBanner("info", "No <strong>KnowledgeBaseClass</strong> found in the cluster. Create one (e.g. from <code>samples/knowledge-base/manifests.yaml</code>) before deploying this KB."));
    }
  }, true));

  // ---- Sources ----
  host.appendChild(section("Sources", "Where the corpus comes from. Add one or more entries.", (body) => {
    body.appendChild(kbSourcesField(spec.sources || [], (v) => setOrDel(spec, "sources", v)));
  }, true));

  // ---- Index config ----
  host.appendChild(section("Index config", "Override the class defaults for this KB.", (body) => {
    const ic = spec.indexConfig || {};
    const updateIC = () => setOrDel(spec, "indexConfig", { ...ic });
    body.appendChild(refSelect("Embedding model ref", (ic.embeddingModelRef && ic.embeddingModelRef.name) || "", state.lists.models || [], (v) => {
      if (!v) delete ic.embeddingModelRef;
      else ic.embeddingModelRef = { name: v };
      updateIC();
    }));
    body.appendChild(fieldGrid([
      numberField("Chunk size", ic.chunkSize, (v) => { if (v === undefined) delete ic.chunkSize; else ic.chunkSize = v; updateIC(); }, "tokens per chunk"),
      numberField("Chunk overlap", ic.chunkOverlap, (v) => { if (v === undefined) delete ic.chunkOverlap; else ic.chunkOverlap = v; updateIC(); }, "tokens shared between chunks"),
    ]));
  }));

  // ---- Refresh policy ----
  host.appendChild(section("Refresh policy", "How and when the corpus is re-indexed.", (body) => {
    const rp = spec.refreshPolicy || {};
    const updateRP = () => setOrDel(spec, "refreshPolicy", { ...rp });
    body.appendChild(fieldGrid([
      selectField("Mode", rp.mode || "", ["", "onDemand", "periodic", "webhook"], (v) => { if (!v) delete rp.mode; else rp.mode = v; updateRP(); }),
    ]));
    if (rp.mode === "periodic") {
      body.appendChild(durationField("Interval", rp.interval, (v) => { if (v === undefined) delete rp.interval; else rp.interval = v; updateRP(); }, "e.g. 1h"));
    }
    if (rp.mode === "webhook") {
      body.appendChild(textField("Webhook path", rp.webhookPath || "", (v) => { if (!v) delete rp.webhookPath; else rp.webhookPath = v; updateRP(); }, false, "/kb/refresh/<name>"));
    }
  }));

  // ---- Status (read-only) ----
  if (state.existsInCluster && kb.status) {
    host.appendChild(section("Status", "Reconciler-reported state \u2014 read only.", (body) => {
      body.appendChild(kbStatusView(kb.status));
    }, false, true));
  }

  // ---- Runtime note ----
  host.appendChild(previewBanner("info", "The current Kognition runtime treats KnowledgeBase objects as manifest-level metadata. Retrieval still flows through a Tool CR pointing at a search endpoint \u2014 KB ingestion is planned. See <code>samples/knowledge-base/manifests.yaml</code>."));
}

// ============================================================================
// KnowledgeBaseClaim builder
// ============================================================================
function renderKnowledgeBaseClaimBuilder(host) {
  const kbc = state.draft;
  kbc.spec = kbc.spec || {};
  kbc.metadata = kbc.metadata || {};
  const spec = kbc.spec;
  const m = kbc.metadata;

  const setOrDel = (obj, key, v) => {
    const isEmptyObj = v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0;
    const isEmptyArr = Array.isArray(v) && v.length === 0;
    if (v === undefined || v === null || v === "" || isEmptyObj || isEmptyArr) delete obj[key];
    else obj[key] = v;
    markDirty();
  };

  // ---- Identity ----
  host.appendChild(section("Identity", "Naming and Kubernetes metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", m.name || "", (v) => { m.name = v; markDirty(); updateTitle(); }, true, "my-kb-claim"),
      textField("Namespace", m.namespace || "default", (v) => { m.namespace = v; markDirty(); updateTitle(); }),
    ]));
    body.appendChild(kvField("Labels", m.labels || {}, (v) => { m.labels = v; markDirty(); }));
    body.appendChild(kvField("Annotations", m.annotations || {}, (v) => { m.annotations = v; markDirty(); }));
  }, true));

  // ---- Binding ----
  host.appendChild(section("Binding", "Which KnowledgeBase in this namespace the claim binds to, and how it is accessed.", (body) => {
    body.appendChild(refSelect("Knowledge base", spec.knowledgeBaseName || "", state.lists.knowledgebases || [], (v) => setOrDel(spec, "knowledgeBaseName", v), true));
    body.appendChild(selectField("Access mode", spec.accessMode || "ReadOnly", ["ReadOnly", "ReadWrite"], (v) => setOrDel(spec, "accessMode", v === "ReadOnly" ? "" : v)));
    if (!(state.lists.knowledgebases || []).length) {
      body.appendChild(previewBanner("info", "No <strong>KnowledgeBase</strong> found in this namespace. Create one from <strong>+ Knowledge Base</strong> first."));
    }
  }, true));

  // ---- Retrieval overrides ----
  host.appendChild(section("Retrieval overrides", "Per-claim overrides for query behaviour. Never mutate the underlying corpus.", (body) => {
    const rv = spec.retrieval || {};
    const updateRV = () => setOrDel(spec, "retrieval", { ...rv });
    body.appendChild(fieldGrid([
      numberField("Top K", rv.topK, (v) => { if (v === undefined) delete rv.topK; else rv.topK = v; updateRV(); }),
      textField("Min score", rv.minScore || "", (v) => { if (!v) delete rv.minScore; else rv.minScore = v; updateRV(); }, false, "e.g. 0.35"),
    ]));
    body.appendChild(kvField("Filter", rv.filter || {}, (v) => {
      if (v && Object.keys(v).length) rv.filter = v; else delete rv.filter;
      updateRV();
    }));
    const hw = rv.hybridWeights || {};
    const updateHW = () => {
      if (hw.keyword || hw.vector) rv.hybridWeights = { ...hw };
      else delete rv.hybridWeights;
      updateRV();
    };
    body.appendChild(fieldGrid([
      textField("Hybrid keyword weight", hw.keyword || "", (v) => { if (!v) delete hw.keyword; else hw.keyword = v; updateHW(); }, false, "e.g. 0.4"),
      textField("Hybrid vector weight", hw.vector || "", (v) => { if (!v) delete hw.vector; else hw.vector = v; updateHW(); }, false, "e.g. 0.6"),
    ]));
  }, false, true));

  // ---- Usage quota ----
  host.appendChild(section("Usage quota", "Caps enforced by governance/quota admission at query time.", (body) => {
    const u = spec.usage || {};
    const updateU = () => setOrDel(spec, "usage", { ...u });
    body.appendChild(fieldGrid([
      numberField("Max queries / minute", u.maxQueriesPerMinute, (v) => { if (v === undefined) delete u.maxQueriesPerMinute; else u.maxQueriesPerMinute = v; updateU(); }),
      numberField("Max tokens returned", u.maxTokensReturned, (v) => { if (v === undefined) delete u.maxTokensReturned; else u.maxTokensReturned = v; updateU(); }),
    ]));
  }, false, true));

  // ---- Status ----
  if (kbc.status && Object.keys(kbc.status).length) {
    host.appendChild(section("Status", "Reconciler-reported state \u2014 read only.", (body) => {
      const rows = [
        ["Phase", kbc.status.phase || "\u2014"],
        ["Bound KnowledgeBase", kbc.status.boundKnowledgeBase || "\u2014"],
        ["Observed generation", kbc.status.observedGeneration != null ? String(kbc.status.observedGeneration) : "\u2014"],
      ];
      const wrap = document.createElement("div"); wrap.className = "kb-status";
      for (const [k, v] of rows) {
        const row = document.createElement("div"); row.className = "kb-status-row";
        const kEl = document.createElement("span"); kEl.className = "kb-status-k"; kEl.textContent = k;
        const vEl = document.createElement("span"); vEl.className = "kb-status-v"; vEl.textContent = v;
        row.appendChild(kEl); row.appendChild(vEl); wrap.appendChild(row);
      }
      body.appendChild(wrap);
    }, false, true));
  }
}

// Editor for spec.sources on a KnowledgeBase. Each entry is a KBSource with a
// type enum and a shape that depends on the type.
function kbSourcesField(sources, onChange) {
  let items = Array.isArray(sources) ? sources.map((s) => ({ ...s })) : [];
  const wrap = document.createElement("div");
  wrap.className = "kb-sources";

  const commit = () => onChange(items.map((s) => pruneKBSource(s)));

  function pruneKBSource(s) {
    const out = { type: s.type || "inline" };
    switch (out.type) {
      case "inline":
        if (s.data) out.data = s.data;
        break;
      case "url":
      case "file":
        if (s.uri) out.uri = s.uri;
        break;
      case "directory":
        if (s.directory) out.directory = s.directory;
        break;
      case "git":
        if (s.git && (s.git.url || s.git.branch || s.git.path)) {
          out.git = {};
          if (s.git.url) out.git.url = s.git.url;
          if (s.git.branch) out.git.branch = s.git.branch;
          if (s.git.path) out.git.path = s.git.path;
        }
        break;
      case "configMap":
        if (s.configMap && s.configMap.name) out.configMap = { name: s.configMap.name };
        break;
      case "blob": {
        const b = s.blob || {};
        const blob = {};
        if (b.provider) blob.provider = b.provider;
        if (b.bucket) blob.bucket = b.bucket;
        if (b.prefix) blob.prefix = b.prefix;
        if (b.region) blob.region = b.region;
        if (b.endpoint) blob.endpoint = b.endpoint;
        if (b.credentialSecretRef && b.credentialSecretRef.name) blob.credentialSecretRef = { name: b.credentialSecretRef.name };
        if (Array.isArray(b.include) && b.include.length) blob.include = [...b.include];
        if (Array.isArray(b.exclude) && b.exclude.length) blob.exclude = [...b.exclude];
        if (Object.keys(blob).length) out.blob = blob;
        break;
      }
    }
    return out;
  }

  function refresh() {
    wrap.innerHTML = "";
    items.forEach((src, idx) => {
      const row = document.createElement("div");
      row.className = "kb-source-row";

      const header = document.createElement("div");
      header.className = "kb-source-header";
      const label = document.createElement("span");
      label.className = "kb-source-label";
      label.textContent = `Source ${idx + 1}`;
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "kb-source-remove";
      remove.title = "Remove source";
      remove.textContent = "\u2715";
      remove.addEventListener("click", () => { items.splice(idx, 1); commit(); refresh(); });
      header.appendChild(label);
      header.appendChild(remove);
      row.appendChild(header);

      row.appendChild(selectField("Type", src.type || "inline", ["inline", "url", "file", "directory", "git", "configMap", "blob"], (v) => {
        src.type = v;
        commit();
        refresh();
      }));

      switch (src.type || "inline") {
        case "inline":
          row.appendChild(textareaField("Inline data", src.data || "", (v) => { src.data = v; commit(); }, "Raw text (markdown, plain text, etc.) indexed as-is."));
          break;
        case "url":
          row.appendChild(textField("URL", src.uri || "", (v) => { src.uri = v; commit(); }, false, "https://example.com/docs.md"));
          break;
        case "file":
          row.appendChild(textField("File URI", src.uri || "", (v) => { src.uri = v; commit(); }, false, "file:///path or https://\u2026"));
          break;
        case "directory":
          row.appendChild(textField("Directory", src.directory || "", (v) => { src.directory = v; commit(); }, false, "/mnt/kb/docs"));
          break;
        case "git": {
          const g = src.git || {};
          const updateGit = () => { src.git = { ...g }; commit(); };
          row.appendChild(fieldGrid([
            textField("Git URL", g.url || "", (v) => { g.url = v; updateGit(); }, false, "https://github.com/org/repo.git"),
            textField("Branch", g.branch || "", (v) => { g.branch = v; updateGit(); }, false, "main"),
            textField("Path", g.path || "", (v) => { g.path = v; updateGit(); }, false, "docs/"),
          ]));
          break;
        }
        case "configMap": {
          const c = src.configMap || {};
          row.appendChild(textField("ConfigMap name", c.name || "", (v) => { src.configMap = { name: v }; commit(); }, false, "kb-corpus"));
          break;
        }
        case "blob": {
          const b = src.blob || {};
          const updateBlob = () => { src.blob = { ...b }; commit(); };
          row.appendChild(fieldGrid([
            selectField("Provider", b.provider || "azure", ["azure", "s3"], (v) => { b.provider = v; updateBlob(); }, true),
            textField("Bucket / container", b.bucket || "", (v) => { b.bucket = v; updateBlob(); }, true, "docs"),
            textField("Prefix", b.prefix || "", (v) => { b.prefix = v; updateBlob(); }, false, "mark7/"),
            textField("Region", b.region || "", (v) => { b.region = v; updateBlob(); }, false, "us-west-2 (s3 only)"),
            textField("Endpoint", b.endpoint || "", (v) => { b.endpoint = v; updateBlob(); }, false, "https://\u2026 (S3-compatible)"),
          ]));
          const cr = b.credentialSecretRef || {};
          row.appendChild(refSelect("Credential secret ref", cr.name || "", state.lists.secretrefs || [], (v) => {
            if (!v) delete b.credentialSecretRef;
            else b.credentialSecretRef = { name: v };
            updateBlob();
          }));
          row.appendChild(stringListField("Include globs", b.include || [], (v) => { b.include = v; updateBlob(); }, "**/*.md"));
          row.appendChild(stringListField("Exclude globs", b.exclude || [], (v) => { b.exclude = v; updateBlob(); }, "drafts/**"));
          row.appendChild(previewBanner("info", "Credentials optional \u2014 workload identity / IRSA is used when the secret ref is empty. Secret keys: <code>azure</code> \u2192 <code>connectionString</code> or <code>accountName</code>+<code>accountKey</code>/<code>sasToken</code>; <code>s3</code> \u2192 <code>accessKeyId</code>+<code>secretAccessKey</code> (+ optional <code>sessionToken</code>)."));
          break;
        }
      }
      wrap.appendChild(row);
    });

    const add = document.createElement("button");
    add.type = "button";
    add.className = "add-row";
    add.textContent = "+ Add source";
    add.addEventListener("click", () => { items.push({ type: "inline" }); commit(); refresh(); });
    wrap.appendChild(add);
  }
  refresh();
  return wrap;
}

function kbStatusView(status) {
  const wrap = document.createElement("div");
  wrap.className = "kb-status";
  const rows = [
    ["Phase", status.phase || "\u2014"],
    ["Document count", status.documentCount != null ? String(status.documentCount) : "\u2014"],
    ["Indexed chunks", status.indexedChunks != null ? String(status.indexedChunks) : "\u2014"],
    ["Observed generation", status.generation != null ? String(status.generation) : "\u2014"],
    ["Last refresh", status.lastRefresh || "\u2014"],
  ];
  for (const [k, v] of rows) {
    const row = document.createElement("div");
    row.className = "kb-status-row";
    const kEl = document.createElement("span"); kEl.className = "kb-status-k"; kEl.textContent = k;
    const vEl = document.createElement("span"); vEl.className = "kb-status-v"; vEl.textContent = v;
    row.appendChild(kEl);
    row.appendChild(vEl);
    wrap.appendChild(row);
  }
  if (Array.isArray(status.conditions) && status.conditions.length) {
    const list = document.createElement("ul");
    list.className = "kb-conditions";
    for (const c of status.conditions) {
      const li = document.createElement("li");
      const mark = c.status === "True" ? "\u2713" : c.status === "False" ? "\u2717" : "\u2022";
      li.textContent = `${mark} ${c.type}: ${c.reason || ""} ${c.message ? "\u2014 " + c.message : ""}`;
      list.appendChild(li);
    }
    wrap.appendChild(list);
  }
  return wrap;
}

// ============================================================================
// Form widgets
// ============================================================================
function section(title, desc, build, openByDefault = false, advanced = false) {
  const s = document.createElement("details");
  s.className = "form-section";
  if (openByDefault) s.open = true;
  const sum = document.createElement("summary");
  const titleSpan = document.createElement("span");
  titleSpan.textContent = title;
  if (advanced) {
    const tag = document.createElement("span");
    tag.className = "advanced-tag"; tag.textContent = "advanced";
    titleSpan.appendChild(tag);
  }
  sum.appendChild(titleSpan);
  s.appendChild(sum);
  const body = document.createElement("div");
  body.className = "section-body";
  if (desc) {
    const d = document.createElement("div"); d.className = "section-desc"; d.textContent = desc;
    body.appendChild(d);
  }
  build(body);
  s.appendChild(body);
  return s;
}

let generatedFieldID = 0;

function field(label, control, opts = {}) {
  const f = document.createElement("div"); f.className = "field";
  if (label) {
    if (!control.id) control.id = `studio-field-${++generatedFieldID}`;
    const l = document.createElement("label");
    l.htmlFor = control.id;
    l.textContent = label;
    if (opts.required) { const r = document.createElement("span"); r.className = "req"; r.textContent = "*"; l.appendChild(r); }
    f.appendChild(l);
  }
  f.appendChild(control);
  if (opts.hint) {
    const h = document.createElement("div"); h.className = "hint"; h.id = `${control.id}-hint`; h.textContent = opts.hint;
    control.setAttribute("aria-describedby", h.id);
    f.appendChild(h);
  }
  return f;
}

function fieldGrid(children) {
  const g = document.createElement("div"); g.className = "field-grid";
  for (const c of children) g.appendChild(c);
  return g;
}

function textField(label, value, onChange, required, placeholder) {
  const i = document.createElement("input");
  i.type = "text"; i.value = value || ""; if (placeholder) i.placeholder = placeholder;
  i.addEventListener("input", () => onChange(i.value));
  return field(label, i, { required });
}

function numberField(label, value, onChange, hint) {
  const i = document.createElement("input");
  i.type = "number"; i.value = (value === undefined || value === null) ? "" : value;
  i.addEventListener("input", () => onChange(i.value === "" ? undefined : Number(i.value)));
  return field(label, i, { hint });
}

function textareaField(label, value, onChange, placeholder) {
  const t = document.createElement("textarea");
  t.value = value || ""; if (placeholder) t.placeholder = placeholder;
  t.addEventListener("input", () => onChange(t.value));
  return field(label, t);
}

function checkboxField(label, value, onChange) {
  const wrap = document.createElement("div"); wrap.className = "field-row";
  const cb = document.createElement("input"); cb.type = "checkbox"; cb.checked = !!value;
  cb.addEventListener("change", () => onChange(cb.checked));
  const l = document.createElement("label"); l.textContent = label;
  wrap.appendChild(cb); wrap.appendChild(l);
  const f = document.createElement("div"); f.className = "field"; f.appendChild(wrap);
  return f;
}

function selectField(label, value, options, onChange, required) {
  const s = document.createElement("select");
  for (const o of options) {
    const opt = document.createElement("option");
    if (typeof o === "string") { opt.value = o; opt.textContent = o; }
    else { opt.value = o.value; opt.textContent = o.label; }
    if (opt.value === value) opt.selected = true;
    s.appendChild(opt);
  }
  s.addEventListener("change", () => onChange(s.value));
  return field(label, s, { required });
}

function refSelect(label, currentName, items, onChange, required) {
  const opts = [{ value: "", label: "— none —" }];
  for (const it of items) {
    const name = (it.metadata && it.metadata.name) || it.name;
    opts.push({ value: name, label: name + (it.metadata?.namespace ? `  (${it.metadata.namespace})` : "") });
  }
  // If currentName isn't in the list (filtered by namespace or not loaded yet), still allow it.
  if (currentName && !opts.find((o) => o.value === currentName)) opts.push({ value: currentName, label: currentName + "  (not found)" });
  return selectField(label, currentName || "", opts, onChange, required);
}

function refMultiSelect(label, currentNames, items, onChange) {
  const wrap = document.createElement("div");
  const chips = document.createElement("div"); chips.className = "chip-list";
  function refresh() {
    chips.innerHTML = "";
    for (const n of currentNames) {
      const chip = document.createElement("span"); chip.className = "chip";
      chip.appendChild(document.createTextNode(n));
      const rm = document.createElement("span"); rm.className = "remove"; rm.textContent = "✕";
      rm.addEventListener("click", () => {
        currentNames = currentNames.filter((x) => x !== n);
        onChange([...currentNames]);
        refresh();
      });
      chip.appendChild(rm);
      chips.appendChild(chip);
    }
    const sel = document.createElement("select");
    sel.style.minWidth = "8rem";
    const placeholder = document.createElement("option");
    placeholder.value = ""; placeholder.textContent = "+ add"; sel.appendChild(placeholder);
    for (const it of items) {
      const name = (it.metadata && it.metadata.name) || it.name;
      if (currentNames.includes(name)) continue;
      const o = document.createElement("option"); o.value = name; o.textContent = name; sel.appendChild(o);
    }
    sel.addEventListener("change", () => {
      if (sel.value) {
        currentNames.push(sel.value);
        onChange([...currentNames]);
        refresh();
      }
    });
    chips.appendChild(sel);
  }
  refresh();
  wrap.appendChild(chips);
  return field(label, wrap);
}

function toolPicker(current, items, onChange) {
  const wrap = document.createElement("div");
  const chips = document.createElement("div"); chips.className = "chip-list";
  function refresh() {
    chips.innerHTML = "";
    for (let i = 0; i < current.length; i++) {
      const t = current[i];
      const chip = document.createElement("span"); chip.className = "chip" + (t.required ? " required" : "");
      chip.appendChild(document.createTextNode(t.ref?.name || "?"));
      const req = document.createElement("label");
      req.style.fontSize = "0.7rem"; req.style.marginLeft = "0.35rem"; req.style.cursor = "pointer";
      const cb = document.createElement("input"); cb.type = "checkbox"; cb.checked = !!t.required; cb.style.marginRight = "0.2rem";
      cb.addEventListener("change", () => { t.required = cb.checked; onChange([...current]); refresh(); });
      req.appendChild(cb); req.appendChild(document.createTextNode("required"));
      chip.appendChild(req);
      const rm = document.createElement("span"); rm.className = "remove"; rm.textContent = "✕";
      rm.addEventListener("click", () => { current.splice(i, 1); onChange([...current]); refresh(); });
      chip.appendChild(rm);
      chips.appendChild(chip);
    }
    const sel = document.createElement("select");
    const placeholder = document.createElement("option"); placeholder.value = ""; placeholder.textContent = "+ add tool"; sel.appendChild(placeholder);
    for (const it of items) {
      const name = (it.metadata && it.metadata.name) || it.name;
      if (current.find((x) => x.ref?.name === name)) continue;
      const o = document.createElement("option"); o.value = name; o.textContent = name; sel.appendChild(o);
    }
    sel.addEventListener("change", () => {
      if (sel.value) {
        current.push({ ref: { name: sel.value }, required: false });
        onChange([...current]);
        refresh();
      }
    });
    chips.appendChild(sel);
  }
  refresh();
  wrap.appendChild(chips);
  return wrap;
}

// kbClaimPicker edits a list of KnowledgeBaseClaimRef entries (`{name, required}`)
// against the pool of KnowledgeBaseClaims in the current namespace.
function kbClaimPicker(current, items, onChange) {
  const wrap = document.createElement("div");
  const chips = document.createElement("div"); chips.className = "chip-list";
  function refresh() {
    chips.innerHTML = "";
    for (let i = 0; i < current.length; i++) {
      const t = current[i];
      const chip = document.createElement("span"); chip.className = "chip" + (t.required ? " required" : "");
      chip.appendChild(document.createTextNode(t.name || "?"));
      const req = document.createElement("label");
      req.style.fontSize = "0.7rem"; req.style.marginLeft = "0.35rem"; req.style.cursor = "pointer";
      const cb = document.createElement("input"); cb.type = "checkbox"; cb.checked = !!t.required; cb.style.marginRight = "0.2rem";
      cb.addEventListener("change", () => { t.required = cb.checked; onChange([...current]); refresh(); });
      req.appendChild(cb); req.appendChild(document.createTextNode("required"));
      chip.appendChild(req);
      const rm = document.createElement("span"); rm.className = "remove"; rm.textContent = "\u2715";
      rm.addEventListener("click", () => { current.splice(i, 1); onChange([...current]); refresh(); });
      chip.appendChild(rm);
      chips.appendChild(chip);
    }
    const sel = document.createElement("select");
    const placeholder = document.createElement("option"); placeholder.value = ""; placeholder.textContent = "+ add KB claim"; sel.appendChild(placeholder);
    for (const it of items) {
      const name = (it.metadata && it.metadata.name) || it.name;
      if (current.find((x) => x.name === name)) continue;
      const o = document.createElement("option"); o.value = name; o.textContent = name; sel.appendChild(o);
    }
    sel.addEventListener("change", () => {
      if (sel.value) { current.push({ name: sel.value, required: false }); onChange([...current]); refresh(); }
    });
    chips.appendChild(sel);
  }
  refresh();
  wrap.appendChild(chips);
  return wrap;
}

function kvField(label, map, onChange) {
  const wrap = document.createElement("div"); wrap.className = "kv-rows";
  function refresh() {
    wrap.innerHTML = "";
    const entries = Object.entries(map);
    if (!entries.length) {
      const empty = document.createElement("div"); empty.className = "hint";
      empty.textContent = `No ${label.toLowerCase()} configured. Entries are optional key/value pairs.`;
      wrap.appendChild(empty);
    }
    for (let i = 0; i < entries.length; i++) {
      const [k, v] = entries[i];
      let currentKey = k;
      const row = document.createElement("div"); row.className = "kv-row";
      const ki = document.createElement("input"); ki.type = "text"; ki.value = k; ki.placeholder = "key";
      const vi = document.createElement("input"); vi.type = "text"; vi.value = v; vi.placeholder = "value";
      ki.setAttribute("aria-label", `${label} key ${i + 1}`);
      vi.setAttribute("aria-label", `${label} value ${i + 1}`);
      const rm = document.createElement("button"); rm.type = "button"; rm.textContent = "✕";
      rm.setAttribute("aria-label", `Remove ${label.toLowerCase()} entry ${i + 1}`);
      ki.addEventListener("input", () => {
        if (ki.value === currentKey) return;
        delete map[currentKey];
        currentKey = ki.value;
        map[currentKey] = vi.value;
        onChange({ ...map });
      });
      vi.addEventListener("input", () => { map[ki.value] = vi.value; onChange({ ...map }); });
      rm.addEventListener("click", () => { delete map[currentKey]; onChange({ ...map }); refresh(); });
      row.appendChild(field("Key", ki)); row.appendChild(field("Value", vi)); row.appendChild(rm);
      wrap.appendChild(row);
    }
    const add = document.createElement("button"); add.type = "button"; add.className = "add-row"; add.textContent = "+ add entry";
    add.addEventListener("click", () => { map[""] = ""; onChange({ ...map }); refresh(); });
    wrap.appendChild(add);
  }
  refresh();
  return field(label, wrap);
}

// Editable list of plain strings. Renders as one text input per entry, with
// add/remove controls; the parent gets a fresh array on every change.
function stringListField(label, values, onChange, placeholder) {
  const list = Array.isArray(values) ? [...values] : [];
  const wrap = document.createElement("div"); wrap.className = "kv-list";
  function refresh() {
    wrap.innerHTML = "";
    list.forEach((val, i) => {
      const row = document.createElement("div"); row.className = "kv-row";
      const input = document.createElement("input"); input.type = "text"; input.value = val || ""; input.placeholder = placeholder || "";
      const rm = document.createElement("button"); rm.type = "button"; rm.textContent = "\u2715";
      input.addEventListener("input", () => { list[i] = input.value; onChange([...list]); });
      rm.addEventListener("click", () => { list.splice(i, 1); onChange([...list]); refresh(); });
      row.appendChild(input); row.appendChild(rm);
      wrap.appendChild(row);
    });
    const add = document.createElement("button"); add.type = "button"; add.className = "add-row"; add.textContent = "+ add entry";
    add.addEventListener("click", () => { list.push(""); onChange([...list]); refresh(); });
    wrap.appendChild(add);
  }
  refresh();
  return field(label, wrap);
}

// Go duration input. Accepts strings like "30s", "500ms", "5m", "1h30m".
// Validation is best-effort on blur; empty is always allowed (caller decides
// whether the field is required).
const GO_DURATION_RE = /^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))*$/i;
function durationField(label, value, onChange, placeholder) {
  const wrap = document.createElement("div");
  const i = document.createElement("input");
  i.type = "text"; i.value = value || "";
  i.placeholder = placeholder || "e.g. 30s";
  const err = document.createElement("div"); err.className = "field-error"; err.style.display = "none";
  i.addEventListener("input", () => {
    err.style.display = "none";
    onChange(i.value === "" ? undefined : i.value);
  });
  i.addEventListener("blur", () => {
    const v = i.value.trim();
    if (v && !GO_DURATION_RE.test(v)) {
      err.textContent = "Not a valid Go duration (e.g. 30s, 500ms, 5m).";
      err.style.display = "";
    } else {
      err.style.display = "none";
    }
  });
  wrap.appendChild(i); wrap.appendChild(err);
  return field(label, wrap, { hint: "Go duration format: e.g. 30s, 500ms, 5m." });
}

// JSON-object field: textarea whose raw text lives in a local closure so
// intermediate keystrokes don't destroy in-progress edits. Parses on blur
// only. Empty text becomes undefined (or {} depending on caller intent).
function jsonMapField(label, value, onChange, placeholder, opts) {
  const options = opts || {};
  const wrap = document.createElement("div");
  const t = document.createElement("textarea");
  t.rows = 8;
  t.spellcheck = false;
  t.placeholder = placeholder || '{ "type": "object" }';
  let raw = (value === undefined || value === null) ? "" : JSON.stringify(value, null, 2);
  t.value = raw;
  const err = document.createElement("div"); err.className = "field-error"; err.style.display = "none";
  t.addEventListener("input", () => { raw = t.value; err.style.display = "none"; });
  t.addEventListener("blur", () => {
    const trimmed = raw.trim();
    if (trimmed === "") {
      if (options.required) {
        err.textContent = "This field is required.";
        err.style.display = "";
        return;
      }
      onChange(undefined);
      err.style.display = "none";
      return;
    }
    try {
      const parsed = JSON.parse(trimmed);
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
        err.textContent = "Value must be a JSON object.";
        err.style.display = "";
        return;
      }
      onChange(parsed);
      err.style.display = "none";
    } catch (e) {
      err.textContent = "JSON parse error: " + e.message;
      err.style.display = "";
    }
  });
  wrap.appendChild(t); wrap.appendChild(err);
  return field(label, wrap, { required: !!options.required });
}

// Render a dynamic form driven by a JSON Schema (draft-07 shape). `values` is
// mutated in place; onChange fires after every edit with the same reference so
// callers can persist without a copy. Falls back gracefully when the schema is
// missing properties, has non-object type, or contains fields we can't map to
// a widget (renders those as raw JSON textareas).
function renderSchemaForm(schema, values, onChange) {
  const container = document.createElement("div");
  container.className = "schema-form";
  if (!schema || typeof schema !== "object" || (schema.type && schema.type !== "object")) {
    const msg = document.createElement("div"); msg.className = "hint";
    msg.textContent = "No structured schema available — use Raw JSON mode.";
    container.appendChild(msg);
    return container;
  }
  const props = schema.properties || {};
  const required = Array.isArray(schema.required) ? schema.required : [];
  const keys = Object.keys(props);
  if (keys.length === 0) {
    const msg = document.createElement("div"); msg.className = "hint";
    msg.textContent = "This tool takes no inputs.";
    container.appendChild(msg);
    return container;
  }
  const grid = fieldGrid([]);
  for (const key of keys) {
    const prop = props[key] || {};
    const label = prop.title || key;
    const isReq = required.includes(key);
    const desc = typeof prop.description === "string" ? prop.description : "";
    const type = prop.type;
    const setVal = (v) => {
      if (v === undefined) delete values[key];
      else values[key] = v;
      onChange(values);
    };
    let el = null;
    if (Array.isArray(prop.enum) && prop.enum.length) {
      const opts = prop.enum.map((v) => ({ value: String(v), label: String(v) }));
      if (!isReq) opts.unshift({ value: "", label: "— unset —" });
      const cur = values[key];
      el = selectField(label, cur === undefined ? "" : String(cur), opts, (v) => {
        if (v === "") return setVal(undefined);
        if (type === "integer" || type === "number") return setVal(Number(v));
        if (type === "boolean") return setVal(v === "true");
        return setVal(v);
      }, isReq);
    } else if (type === "boolean") {
      el = checkboxField(label, !!values[key], (v) => setVal(v));
    } else if (type === "integer" || type === "number") {
      const hintBits = [];
      if (typeof prop.minimum === "number") hintBits.push(`min ${prop.minimum}`);
      if (typeof prop.maximum === "number") hintBits.push(`max ${prop.maximum}`);
      if (prop.default !== undefined) hintBits.push(`default ${prop.default}`);
      el = numberField(label, values[key], (v) => {
        if (v === undefined) return setVal(undefined);
        if (type === "integer") return setVal(Math.trunc(v));
        return setVal(v);
      }, hintBits.join(" · "));
    } else if (type === "string") {
      const placeholder = prop.example !== undefined ? String(prop.example) : (prop.default !== undefined ? String(prop.default) : "");
      const cur = values[key];
      if (prop.format === "textarea" || (typeof cur === "string" && cur.includes("\n"))) {
        el = textareaField(label, cur === undefined ? "" : String(cur), (v) => setVal(v === "" ? undefined : v), placeholder);
      } else {
        el = textField(label, cur === undefined ? "" : String(cur), (v) => setVal(v === "" ? undefined : v), isReq, placeholder);
      }
    } else if (type === "object" && prop.properties) {
      const nested = (values[key] && typeof values[key] === "object" && !Array.isArray(values[key])) ? values[key] : (values[key] = {});
      const sub = renderSchemaForm(prop, nested, () => onChange(values));
      const fs = document.createElement("fieldset"); fs.className = "schema-form-nested";
      const legend = document.createElement("legend"); legend.textContent = label + (isReq ? " *" : ""); fs.appendChild(legend);
      if (desc) { const h = document.createElement("div"); h.className = "hint"; h.textContent = desc; fs.appendChild(h); }
      fs.appendChild(sub);
      grid.appendChild(fs);
      continue;
    } else {
      const cur = values[key];
      const raw = cur === undefined ? "" : (typeof cur === "string" ? cur : JSON.stringify(cur, null, 2));
      el = textareaField(label + " (JSON)", raw, (v) => {
        const trimmed = (v || "").trim();
        if (trimmed === "") return setVal(undefined);
        try { setVal(JSON.parse(trimmed)); } catch (_) { /* keep editing */ }
      }, prop.description || "");
    }
    if (el && desc) {
      const h = document.createElement("div"); h.className = "hint"; h.textContent = desc;
      el.appendChild(h);
    }
    if (el) grid.appendChild(el);
  }
  container.appendChild(grid);
  return container;
}

// Seed a values object from a JSON Schema's declared defaults. Only fills
// properties that don't already have a value in `values`. Idempotent.
function seedValuesFromSchema(schema, values) {
  if (!schema || typeof schema !== "object") return values;
  const props = schema.properties || {};
  for (const key of Object.keys(props)) {
    const prop = props[key] || {};
    if (values[key] !== undefined) continue;
    if (prop.default !== undefined) values[key] = prop.default;
    else if (prop.type === "object" && prop.properties) {
      values[key] = {};
      seedValuesFromSchema(prop, values[key]);
    }
  }
  return values;
}

// Repeatable list of {name, key} pairs binding cluster SecretRefs to runtime
// env keys. `secretsList` is state.lists.secretrefs — we surface names in a
// picker so operators can't fat-finger a reference to a nonexistent Secret.
function secretRefsField(label, list, secretsList, onChange) {
  // Work on a private copy so callers don't observe intermediate states.
  const rows = Array.isArray(list) ? list.map((r) => ({ ...r })) : [];
  const wrap = document.createElement("div"); wrap.className = "kv-rows";
  function commit() { onChange(rows.map((r) => ({ ...r }))); }
  function refresh() {
    wrap.innerHTML = "";
    for (let i = 0; i < rows.length; i++) {
      const row = rows[i];
      const line = document.createElement("div"); line.className = "kv-row";
      // name select
      const sel = document.createElement("select"); sel.style.minWidth = "10rem";
      const empty = document.createElement("option"); empty.value = ""; empty.textContent = "— select secret —"; sel.appendChild(empty);
      for (const s of secretsList || []) {
        const nm = (s.metadata && s.metadata.name) || s.name;
        const opt = document.createElement("option"); opt.value = nm; opt.textContent = nm; sel.appendChild(opt);
      }
      if (row.name && !(secretsList || []).some((s) => ((s.metadata && s.metadata.name) || s.name) === row.name)) {
        const opt = document.createElement("option"); opt.value = row.name; opt.textContent = row.name + "  (not found)"; sel.appendChild(opt);
      }
      sel.value = row.name || "";
      sel.addEventListener("change", () => { row.name = sel.value; commit(); refresh(); });
      // key input
      const ki = document.createElement("input"); ki.type = "text"; ki.value = row.key || ""; ki.placeholder = "key";
      ki.addEventListener("input", () => { row.key = ki.value; commit(); });
      // remove
      const rm = document.createElement("button"); rm.type = "button"; rm.textContent = "✕";
      rm.addEventListener("click", () => { rows.splice(i, 1); commit(); refresh(); });
      line.appendChild(sel); line.appendChild(ki); line.appendChild(rm);
      // per-row validation: if either field is set, both must be
      if ((row.name && !row.key) || (!row.name && row.key)) {
        const err = document.createElement("div"); err.className = "field-error";
        err.textContent = "Both secret name and key are required.";
        line.appendChild(err);
      }
      wrap.appendChild(line);
    }
    const add = document.createElement("button"); add.type = "button"; add.className = "add-row"; add.textContent = "+ add secret ref";
    add.addEventListener("click", () => { rows.push({ name: "", key: "" }); commit(); refresh(); });
    wrap.appendChild(add);
  }
  refresh();
  return field(label, wrap);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// Small, self-contained Markdown renderer for chat bubbles. We do not pull in
// a library because studio is offline-capable; we escape everything first and
// then re-introduce a fixed set of inline/block constructs. Links are limited
// to http(s) and mailto schemes to avoid javascript: injection.
function renderMarkdown(src) {
  if (!src) return "";
  const codeBlocks = [];
  const inlineCodes = [];
  let text = String(src).replace(/\r\n?/g, "\n");

  // 1) Pull out fenced code blocks first so their contents are not parsed.
  text = text.replace(/```([a-zA-Z0-9_+-]*)\n([\s\S]*?)```/g, (_m, lang, code) => {
    const i = codeBlocks.length;
    codeBlocks.push({ lang: lang || "", code });
    return `\u0000CODEBLOCK${i}\u0000`;
  });

  // 2) Pull out inline code spans.
  text = text.replace(/`([^`\n]+)`/g, (_m, code) => {
    const i = inlineCodes.length;
    inlineCodes.push(code);
    return `\u0000INLINECODE${i}\u0000`;
  });

  // 3) Escape every remaining character.
  text = escapeHtml(text);

  // 4) Inline transforms applied to escaped text.
  const safeUrl = (u) => /^(https?:\/\/|mailto:|\/|#)/i.test(u) ? u : "#";
  text = text.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_m, label, url) =>
    `<a href="${escapeHtml(safeUrl(url))}" target="_blank" rel="noopener noreferrer">${label}</a>`);
  text = text.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  text = text.replace(/(^|[\s(])\*([^*\n]+)\*(?=[\s).,!?:;]|$)/g, "$1<em>$2</em>");
  text = text.replace(/(^|[\s(])_([^_\n]+)_(?=[\s).,!?:;]|$)/g, "$1<em>$2</em>");

  // 5) Block-level: split on blank lines, then per-block detect headings,
  // lists, blockquotes, fenced placeholders, otherwise paragraph with <br/>.
  const blocks = text.split(/\n{2,}/);
  const html = blocks.map((raw) => {
    const block = raw.trim();
    if (!block) return "";
    // Code block placeholder occupies its own block.
    const cbMatch = block.match(/^\u0000CODEBLOCK(\d+)\u0000$/);
    if (cbMatch) {
      const cb = codeBlocks[Number(cbMatch[1])];
      const langCls = cb.lang ? ` class="lang-${escapeHtml(cb.lang)}"` : "";
      return `<pre class="md-code"><code${langCls}>${escapeHtml(cb.code)}</code></pre>`;
    }
    // Heading: # .. ######
    const h = block.match(/^(#{1,6})\s+(.+)$/);
    if (h && !block.includes("\n")) {
      const lvl = h[1].length;
      return `<h${lvl} class="md-h">${h[2]}</h${lvl}>`;
    }
    // Unordered list: every line starts with - or *
    if (/^\s*[-*]\s+/.test(block) && block.split("\n").every((l) => /^\s*[-*]\s+/.test(l))) {
      const items = block.split("\n").map((l) => `<li>${l.replace(/^\s*[-*]\s+/, "")}</li>`).join("");
      return `<ul class="md-list">${items}</ul>`;
    }
    // Ordered list: every line starts with N.
    if (/^\s*\d+\.\s+/.test(block) && block.split("\n").every((l) => /^\s*\d+\.\s+/.test(l))) {
      const items = block.split("\n").map((l) => `<li>${l.replace(/^\s*\d+\.\s+/, "")}</li>`).join("");
      return `<ol class="md-list">${items}</ol>`;
    }
    // Blockquote: every line starts with >
    if (block.split("\n").every((l) => /^>\s?/.test(l))) {
      const inner = block.split("\n").map((l) => l.replace(/^>\s?/, "")).join("<br/>");
      return `<blockquote class="md-quote">${inner}</blockquote>`;
    }
    // Default: paragraph; preserve hard line breaks.
    return `<p class="md-p">${block.replace(/\n/g, "<br/>")}</p>`;
  }).join("");

  // 6) Restore inline code spans (after escape so we keep the raw code as text).
  return html.replace(/\u0000INLINECODE(\d+)\u0000/g, (_m, i) =>
    `<code class="md-inline-code">${escapeHtml(inlineCodes[Number(i)])}</code>`);
}

// ============================================================================
// Workflow builder — visual canvas
// ============================================================================
function setWorkflowMemoryOption(owner, key, value) {
  const memoryClaim = { ...owner.memoryClaim };
  if (value) memoryClaim[key] = value;
  else delete memoryClaim[key];
  if (Object.keys(memoryClaim).length) owner.memoryClaim = memoryClaim;
  else delete owner.memoryClaim;
}

function renderWorkflowBuilder(host) {
  const wf = state.draft;
  wf.spec = wf.spec || { steps: [], edges: [] };

  // Top: workflow-level config (collapsible) + identity
  host.appendChild(section("Identity", "Naming and metadata.", (body) => {
    body.appendChild(fieldGrid([
      textField("Name", wf.metadata.name || "", (v) => { wf.metadata.name = v; markDirty(); updateTitle(); }, true),
      textField("Namespace", wf.metadata.namespace || "default", (v) => { wf.metadata.namespace = v; markDirty(); updateTitle(); }),
    ]));
  }, true));

  host.appendChild(section("Workflow settings", "Top-level execution config — entryPoint, parallelism, fail behavior, history.", (body) => {
    const entryOpts = ["", ...(wf.spec.steps || []).map((s) => s.name)];
    body.appendChild(fieldGrid([
      selectField("EntryPoint (for cyclic graphs)", wf.spec.entryPoint || "", entryOpts.map((o) => ({ value: o, label: o || "— none —" })), (v) => { wf.spec.entryPoint = v || undefined; markDirty(); }),
      numberField("MaxParallelSteps", wf.spec.maxParallelSteps, (v) => { wf.spec.maxParallelSteps = v; markDirty(); }),
      numberField("MaxConcurrentRuns", wf.spec.maxConcurrentRuns, (v) => { wf.spec.maxConcurrentRuns = v; markDirty(); }),
      numberField("HistoryLimit", wf.spec.historyLimit, (v) => { wf.spec.historyLimit = v; markDirty(); }),
    ]));
    body.appendChild(fieldGrid([
      checkboxField("Fail fast", wf.spec.failFast, (v) => { wf.spec.failFast = v || undefined; markDirty(); }),
      checkboxField("Continue on error", wf.spec.continueOnError, (v) => { wf.spec.continueOnError = v || undefined; markDirty(); }),
    ]));
    body.appendChild(kvField("Parameters", wf.spec.parameters || {}, (m) => { wf.spec.parameters = Object.keys(m).length ? m : undefined; markDirty(); }));
  }, true));

  host.appendChild(section("Budgets", "Hard limits on workflow execution.", (body) => {
    wf.spec.budgets = wf.spec.budgets || {};
    body.appendChild(fieldGrid([
      textField("MaxDuration", wf.spec.budgets.maxDuration || "", (v) => { wf.spec.budgets.maxDuration = v || undefined; markDirty(); }, false, "10m"),
      numberField("MaxTokens", wf.spec.budgets.maxTokens, (v) => { wf.spec.budgets.maxTokens = v; markDirty(); }),
      numberField("MaxIterations", wf.spec.budgets.maxIterations, (v) => { wf.spec.budgets.maxIterations = v; markDirty(); }),
      numberField("MaxTokensPerCycle", wf.spec.budgets.maxTokensPerCycle, (v) => { wf.spec.budgets.maxTokensPerCycle = v; markDirty(); }),
    ]));
  }, false, true));

  host.appendChild(section("Emergent execution", "Allow the workflow to spawn steps beyond its declared graph.", (body) => {
    wf.spec.emergent = wf.spec.emergent || {};
    body.appendChild(checkboxField("Enabled", wf.spec.emergent.enabled, (v) => { wf.spec.emergent.enabled = v; markDirty(); }));
    body.appendChild(fieldGrid([
      numberField("MaxSteps", wf.spec.emergent.maxSteps, (v) => { wf.spec.emergent.maxSteps = v; markDirty(); }),
      numberField("MaxTokens", wf.spec.emergent.maxTokens, (v) => { wf.spec.emergent.maxTokens = v; markDirty(); }),
      textField("MaxCost", wf.spec.emergent.maxCost || "", (v) => { wf.spec.emergent.maxCost = v || undefined; markDirty(); }, false, "0.50"),
    ]));
  }, false, true));

  host.appendChild(section("Memory", "Workflow-scoped MemoryClaim and retention.", (body) => {
    const memoryClaim = wf.spec.memoryClaim || {};
    body.appendChild(fieldGrid([
      refSelect("MemoryClass", memoryClaim.memoryClassName || "", state.lists.memoryclasses || [], (v) => {
        setWorkflowMemoryOption(wf.spec, "memoryClassName", v); markDirty();
      }),
      selectField("AccessMode", memoryClaim.accessMode || "", [
        { value: "", label: "— none —" }, { value: "ReadOnly", label: "ReadOnly" }, { value: "ReadWrite", label: "ReadWrite" }, { value: "WriteOnly", label: "WriteOnly" },
      ], (v) => { setWorkflowMemoryOption(wf.spec, "accessMode", v); markDirty(); }),
      selectField("RetentionPolicy", wf.spec.memoryRetentionPolicy || "", [
        { value: "", label: "— default —" }, { value: "Delete", label: "Delete" }, { value: "Retain", label: "Retain" },
      ], (v) => { wf.spec.memoryRetentionPolicy = v || undefined; markDirty(); }),
    ]));
  }, false, true));

  // Approval gates
  host.appendChild(section("Approval Gates", "Pause execution between two steps and require human approval.", (body) => {
    const gates = wf.spec.approvalGates || [];
    body.appendChild(approvalGatesWidget(gates, (newList, label, coalesceKey) => {
      editWorkflow(label, () => {
        wf.spec.approvalGates = newList.length ? newList : undefined;
      }, { coalesceKey });
    }));
  }, false, true));

  // Canvas
  const canvasSec = section("Graph", "Click to place steps, drag to move, link the output port of one step into the input port of another to create an edge.", (body) => {
    body.appendChild(renderWorkflowCanvas());
  }, true);
  host.appendChild(canvasSec);

  // Render inspector if a step/edge is selected
  if (state.selectedStep) renderStepInspector(state.selectedStep);
  else if (state.selectedEdge) renderEdgeInspector(state.selectedEdge);
}

function approvalGatesWidget(gates, onChange) {
  const wrap = document.createElement("div");
  const update = (label, mutate, coalesceKey = null, refreshWidget = false) => {
    const next = clone(gates);
    mutate(next);
    onChange(next, label, coalesceKey);
    gates = next;
    if (refreshWidget) refresh();
  };
  function refresh() {
    wrap.innerHTML = "";
    for (let i = 0; i < gates.length; i++) {
      const g = gates[i];
      const sec = document.createElement("div");
      sec.style.background = "var(--panel-2)";
      sec.style.border = "1px solid var(--border)";
      sec.style.borderRadius = "6px";
      sec.style.padding = "0.6rem";
      sec.style.marginBottom = "0.5rem";
      const stepOpts = [{ value: "", label: "— none —" }, ...((state.draft.spec.steps || []).map((s) => ({ value: s.name, label: s.name })))];
      sec.appendChild(fieldGrid([
        selectField("From step", g.from || "", stepOpts, (v) => update("change approval gate source", (next) => { next[i].from = v; })),
        selectField("To step", g.to || "", stepOpts, (v) => update("change approval gate target", (next) => { next[i].to = v; })),
        textField("Timeout", g.timeout || "", (v) => update("edit approval gate timeout", (next) => {
          next[i].timeout = v || undefined;
        }, `approval:${i}:timeout`), false, "1h"),
      ]));
      sec.appendChild(textField("Required approvers (comma-separated)",
        Array.isArray(g.requiredApprovers) ? g.requiredApprovers.join(", ") : "",
        (v) => update("edit approval gate approvers", (next) => {
          next[i].requiredApprovers = v ? v.split(",").map((x) => x.trim()).filter(Boolean) : undefined;
        }, `approval:${i}:approvers`)));
      const rm = document.createElement("button");
      rm.type = "button"; rm.className = "action danger"; rm.textContent = "Remove gate";
      rm.addEventListener("click", () => update("remove approval gate", (next) => { next.splice(i, 1); }, null, true));
      sec.appendChild(rm);
      wrap.appendChild(sec);
    }
    const add = document.createElement("button"); add.type = "button"; add.className = "add-row"; add.textContent = "+ add gate";
    add.addEventListener("click", () => update("add approval gate", (next) => {
      next.push({ from: "", to: "", requiredApprovers: [] });
    }, null, true));
    wrap.appendChild(add);
  }
  refresh();
  return wrap;
}

// ============================================================================
// Workflow canvas
// ============================================================================
function validateWorkflowGraph(workflow = state.draft) {
  if (!workflow || workflow.kind !== "Workflow") return [];
  const spec = workflow.spec || {};
  const steps = spec.steps || [];
  const edges = spec.edges || [];
  const problems = [];
  const add = (severity, message, target = {}) => problems.push({ severity, message, ...target });
  const counts = new Map();

  for (const step of steps) {
    const name = (step.name || "").trim();
    if (!name) add("error", "A workflow step is missing its name.");
    else counts.set(name, (counts.get(name) || 0) + 1);
    if (step.type === "collaboration") {
      for (const problem of teamProblems(step, workflow)) add("error", `${name || "Unnamed team"}: ${problem}`, name ? { step: name } : {});
      continue;
    }
    const reference = step.type === "tool" ? step.toolRef?.name : step.agentRef?.name;
    if (!reference) add("error", `${name || "Unnamed step"} is missing its ${step.type === "tool" ? "tool" : "agent"} reference.`, name ? { step: name } : {});
  }
  for (const [name, count] of counts) {
    if (count > 1) add("error", `Step name “${name}” is used ${count} times. Step names must be unique.`, { step: name });
  }

  const names = new Set([...counts.keys()]);
  const validEdges = [];
  const edgeCounts = new Map();
  for (const edge of edges) {
    const target = { edge: { from: edge.from || "", to: edge.to || "" } };
    if (!edge.from || !names.has(edge.from)) add("error", `Edge source “${edge.from || "(missing)"}” does not reference a workflow step.`, target);
    if (!edge.to || !names.has(edge.to)) add("error", `Edge target “${edge.to || "(missing)"}” does not reference a workflow step.`, target);
    if (edge.from && edge.from === edge.to) add("error", `Edge from ${edge.from} to ${edge.to} cannot connect a step to itself.`, target);
    if (names.has(edge.from) && names.has(edge.to)) {
      validEdges.push(edge);
      const key = `${edge.from}\u0000${edge.to}`;
      edgeCounts.set(key, (edgeCounts.get(key) || 0) + 1);
    }
  }
  for (const [key, count] of edgeCounts) {
    if (count < 2) continue;
    const [from, to] = key.split("\u0000");
    add("error", `Edge from ${from} to ${to} is declared ${count} times.`, { edge: { from, to } });
  }

  if (spec.entryPoint && !names.has(spec.entryPoint)) {
    add("error", `Entry point “${spec.entryPoint}” does not reference a workflow step.`);
  }
  const outgoing = new Map([...names].map((name) => [name, []]));
  const indegree = new Map([...names].map((name) => [name, 0]));
  for (const edge of validEdges) {
    outgoing.get(edge.from).push(edge.to);
    indegree.set(edge.to, indegree.get(edge.to) + 1);
  }

  let cycleStep = null;
  const visiting = new Set();
  const visited = new Set();
  const visitForCycle = (name) => {
    if (visiting.has(name)) { cycleStep = name; return; }
    if (visited.has(name) || cycleStep) return;
    visiting.add(name);
    for (const next of outgoing.get(name) || []) visitForCycle(next);
    visiting.delete(name); visited.add(name);
  };
  for (const name of names) visitForCycle(name);
  if (cycleStep) {
    if (!spec.entryPoint) add("error", "The graph contains a cycle. Set EntryPoint so execution has an explicit starting step.", { step: cycleStep });
    if (Number(spec.budgets?.maxIterations || 0) <= 0) {
      add("error", "The graph contains a cycle. MaxIterations must be greater than 0 to bound cyclic execution.", { step: cycleStep });
    }
  }

  const roots = [...names].filter((name) => indegree.get(name) === 0);
  const starts = spec.entryPoint && names.has(spec.entryPoint) ? [spec.entryPoint] : roots;
  const reachable = new Set();
  const queue = [...starts];
  while (queue.length) {
    const name = queue.shift();
    if (reachable.has(name)) continue;
    reachable.add(name);
    queue.push(...(outgoing.get(name) || []));
  }
  for (const name of names) {
    if (!reachable.has(name)) add("warning", `${name} is unreachable from the workflow entry.`, { step: name });
  }

  for (const [index, gate] of (spec.approvalGates || []).entries()) {
    const label = `Approval gate ${index + 1}`;
    if (!gate.from || !names.has(gate.from)) add("error", `${label} has an invalid source step “${gate.from || "(missing)"}”.`, gate.from ? { step: gate.from } : {});
    if (!gate.to || !names.has(gate.to)) add("error", `${label} has an invalid target step “${gate.to || "(missing)"}”.`, gate.to ? { step: gate.to } : {});
    if (names.has(gate.from) && names.has(gate.to)
        && !validEdges.some((edge) => edge.from === gate.from && edge.to === gate.to)) {
      add("error", `${label} must reference an existing edge from ${gate.from} to ${gate.to}.`, { edge: { from: gate.from, to: gate.to } });
    }
  }
  return problems;
}

function workflowAssistant() {
  const pane = document.createElement("aside");
  pane.id = "workflow-graph-assistant";
  pane.className = "canvas-bottom-pane";
  pane.setAttribute("aria-label", "Workflow graph navigation and problems");
  const tabs = document.createElement("div"); tabs.className = "graph-panel-tabs"; tabs.setAttribute("role", "tablist");
  for (const panel of ["outline", "problems"]) {
    const button = document.createElement("button");
    button.type = "button"; button.dataset.graphPanel = panel; button.setAttribute("role", "tab");
    button.setAttribute("aria-controls", "workflow-graph-panel");
    button.addEventListener("click", () => { state.workflowPanel = panel; refreshWorkflowAssistant(); });
    tabs.appendChild(button);
  }
  const body = document.createElement("div"); body.id = "workflow-graph-panel"; body.className = "graph-panel-body"; body.setAttribute("role", "tabpanel");
  pane.append(tabs, body);
  return pane;
}

function activateWorkflowTarget(target) {
  const steps = new Set((state.draft?.spec?.steps || []).map((step) => step.name));
  if (target.edge && steps.has(target.edge.from) && steps.has(target.edge.to)) selectOnlyEdge(target.edge);
  else if (target.step && steps.has(target.step)) selectOnlyStep(target.step);
  else if (target.edge && steps.has(target.edge.from)) selectOnlyStep(target.edge.from);
  else if (target.edge && steps.has(target.edge.to)) selectOnlyStep(target.edge.to);
  locateWorkflowSelection();
}

function refreshWorkflowAssistant() {
  const pane = document.getElementById("workflow-graph-assistant");
  const body = document.getElementById("workflow-graph-panel");
  if (!pane || !body || state.draft?.kind !== "Workflow") return;
  const problems = validateWorkflowGraph();
  pane.querySelectorAll("[data-graph-panel]").forEach((button) => {
    const panel = button.dataset.graphPanel;
    button.textContent = panel === "problems" ? `Problems (${problems.length})` : "Outline";
    const active = panel === state.workflowPanel;
    button.classList.toggle("active", active); button.setAttribute("aria-selected", String(active)); button.tabIndex = active ? 0 : -1;
  });
  body.replaceChildren();
  if (state.workflowPanel === "problems") {
    body.setAttribute("aria-label", `${problems.length} workflow graph problems`);
    if (!problems.length) {
      const empty = document.createElement("p"); empty.className = "graph-panel-empty"; empty.textContent = "No graph problems found."; body.appendChild(empty);
      return;
    }
    const list = document.createElement("ul"); list.className = "graph-problem-list";
    for (const problem of problems) {
      const item = document.createElement("li");
      const button = document.createElement("button"); button.type = "button"; button.className = `graph-problem ${problem.severity}`;
      const severity = document.createElement("span"); severity.className = "graph-problem-severity"; severity.textContent = problem.severity;
      const message = document.createElement("span"); message.textContent = problem.message;
      button.append(severity, message); button.addEventListener("click", () => activateWorkflowTarget(problem));
      item.appendChild(button); list.appendChild(item);
    }
    body.appendChild(list);
    return;
  }

  body.setAttribute("aria-label", "Workflow graph outline");
  const list = document.createElement("ul"); list.className = "graph-outline-list";
  for (const step of state.draft.spec?.steps || []) {
    const item = document.createElement("li"); const button = document.createElement("button"); button.type = "button";
    button.textContent = `${step.name || "(unnamed)"} · ${step.type || "agent"} step`;
    button.addEventListener("click", () => activateWorkflowTarget({ step: step.name })); item.appendChild(button); list.appendChild(item);
  }
  for (const edge of state.draft.spec?.edges || []) {
    const item = document.createElement("li"); const button = document.createElement("button"); button.type = "button";
    button.textContent = `${edge.from || "(missing)"} → ${edge.to || "(missing)"} · edge`;
    button.addEventListener("click", () => activateWorkflowTarget({ edge })); item.appendChild(button); list.appendChild(item);
  }
  if (!list.children.length) {
    const empty = document.createElement("p"); empty.className = "graph-panel-empty"; empty.textContent = "The graph is empty."; body.appendChild(empty);
  } else body.appendChild(list);
}

function renderWorkflowCanvas() {
  const wrap = document.createElement("div");
  wrap.className = "canvas-wrap";

  const toolbar = document.createElement("div"); toolbar.className = "canvas-toolbar";
  const addAgentBtn = document.createElement("button"); addAgentBtn.className = "action"; addAgentBtn.textContent = "+ Agent";
  const addToolBtn = document.createElement("button"); addToolBtn.className = "action"; addToolBtn.textContent = "+ Tool";
  const addTeamBtn = document.createElement("button"); addTeamBtn.className = "action"; addTeamBtn.textContent = "+ Team step";
  addAgentBtn.addEventListener("click", () => addStep("agent"));
  addToolBtn.addEventListener("click", () => addStep("tool"));
  addTeamBtn.addEventListener("click", () => addStep("collaboration"));
  toolbar.append(addAgentBtn, addToolBtn, addTeamBtn);

  const divider = document.createElement("span"); divider.className = "toolbar-divider"; toolbar.appendChild(divider);
  const commandButton = (label, title, onClick) => {
    const button = document.createElement("button");
    button.type = "button"; button.className = "canvas-command";
    button.textContent = label; button.title = title; button.setAttribute("aria-label", title);
    button.addEventListener("click", onClick); toolbar.appendChild(button);
    return button;
  };
  const undo = commandButton("↶", "Undo graph change (Ctrl+Z)", undoWorkflowCommand);
  const redo = commandButton("↷", "Redo graph change (Ctrl+Shift+Z)", redoWorkflowCommand);
  undo.id = "workflow-undo";
  redo.id = "workflow-redo";
  undo.disabled = state.workflowHistory.undo.length === 0;
  redo.disabled = state.workflowHistory.redo.length === 0;
  commandButton("−", "Zoom out", () => setCanvasZoom(state.canvasZoom - 0.1));
  const zoomLabel = document.createElement("span"); zoomLabel.id = "canvas-zoom-label"; zoomLabel.className = "zoom-label";
  zoomLabel.textContent = `${Math.round(state.canvasZoom * 100)}%`; toolbar.appendChild(zoomLabel);
  commandButton("+", "Zoom in", () => setCanvasZoom(state.canvasZoom + 0.1));
  commandButton("Fit", "Fit graph in view", fitWorkflowGraph);
  commandButton("Locate", "Center the current selection", locateWorkflowSelection);
  commandButton("Layout", "Arrange steps automatically", autoLayoutWorkflow);
  commandButton("Duplicate", "Duplicate selected steps (Ctrl+D)", duplicateWorkflowSelection);
  commandButton("Disconnect", "Remove connections from the selection", disconnectWorkflowSelection);
  wrap.appendChild(toolbar);

  const canvas = document.createElement("div"); canvas.className = "canvas";
  canvas.id = "wf-canvas";
  canvas.tabIndex = 0;
  canvas.setAttribute("role", "application");
  canvas.setAttribute("aria-label", "Workflow graph. Use arrow keys to move a selected step, Delete to remove, and Space plus drag to pan.");

  const planeShell = document.createElement("div"); planeShell.className = "canvas-plane-shell";
  const plane = document.createElement("div"); plane.className = "canvas-plane"; plane.id = "wf-plane";
  planeShell.appendChild(plane); canvas.appendChild(planeShell);

  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.classList.add("edges");
  plane.appendChild(svg);

  // Place nodes
  const steps = state.draft.spec.steps || [];
  if (!state.canvasPositions || Object.keys(state.canvasPositions).length === 0) {
    state.canvasPositions = autoLayout(state.draft);
  }
  for (const step of steps) {
    if (!state.canvasPositions[step.name]) {
      state.canvasPositions[step.name] = { x: 40, y: 40 + Object.keys(state.canvasPositions).length * 100 };
    }
    plane.appendChild(buildNode(step));
  }

  if (steps.length === 0) {
    const empty = document.createElement("div"); empty.className = "canvas-empty";
    const title = document.createElement("strong"); title.textContent = "Start the workflow";
    const copy = document.createElement("span"); copy.textContent = "Add an agent or tool step, then connect their ports.";
    const actions = document.createElement("div"); actions.className = "canvas-empty-actions";
    const agent = addAgentBtn.cloneNode(true); agent.addEventListener("click", () => addStep("agent"));
    const tool = addToolBtn.cloneNode(true); tool.addEventListener("click", () => addStep("tool"));
    actions.append(agent, tool); empty.append(title, copy, actions); canvas.appendChild(empty);
  }

  sizeWorkflowPlane();
  renderEdges(svg);
  requestAnimationFrame(() => {
    applyCanvasTransform();
    canvas.scrollLeft = state.canvasViewport.left;
    canvas.scrollTop = state.canvasViewport.top;
    refreshWorkflowMinimapContent();
  });
  canvas.addEventListener("scroll", () => {
    state.canvasViewport = { left: canvas.scrollLeft, top: canvas.scrollTop };
    refreshWorkflowMinimapViewport();
  }, { passive: true });
  canvas.addEventListener("wheel", (event) => {
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    setCanvasZoom(state.canvasZoom + (event.deltaY < 0 ? 0.1 : -0.1), event.clientX, event.clientY);
  }, { passive: false });

  // Marquee selection on empty canvas. We only start a marquee when the pointerdown
  // originates on the bare canvas/svg (nodes/ports/edges stop propagation).
  let panStart = null;
  canvas.addEventListener("pointerdown", (e) => {
    if (state.linkFrom) return;
    if (e.target !== canvas && e.target !== plane && e.target !== svg) return;
    if (e.button === 1 || state.canvasSpacePanning) {
      e.preventDefault();
      panStart = { x: e.clientX, y: e.clientY, left: canvas.scrollLeft, top: canvas.scrollTop };
      canvas.classList.add("panning"); canvas.setPointerCapture(e.pointerId);
      return;
    }
    if (e.button !== 0) return;
    e.preventDefault();
    const { x: startX, y: startY } = workflowCanvasPoint(e.clientX, e.clientY);
    const box = document.createElement("div");
    box.className = "marquee";
    box.style.left = startX + "px"; box.style.top = startY + "px";
    box.style.width = "0px"; box.style.height = "0px";
    plane.appendChild(box);
    state.marquee = { startX, startY, el: box, moved: false, additive: e.shiftKey };
  });
  canvas.addEventListener("pointermove", (e) => {
    if (!panStart) return;
    canvas.scrollLeft = panStart.left - (e.clientX - panStart.x);
    canvas.scrollTop = panStart.top - (e.clientY - panStart.y);
  });
  const stopPan = () => { panStart = null; canvas.classList.remove("panning"); };
  canvas.addEventListener("pointerup", stopPan);
  canvas.addEventListener("pointercancel", stopPan);

  const stage = document.createElement("div"); stage.className = "canvas-stage";
  stage.append(canvas, buildWorkflowMinimap());
  wrap.appendChild(stage);
  wrap.appendChild(workflowAssistant());
  requestAnimationFrame(refreshWorkflowAssistant);
  return wrap;
}

function workflowCanvasPoint(clientX, clientY) {
  const canvas = document.getElementById("wf-canvas");
  if (!canvas) return { x: 0, y: 0 };
  const rect = canvas.getBoundingClientRect();
  return {
    x: (clientX - rect.left + canvas.scrollLeft) / state.canvasZoom,
    y: (clientY - rect.top + canvas.scrollTop) / state.canvasZoom,
  };
}

function workflowGraphBounds() {
  const positions = Object.values(state.canvasPositions || {});
  if (positions.length === 0) return { left: 0, top: 0, right: 900, bottom: 520 };
  return {
    left: Math.min(...positions.map((p) => p.x)),
    top: Math.min(...positions.map((p) => p.y)),
    right: Math.max(...positions.map((p) => p.x + 190)),
    bottom: Math.max(...positions.map((p) => p.y + 80)),
  };
}

function sizeWorkflowPlane() {
  const plane = document.getElementById("wf-plane");
  const shell = plane?.parentElement;
  if (!plane || !shell) return;
  const bounds = workflowGraphBounds();
  const width = Math.max(1000, bounds.right + 240);
  const height = Math.max(650, bounds.bottom + 200);
  plane.style.width = `${width}px`; plane.style.height = `${height}px`;
  shell.style.width = `${width * state.canvasZoom}px`; shell.style.height = `${height * state.canvasZoom}px`;
}

function applyCanvasTransform() {
  const plane = document.getElementById("wf-plane");
  if (!plane) return;
  plane.style.transform = `scale(${state.canvasZoom})`;
  sizeWorkflowPlane();
  const label = document.getElementById("canvas-zoom-label");
  if (label) label.textContent = `${Math.round(state.canvasZoom * 100)}%`;
  refreshWorkflowMinimapViewport();
}

let workflowMinimapFrame = 0;

function buildWorkflowMinimap() {
  const minimap = document.createElement("div");
  minimap.id = "workflow-minimap";
  minimap.className = "workflow-minimap";
  minimap.tabIndex = 0;
  minimap.setAttribute("role", "navigation");
  minimap.setAttribute("aria-label", "Workflow minimap. Click or drag to navigate; use arrow keys to pan.");
  minimap.title = "Workflow minimap — click or drag to navigate";

  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("preserveAspectRatio", "none");
  const edges = document.createElementNS("http://www.w3.org/2000/svg", "g");
  edges.classList.add("workflow-minimap-edges");
  const nodes = document.createElementNS("http://www.w3.org/2000/svg", "g");
  nodes.classList.add("workflow-minimap-nodes");
  const viewport = document.createElementNS("http://www.w3.org/2000/svg", "rect");
  viewport.classList.add("workflow-minimap-viewport");
  svg.append(edges, nodes, viewport);
  minimap.appendChild(svg);

  let navigatingPointer = null;
  const navigate = (event) => {
    const canvas = document.getElementById("wf-canvas");
    const plane = document.getElementById("wf-plane");
    const bounds = svg.getBoundingClientRect();
    if (!canvas || !plane || bounds.width === 0 || bounds.height === 0) return;
    const worldWidth = Number.parseFloat(plane.style.width) || 1000;
    const worldHeight = Number.parseFloat(plane.style.height) || 650;
    const x = Math.max(0, Math.min(worldWidth, (event.clientX - bounds.left) / bounds.width * worldWidth));
    const y = Math.max(0, Math.min(worldHeight, (event.clientY - bounds.top) / bounds.height * worldHeight));
    canvas.scrollLeft = x * state.canvasZoom - canvas.clientWidth / 2;
    canvas.scrollTop = y * state.canvasZoom - canvas.clientHeight / 2;
  };
  minimap.addEventListener("pointerdown", (event) => {
    if (event.button !== 0) return;
    event.preventDefault(); event.stopPropagation();
    navigatingPointer = event.pointerId;
    minimap.classList.add("navigating");
    minimap.setPointerCapture(event.pointerId);
    navigate(event);
  });
  minimap.addEventListener("pointermove", (event) => {
    if (event.pointerId === navigatingPointer) navigate(event);
  });
  const stopNavigating = (event) => {
    if (event.pointerId !== navigatingPointer) return;
    navigatingPointer = null;
    minimap.classList.remove("navigating");
  };
  minimap.addEventListener("pointerup", stopNavigating);
  minimap.addEventListener("pointercancel", stopNavigating);
  minimap.addEventListener("keydown", (event) => {
    if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key)) return;
    const canvas = document.getElementById("wf-canvas");
    if (!canvas) return;
    event.preventDefault();
    const horizontal = Math.max(80, canvas.clientWidth * 0.25);
    const vertical = Math.max(60, canvas.clientHeight * 0.25);
    canvas.scrollBy({
      left: event.key === "ArrowLeft" ? -horizontal : event.key === "ArrowRight" ? horizontal : 0,
      top: event.key === "ArrowUp" ? -vertical : event.key === "ArrowDown" ? vertical : 0,
    });
  });
  return minimap;
}

function scheduleWorkflowMinimapContentRefresh() {
  if (workflowMinimapFrame) return;
  workflowMinimapFrame = requestAnimationFrame(() => {
    workflowMinimapFrame = 0;
    refreshWorkflowMinimapContent();
  });
}

function refreshWorkflowMinimapContent() {
  const minimap = document.getElementById("workflow-minimap");
  const svg = minimap?.querySelector("svg");
  const edgeGroup = svg?.querySelector(".workflow-minimap-edges");
  const nodeGroup = svg?.querySelector(".workflow-minimap-nodes");
  const plane = document.getElementById("wf-plane");
  if (!minimap || !svg || !edgeGroup || !nodeGroup || !plane) return;
  const steps = state.draft?.spec?.steps || [];
  minimap.hidden = steps.length === 0;
  if (steps.length === 0) return;

  const worldWidth = Number.parseFloat(plane.style.width) || 1000;
  const worldHeight = Number.parseFloat(plane.style.height) || 650;
  svg.setAttribute("viewBox", `0 0 ${worldWidth} ${worldHeight}`);
  edgeGroup.replaceChildren();
  nodeGroup.replaceChildren();

  const dimensions = {};
  for (const step of steps) {
    const node = document.querySelector(`#wf-canvas .node[data-step="${CSS.escape(step.name)}"]`);
    dimensions[step.name] = { width: node?.offsetWidth || 180, height: node?.offsetHeight || 65 };
  }
  for (const edge of state.draft?.spec?.edges || []) {
    const from = state.canvasPositions[edge.from];
    const to = state.canvasPositions[edge.to];
    if (!from || !to) continue;
    const fromSize = dimensions[edge.from] || { width: 180, height: 65 };
    const toSize = dimensions[edge.to] || { width: 180, height: 65 };
    const line = document.createElementNS("http://www.w3.org/2000/svg", "line");
    line.setAttribute("x1", from.x + fromSize.width / 2);
    line.setAttribute("y1", from.y + fromSize.height / 2);
    line.setAttribute("x2", to.x + toSize.width / 2);
    line.setAttribute("y2", to.y + toSize.height / 2);
    edgeGroup.appendChild(line);
  }
  for (const step of steps) {
    const position = state.canvasPositions[step.name];
    if (!position) continue;
    const size = dimensions[step.name];
    const rect = document.createElementNS("http://www.w3.org/2000/svg", "rect");
    rect.classList.add("workflow-minimap-node", step.type === "tool" ? "tool" : "agent");
    if (state.draft?.spec?.entryPoint === step.name) rect.classList.add("entry");
    if (state.selectedSteps.has(step.name)) rect.classList.add("selected");
    rect.dataset.step = step.name;
    rect.setAttribute("x", position.x);
    rect.setAttribute("y", position.y);
    rect.setAttribute("width", size.width);
    rect.setAttribute("height", size.height);
    nodeGroup.appendChild(rect);
  }
  refreshWorkflowMinimapViewport();
}

function refreshWorkflowMinimapViewport() {
  const canvas = document.getElementById("wf-canvas");
  const plane = document.getElementById("wf-plane");
  const viewport = document.querySelector("#workflow-minimap .workflow-minimap-viewport");
  if (!canvas || !plane || !viewport) return;
  const worldWidth = Number.parseFloat(plane.style.width) || 1000;
  const worldHeight = Number.parseFloat(plane.style.height) || 650;
  const width = Math.min(worldWidth, canvas.clientWidth / state.canvasZoom);
  const height = Math.min(worldHeight, canvas.clientHeight / state.canvasZoom);
  const x = Math.max(0, Math.min(worldWidth - width, canvas.scrollLeft / state.canvasZoom));
  const y = Math.max(0, Math.min(worldHeight - height, canvas.scrollTop / state.canvasZoom));
  viewport.setAttribute("x", x);
  viewport.setAttribute("y", y);
  viewport.setAttribute("width", width);
  viewport.setAttribute("height", height);
}

function setCanvasZoom(value, clientX, clientY) {
  const canvas = document.getElementById("wf-canvas");
  if (!canvas) return;
  const oldZoom = state.canvasZoom;
  const nextZoom = Math.min(2, Math.max(0.4, Math.round(value * 10) / 10));
  if (nextZoom === oldZoom) return;
  const rect = canvas.getBoundingClientRect();
  const anchorX = clientX === undefined ? rect.width / 2 : clientX - rect.left;
  const anchorY = clientY === undefined ? rect.height / 2 : clientY - rect.top;
  const graphX = (canvas.scrollLeft + anchorX) / oldZoom;
  const graphY = (canvas.scrollTop + anchorY) / oldZoom;
  state.canvasZoom = nextZoom;
  applyCanvasTransform();
  canvas.scrollLeft = graphX * nextZoom - anchorX;
  canvas.scrollTop = graphY * nextZoom - anchorY;
}

function fitWorkflowGraph() {
  const canvas = document.getElementById("wf-canvas");
  if (!canvas) return;
  const bounds = workflowGraphBounds();
  const width = Math.max(1, bounds.right - bounds.left);
  const height = Math.max(1, bounds.bottom - bounds.top);
  state.canvasZoom = Math.min(1.5, Math.max(0.4, Math.min((canvas.clientWidth - 64) / width, (canvas.clientHeight - 64) / height)));
  applyCanvasTransform();
  canvas.scrollLeft = Math.max(0, bounds.left * state.canvasZoom - 32);
  canvas.scrollTop = Math.max(0, bounds.top * state.canvasZoom - 32);
  refreshWorkflowMinimapViewport();
}

function locateWorkflowSelection(behavior = "smooth") {
  const name = state.selectedStep || [...state.selectedSteps][0];
  const position = name && state.canvasPositions[name];
  const edge = state.selectedEdge || state.selectedEdges[0];
  const from = edge && state.canvasPositions[edge.from];
  const to = edge && state.canvasPositions[edge.to];
  const canvas = document.getElementById("wf-canvas");
  if (!canvas) return;
  const node = position && document.querySelector(`#wf-canvas .node[data-step="${CSS.escape(name)}"]`);
  if (node) {
    const nodeBounds = node.getBoundingClientRect();
    const canvasBounds = canvas.getBoundingClientRect();
    canvas.scrollBy({
      left: nodeBounds.left + nodeBounds.width / 2 - (canvasBounds.left + canvas.clientWidth / 2),
      top: nodeBounds.top + nodeBounds.height / 2 - (canvasBounds.top + canvas.clientHeight / 2),
      behavior,
    });
    node.focus();
    return;
  }
  const target = position || (from && to ? { x: (from.x + to.x) / 2, y: (from.y + to.y) / 2 } : null);
  if (!target) { fitWorkflowGraph(); return; }
  canvas.scrollTo({
    left: Math.max(0, (target.x + 90) * state.canvasZoom - canvas.clientWidth / 2),
    top: Math.max(0, (target.y + 32) * state.canvasZoom - canvas.clientHeight / 2),
    behavior,
  });
  document.querySelector(`#wf-canvas path[data-from="${CSS.escape(edge.from)}"][data-to="${CSS.escape(edge.to)}"]`)?.focus();
}

function autoLayoutWorkflow() {
  recordWorkflowHistory("auto layout");
  state.canvasPositions = autoLayout(state.draft);
  saveWorkflowLayout();
  renderEditor();
  requestAnimationFrame(fitWorkflowGraph);
}

function buildNode(step) {
  const pos = state.canvasPositions[step.name] || { x: 40, y: 40 };
  const node = document.createElement("div");
  node.className = "node";
  node.dataset.step = step.name;
  node.tabIndex = 0;
  node.setAttribute("role", "button");
  node.setAttribute("aria-label", `${step.name}, ${step.type || "agent"} workflow step`);
  node.style.left = pos.x + "px";
  node.style.top = pos.y + "px";
  if (state.draft.spec.entryPoint === step.name) node.classList.add("entry");
  if (state.selectedSteps.has(step.name)) node.classList.add("selected");

  const nameEl = document.createElement("div"); nameEl.className = "node-name"; nameEl.textContent = step.name || "(unnamed)";
  const kindEl = document.createElement("div"); kindEl.className = "node-kind"; kindEl.textContent = step.type || "agent";
  node.appendChild(nameEl); node.appendChild(kindEl);
  const refName = (step.type === "collaboration" ? `${step.collaboration?.members?.length || 0} members · ${step.collaboration?.strategy || "team"}`
    : step.type === "tool" ? step.toolRef?.name : step.agentRef?.name) || "";
  if (refName) {
    const refEl = document.createElement("div"); refEl.className = "node-ref"; refEl.textContent = refName;
    node.appendChild(refEl);
  }

  const inPort = document.createElement("div"); inPort.className = "port in"; inPort.title = "incoming edges";
  const outPort = document.createElement("div"); outPort.className = "port out"; outPort.title = "drag to create edge";
  node.appendChild(inPort); node.appendChild(outPort);

  // ---- Node drag ----
  let dragging = false;
  let dragMoved = false;
  let offset = { x: 0, y: 0 };
  let dragSnapshot = null;
  node.addEventListener("pointerdown", (e) => {
    if (e.target.classList.contains("port")) return; // ports start a link, not a drag
    if (e.button !== 0) return;
    dragging = true; dragMoved = false;
    const rect = node.getBoundingClientRect();
    offset = { x: (e.clientX - rect.left) / state.canvasZoom, y: (e.clientY - rect.top) / state.canvasZoom };
    dragSnapshot = workflowSnapshot();
    node.setPointerCapture(e.pointerId);
    e.preventDefault();
    e.stopPropagation(); // don't start a marquee
  });
  node.addEventListener("pointermove", (e) => {
    if (!dragging) return;
    dragMoved = true;
    const point = workflowCanvasPoint(e.clientX, e.clientY);
    let x = point.x - offset.x;
    let y = point.y - offset.y;
    x = Math.max(0, x); y = Math.max(0, y);
    node.style.left = x + "px"; node.style.top = y + "px";
    state.canvasPositions[step.name] = { x, y };
    sizeWorkflowPlane();
    const svg = document.querySelector("#wf-canvas svg.edges");
    if (svg) renderEdges(svg);
  });
  node.addEventListener("pointerup", (e) => {
    if (dragging && dragMoved) {
      recordWorkflowHistory("move step", dragSnapshot);
      saveWorkflowLayout();
    }
    dragging = false;
    if (e.target.classList.contains("port")) return;
    if (dragMoved) return; // drag end: let the document mouseup reset state
    if (state.linkFrom) return; // link-drop case is handled globally
    // We're about to stopPropagation so the document mouseup never sees this
    // event — reset the drag state ourselves to avoid getting stuck following
    // the cursor after a plain click.
    dragging = false;
    e.stopPropagation();
    if (e.shiftKey) toggleStepSelection(step.name);
    else selectOnlyStep(step.name);
  });

  // ---- Port linking: outPort starts; drop anywhere on a target node finishes ----
  outPort.addEventListener("pointerdown", (e) => {
    e.stopPropagation(); e.preventDefault();
    if (e.button !== 0) return;
    state.linkFrom = step.name;
    state.linkSnapshot = workflowSnapshot();
    outPort.classList.add("linking");
    document.body.style.cursor = "crosshair";
    const svg = document.querySelector("#wf-canvas svg.edges");
    if (svg) {
      const p = state.canvasPositions[step.name] || { x: 0, y: 0 };
      const startX = p.x + 180;
      const startY = p.y + 32;
      const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
      path.classList.add("link-preview");
      path.setAttribute("d", `M ${startX} ${startY} L ${startX} ${startY}`);
      svg.appendChild(path);
      state.linkPreview = { path, startX, startY };
    }
  });

  node.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault(); selectOnlyStep(step.name); return;
    }
    if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key)) return;
    event.preventDefault();
    recordWorkflowHistory("move step");
    const amount = event.shiftKey ? 20 : 5;
    const position = state.canvasPositions[step.name];
    if (event.key === "ArrowLeft") position.x = Math.max(0, position.x - amount);
    if (event.key === "ArrowRight") position.x += amount;
    if (event.key === "ArrowUp") position.y = Math.max(0, position.y - amount);
    if (event.key === "ArrowDown") position.y += amount;
    saveWorkflowLayout(); renderEditor();
    requestAnimationFrame(() => document.querySelector(`#wf-canvas .node[data-step="${CSS.escape(step.name)}"]`)?.focus());
  });

  return node;
}

// ---- Global pointermove: update link preview and marquee ----
document.addEventListener("pointermove", (e) => {
  if (state.linkFrom && state.linkPreview) {
    const canvasEl = document.getElementById("wf-canvas");
    if (canvasEl) {
      const { x, y } = workflowCanvasPoint(e.clientX, e.clientY);
      const { startX, startY } = state.linkPreview;
      const midX = (startX + x) / 2;
      state.linkPreview.path.setAttribute(
        "d", `M ${startX} ${startY} C ${midX} ${startY}, ${midX} ${y}, ${x} ${y}`);
      document.querySelectorAll("#wf-canvas .node.link-target").forEach((n) => n.classList.remove("link-target"));
      const target = findNodeAt(e.clientX, e.clientY);
      if (target && target.dataset.step !== state.linkFrom) target.classList.add("link-target");
    }
  }
  if (state.marquee) {
    const canvasEl = document.getElementById("wf-canvas");
    if (!canvasEl) return;
    const { x, y } = workflowCanvasPoint(e.clientX, e.clientY);
    const sx = state.marquee.startX, sy = state.marquee.startY;
    const left = Math.min(sx, x), top = Math.min(sy, y);
    const w = Math.abs(x - sx), h = Math.abs(y - sy);
    state.marquee.el.style.left = left + "px";
    state.marquee.el.style.top = top + "px";
    state.marquee.el.style.width = w + "px";
    state.marquee.el.style.height = h + "px";
    if (w > 2 || h > 2) state.marquee.moved = true;
  }
});

// ---- Global pointerup: finish link drag or marquee ----
document.addEventListener("pointerup", (e) => {
  if (state.linkFrom) {
    const target = findNodeAt(e.clientX, e.clientY);
    if (target && target.dataset.step && target.dataset.step !== state.linkFrom) {
      const wf = state.draft;
      if (wf && wf.spec) {
        wf.spec.edges = wf.spec.edges || [];
        const to = target.dataset.step;
        if (!wf.spec.edges.find((ed) => ed.from === state.linkFrom && ed.to === to)) {
          recordWorkflowHistory("connect steps", state.linkSnapshot);
          wf.spec.edges.push({ from: state.linkFrom, to });
          markDirty();
        }
      }
    }
    cancelLinkDrag();
    const svg = document.querySelector("#wf-canvas svg.edges");
    if (svg) renderEdges(svg);
  }

  if (state.marquee) {
    const m = state.marquee;
    state.marquee = null;
    const marqueeRect = m.el.getBoundingClientRect();
    if (m.el.parentNode) m.el.parentNode.removeChild(m.el);
    if (!m.moved) {
      if (!m.additive) clearWorkflowSelection();
      return;
    }
    const canvasEl = document.getElementById("wf-canvas");
    const intersected = new Set();
    document.querySelectorAll("#wf-canvas .node").forEach((n) => {
      const r = n.getBoundingClientRect();
      if (rectsOverlap(r, marqueeRect)) intersected.add(n.dataset.step);
    });
    const cr = canvasEl ? canvasEl.getBoundingClientRect() : { left: 0, top: 0 };
    const scrollLeft = canvasEl ? canvasEl.scrollLeft : 0;
    const scrollTop = canvasEl ? canvasEl.scrollTop : 0;
    const newEdges = [];
    for (const ed of ((state.draft && state.draft.spec && state.draft.spec.edges) || [])) {
      const from = state.canvasPositions[ed.from], to = state.canvasPositions[ed.to];
      if (!from || !to) continue;
      const mxCanvas = (from.x + 180 + to.x) / 2;
      const myCanvas = (from.y + 32 + to.y + 32) / 2;
      const mxClient = cr.left + mxCanvas * state.canvasZoom - scrollLeft;
      const myClient = cr.top + myCanvas * state.canvasZoom - scrollTop;
      if (mxClient >= marqueeRect.left && mxClient <= marqueeRect.right
          && myClient >= marqueeRect.top && myClient <= marqueeRect.bottom) {
        newEdges.push({ from: ed.from, to: ed.to });
      }
    }
    if (!m.additive) {
      state.selectedSteps.clear();
      state.selectedEdges = [];
    }
    intersected.forEach((n) => state.selectedSteps.add(n));
    for (const ne of newEdges) {
      if (!state.selectedEdges.find((x) => x.from === ne.from && x.to === ne.to)) {
        state.selectedEdges.push(ne);
      }
    }
    state.selectedStep = state.selectedSteps.size === 1 && state.selectedEdges.length === 0
      ? [...state.selectedSteps][0] : null;
    state.selectedEdge = (state.selectedSteps.size === 0 && state.selectedEdges.length === 1)
      ? state.selectedEdges[0] : null;
    applyCanvasSelectionVisuals();
    updateInspectorForSelection();
  }
});

function cancelLinkDrag() {
  document.querySelectorAll("#wf-canvas .port.linking").forEach((p) => p.classList.remove("linking"));
  document.querySelectorAll("#wf-canvas .node.link-target").forEach((n) => n.classList.remove("link-target"));
  if (state.linkPreview && state.linkPreview.path && state.linkPreview.path.parentNode) {
    state.linkPreview.path.parentNode.removeChild(state.linkPreview.path);
  }
  state.linkPreview = null;
  state.linkFrom = null;
  state.linkSnapshot = null;
  document.body.style.cursor = "";
}

function findNodeAt(clientX, clientY) {
  const el = document.elementFromPoint(clientX, clientY);
  if (!el) return null;
  return el.closest("#wf-canvas .node");
}

function rectsOverlap(a, b) {
  return !(a.right < b.left || a.left > b.right || a.bottom < b.top || a.top > b.bottom);
}

function selectOnlyStep(name) {
  state.selectedSteps.clear();
  state.selectedEdges = [];
  state.selectedSteps.add(name);
  state.selectedStep = name;
  state.selectedEdge = null;
  applyCanvasSelectionVisuals();
  updateInspectorForSelection();
}

function toggleStepSelection(name) {
  if (state.selectedSteps.has(name)) state.selectedSteps.delete(name);
  else state.selectedSteps.add(name);
  state.selectedStep = (state.selectedSteps.size === 1 && state.selectedEdges.length === 0)
    ? [...state.selectedSteps][0] : null;
  state.selectedEdge = null;
  applyCanvasSelectionVisuals();
  updateInspectorForSelection();
}

function selectOnlyEdge(edge) {
  state.selectedSteps.clear();
  state.selectedEdges = [{ from: edge.from, to: edge.to }];
  state.selectedStep = null;
  state.selectedEdge = { from: edge.from, to: edge.to };
  applyCanvasSelectionVisuals();
  updateInspectorForSelection();
}

function toggleEdgeSelection(edge) {
  const idx = state.selectedEdges.findIndex((e) => e.from === edge.from && e.to === edge.to);
  if (idx >= 0) state.selectedEdges.splice(idx, 1);
  else state.selectedEdges.push({ from: edge.from, to: edge.to });
  state.selectedEdge = (state.selectedEdges.length === 1 && state.selectedSteps.size === 0)
    ? state.selectedEdges[0] : null;
  state.selectedStep = null;
  applyCanvasSelectionVisuals();
  updateInspectorForSelection();
}

function clearWorkflowSelection() {
  state.selectedSteps.clear();
  state.selectedEdges = [];
  state.selectedStep = null;
  state.selectedEdge = null;
  applyCanvasSelectionVisuals();
  closeInspector();
}

function applyCanvasSelectionVisuals() {
  document.querySelectorAll("#wf-canvas .node").forEach((n) => {
    n.classList.toggle("selected", state.selectedSteps.has(n.dataset.step));
  });
  document.querySelectorAll("#wf-canvas svg.edges path").forEach((p) => {
    const f = p.dataset.from, t = p.dataset.to;
    if (!f) return;
    p.classList.toggle("selected",
      state.selectedEdges.some((e) => e.from === f && e.to === t));
  });
  document.querySelectorAll("#workflow-minimap .workflow-minimap-node").forEach((node) => {
    node.classList.toggle("selected", state.selectedSteps.has(node.dataset.step));
  });
}

function updateInspectorForSelection() {
  const ns = state.selectedSteps.size;
  const ne = state.selectedEdges.length;
  if (ns + ne === 0) { closeInspector(); return; }
  if (ns + ne === 1) {
    if (ns === 1) renderStepInspector([...state.selectedSteps][0]);
    else renderEdgeInspector(state.selectedEdges[0]);
    return;
  }
  renderMultiSelectInspector();
}

function renderMultiSelectInspector() {
  openInspector();
  const ns = state.selectedSteps.size, ne = state.selectedEdges.length;
  document.getElementById("inspector-title").textContent =
    `${ns} step${ns === 1 ? "" : "s"}, ${ne} edge${ne === 1 ? "" : "s"} selected`;
  const host = document.getElementById("inspector-body");
  host.innerHTML = "";
  const list = document.createElement("div");
  list.style.cssText = "font-size:0.78rem;color:var(--text-dim);margin-bottom:0.6rem;line-height:1.4;";
  if (ns > 0) {
    const ln = document.createElement("div");
    ln.textContent = "Steps: " + [...state.selectedSteps].join(", ");
    list.appendChild(ln);
  }
  if (ne > 0) {
    const le = document.createElement("div");
    le.textContent = "Edges: " + state.selectedEdges.map((e) => `${e.from} \u2192 ${e.to}`).join(", ");
    list.appendChild(le);
  }
  host.appendChild(list);
  const hint = document.createElement("div");
  hint.className = "muted";
  hint.style.cssText = "font-size:0.78rem;margin-bottom:0.6rem;";
  hint.textContent = "Press Delete or Backspace to remove the selection.";
  host.appendChild(hint);
  const del = document.createElement("button");
  del.className = "action danger";
  del.textContent = "Delete selection";
  del.addEventListener("click", deleteWorkflowSelection);
  host.appendChild(del);
}

function deleteWorkflowSelection() {
  const total = state.selectedSteps.size + state.selectedEdges.length;
  if (total === 0) return;
  if (!confirm(`Delete ${total} selected item${total === 1 ? "" : "s"}?`)) return;
  const wf = state.draft;
  if (!wf || !wf.spec) return;
  recordWorkflowHistory("delete selection");
  if (state.selectedSteps.size > 0) {
    const dead = state.selectedSteps;
    wf.spec.steps = (wf.spec.steps || []).filter((s) => !dead.has(s.name));
    wf.spec.edges = (wf.spec.edges || []).filter((e) => !dead.has(e.from) && !dead.has(e.to));
    wf.spec.approvalGates = (wf.spec.approvalGates || []).filter((g) => !dead.has(g.from) && !dead.has(g.to));
    if (wf.spec.entryPoint && dead.has(wf.spec.entryPoint)) wf.spec.entryPoint = undefined;
    for (const name of dead) delete state.canvasPositions[name];
  }
  for (const edge of state.selectedEdges) {
    wf.spec.edges = (wf.spec.edges || []).filter((x) => !(x.from === edge.from && x.to === edge.to));
    wf.spec.approvalGates = (wf.spec.approvalGates || []).filter((g) => !(g.from === edge.from && g.to === edge.to));
  }
  state.selectedSteps.clear();
  state.selectedEdges = [];
  state.selectedStep = null;
  state.selectedEdge = null;
  closeInspector();
  markDirty();
  renderEditor();
}

function duplicateWorkflowSelection() {
  const wf = state.draft;
  const selected = [...state.selectedSteps];
  if (!wf?.spec || selected.length === 0) return;
  recordWorkflowHistory("duplicate steps");
  const steps = wf.spec.steps || [];
  const names = new Set(steps.map((step) => step.name));
  const renamed = new Map();
  const copies = [];
  for (const name of selected) {
    const source = steps.find((step) => step.name === name);
    if (!source) continue;
    let suffix = 2;
    let nextName = `${name}-copy`;
    while (names.has(nextName)) nextName = `${name}-copy-${suffix++}`;
    names.add(nextName);
    renamed.set(name, nextName);
    const copy = clone(source);
    copy.name = nextName;
    copies.push(copy);
    const position = state.canvasPositions[name] || { x: 40, y: 40 };
    state.canvasPositions[nextName] = { x: position.x + 36, y: position.y + 36 };
  }
  wf.spec.steps = [...steps, ...copies];
  const internalEdges = (wf.spec.edges || [])
    .filter((edge) => renamed.has(edge.from) && renamed.has(edge.to))
    .map((edge) => ({ ...clone(edge), from: renamed.get(edge.from), to: renamed.get(edge.to) }));
  wf.spec.edges = [...(wf.spec.edges || []), ...internalEdges];
  const internalGates = (wf.spec.approvalGates || [])
    .filter((gate) => renamed.has(gate.from) && renamed.has(gate.to))
    .map((gate) => ({ ...clone(gate), from: renamed.get(gate.from), to: renamed.get(gate.to) }));
  if (internalGates.length > 0) wf.spec.approvalGates = [...(wf.spec.approvalGates || []), ...internalGates];
  state.selectedSteps = new Set(renamed.values());
  state.selectedEdges = [];
  state.selectedStep = state.selectedSteps.size === 1 ? [...state.selectedSteps][0] : null;
  state.selectedEdge = null;
  saveWorkflowLayout();
  markDirty();
  renderEditor();
}

function disconnectWorkflowSelection() {
  const wf = state.draft;
  if (!wf?.spec) return;
  const selectedSteps = state.selectedSteps;
  const selectedEdges = state.selectedEdges;
  const shouldRemove = (edge) => selectedSteps.has(edge.from) || selectedSteps.has(edge.to)
    || selectedEdges.some((selected) => selected.from === edge.from && selected.to === edge.to);
  const edges = wf.spec.edges || [];
  if (!edges.some(shouldRemove)) return;
  recordWorkflowHistory("disconnect selection");
  wf.spec.edges = edges.filter((edge) => !shouldRemove(edge));
  wf.spec.approvalGates = (wf.spec.approvalGates || []).filter((gate) => !shouldRemove(gate));
  state.selectedEdges = [];
  state.selectedEdge = null;
  markDirty();
  renderEditor();
}

function selectAllWorkflowItems() {
  if (!state.draft?.spec) return;
  state.selectedSteps = new Set((state.draft.spec.steps || []).map((step) => step.name));
  state.selectedEdges = (state.draft.spec.edges || []).map((edge) => ({ from: edge.from, to: edge.to }));
  state.selectedStep = null;
  state.selectedEdge = null;
  applyCanvasSelectionVisuals();
  updateInspectorForSelection();
}

function renderEdges(svg) {
  while (svg.firstChild) svg.removeChild(svg.firstChild);
  const edges = (state.draft.spec.edges || []);
  const gates = (state.draft.spec.approvalGates || []);
  for (let i = 0; i < edges.length; i++) {
    const e = edges[i];
    const from = state.canvasPositions[e.from]; const to = state.canvasPositions[e.to];
    if (!from || !to) continue;
    // approximate node size: width 180, height 65
    const fromX = from.x + 180; const fromY = from.y + 32;
    const toX = to.x; const toY = to.y + 32;
    const midX = (fromX + toX) / 2;
    const d = `M ${fromX} ${fromY} C ${midX} ${fromY}, ${midX} ${toY}, ${toX} ${toY}`;
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", d);
    path.dataset.from = e.from; path.dataset.to = e.to;
    path.tabIndex = 0;
    path.setAttribute("role", "button");
    path.setAttribute("aria-label", `Edge from ${e.from} to ${e.to}`);
    const isApproval = gates.some((g) => g.from === e.from && g.to === e.to);
    if (isApproval) path.classList.add("approval");
    if (state.selectedEdges.some((se) => se.from === e.from && se.to === e.to)) path.classList.add("selected");
    path.style.pointerEvents = "stroke";
    path.addEventListener("mousedown", (ev) => { ev.stopPropagation(); }); // don't start a marquee
    path.addEventListener("click", (ev) => {
      ev.stopPropagation();
      if (ev.shiftKey) toggleEdgeSelection({ from: e.from, to: e.to });
      else selectOnlyEdge({ from: e.from, to: e.to });
    });
    path.addEventListener("keydown", (event) => {
      if (event.key !== "Enter" && event.key !== " ") return;
      event.preventDefault();
      event.stopPropagation();
      if (event.shiftKey) toggleEdgeSelection({ from: e.from, to: e.to });
      else selectOnlyEdge({ from: e.from, to: e.to });
    });
    svg.appendChild(path);
    if (e.condition) {
      const lbl = document.createElementNS("http://www.w3.org/2000/svg", "text");
      lbl.setAttribute("x", midX); lbl.setAttribute("y", (fromY + toY) / 2 - 6);
      lbl.setAttribute("text-anchor", "middle");
      lbl.classList.add("edge-label");
      lbl.textContent = (e.condition.length > 22 ? e.condition.slice(0, 20) + "…" : e.condition);
      svg.appendChild(lbl);
    }
  }
  scheduleWorkflowMinimapContentRefresh();
}

function autoLayout(wf) {
  // Kahn-style BFS layout with cycle protection — workflows are allowed to
  // contain feedback loops (e.g. reviewer → writer revision edges), so we
  // must never infinite-loop on them.
  const steps = (wf.spec && wf.spec.steps) || [];
  const edges = (wf.spec && wf.spec.edges) || [];
  if (steps.length === 0) return {};
  const inDeg = {}; const adj = {};
  for (const s of steps) { inDeg[s.name] = 0; adj[s.name] = []; }
  for (const e of edges) {
    if (adj[e.from] && inDeg[e.to] !== undefined) {
      adj[e.from].push(e.to);
      inDeg[e.to]++;
    }
  }
  const depth = {};
  const queue = [];
  for (const s of steps) if (inDeg[s.name] === 0) { depth[s.name] = 0; queue.push(s.name); }
  // Fallback: if nothing has in-degree 0 (pure cycle), seed with entry point or first step.
  if (queue.length === 0) {
    const seed = (wf.spec && wf.spec.entryPoint) || steps[0].name;
    depth[seed] = 0; queue.push(seed);
  }
  const visited = new Set();
  while (queue.length) {
    const n = queue.shift();
    if (visited.has(n)) continue; // cycle guard — each node enters the depth map once
    visited.add(n);
    for (const m of adj[n] || []) {
      if (visited.has(m)) continue; // skip back-edges to already-placed nodes
      if (depth[m] === undefined || depth[m] < depth[n] + 1) {
        depth[m] = depth[n] + 1;
      }
      queue.push(m);
    }
  }
  // Any leftover nodes (disconnected or unreachable) get stacked at the right edge.
  let maxDepth = 0;
  for (const d of Object.values(depth)) if (d > maxDepth) maxDepth = d;
  for (const s of steps) {
    if (depth[s.name] === undefined) { maxDepth++; depth[s.name] = maxDepth; }
  }
  const cols = {};
  for (const s of steps) {
    const d = depth[s.name] || 0;
    cols[d] = cols[d] || []; cols[d].push(s.name);
  }
  const pos = {};
  Object.keys(cols).sort((a, b) => Number(a) - Number(b)).forEach((d) => {
    const list = cols[d];
    for (let i = 0; i < list.length; i++) {
      pos[list[i]] = { x: 40 + Number(d) * 230, y: 40 + i * 110 };
    }
  });
  return pos;
}

// ----- Team (collaboration) steps ------------------------------------------
// Mirrors the admission rules in internal/admission/workflow_collaboration_validator.go.
const TEAM_STRATEGIES = ["Supervisor", "Parallel", "Handoff", "Peer"];
const TEAM_STRATEGY_BLOCKS = { Supervisor: "supervisor", Parallel: "parallel", Handoff: "handoff", Peer: "peer" };
const TEAM_ALIAS_RE = /^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$/;
const TEAM_MAX_MEMBERS = 8;
const TEAM_MAX_STEP_SECONDS = 24 * 3600;
const TEAM_GUIDANCE_MAX = 2000;
const TEAM_DESCRIPTION_MAX = 500;
const TEAM_LIMIT_MAX = { maxTurns: 100, maxMessages: 500, maxDelegations: 100, maxDelegationDepth: 5, maxParallelTasks: 8 };
const TEAM_STRATEGY_HINTS = {
  Supervisor: "One coordinator delegates work to the other members and produces the final result.",
  Parallel: "Every branch member runs concurrently; an optional synthesizer combines the results.",
  Handoff: "One member owns the task at a time and can transfer ownership to another member.",
  Peer: "Members delegate to each other freely; only the finalizer can complete the step.",
};

function newWorkflowTeam(strategy = "Parallel") {
  const team = {
    strategy, protocol: "A2A",
    members: ["member-1", "member-2"].map((name) => ({ name, agentRef: { name: "" } })),
    limits: { maxTurns: 4, maxMessages: 16, maxDelegations: 1, maxDelegationDepth: 1, maxParallelTasks: 2, maxTokens: 8192 },
  };
  applyTeamStrategy(team, strategy);
  return team;
}

function teamMemberNames(team) {
  return (team.members || []).map((member) => member?.name).filter(Boolean);
}

function setTeamMemberKind(member, external) {
  if (external) {
    delete member.agentRef; delete member.serviceRef;
    member.externalAgentRef = { name: "" };
  } else {
    delete member.externalAgentRef;
    member.agentRef = { name: "" };
  }
}

// Rebuilds the strategy block and interactions for the current members.
function applyTeamStrategy(team, strategy) {
  const names = teamMemberNames(team);
  const [first] = names;
  for (const block of Object.values(TEAM_STRATEGY_BLOCKS)) delete team[block];
  team.strategy = strategy;
  if (strategy === "Parallel") {
    delete team.allowedInteractions;
    team.parallel = { branches: names.map((member) => ({ member })) };
    return;
  }
  const others = names.slice(1);
  if (strategy === "Peer") {
    team.allowedInteractions = names.flatMap((from) => names.filter((to) => to !== from).map((to) => ({ from, to })));
    team.peer = { finalizer: first };
  } else {
    team.allowedInteractions = others.map((to) => ({ from: first, to }));
    if (strategy === "Supervisor") team.supervisor = { coordinator: first };
    else team.handoff = { entryMember: first, maxHandoffs: 1 };
  }
  team.limits.maxDelegations = Math.min(TEAM_LIMIT_MAX.maxDelegations, Math.max(team.limits.maxDelegations || 1, others.length));
}

function addTeamMember(team) {
  let suffix = 1;
  while (team.members.some((member) => member.name === `member-${suffix}`)) suffix++;
  const name = `member-${suffix}`;
  const [first] = teamMemberNames(team);
  team.members.push({ name, agentRef: { name: "" } });
  if (team.strategy === "Parallel") team.parallel.branches.push({ member: name });
  else if (team.strategy === "Peer") team.allowedInteractions.push({ from: first, to: name }, { from: name, to: first });
  else team.allowedInteractions.push({ from: team.supervisor?.coordinator || team.handoff?.entryMember || first, to: name });
  if (team.strategy !== "Parallel") team.limits.maxDelegations = Math.min(TEAM_LIMIT_MAX.maxDelegations, Math.max(team.limits.maxDelegations || 1, team.members.length - 1));
}

function removeTeamMember(team, name) {
  team.members = team.members.filter((member) => member.name !== name);
  const [first] = teamMemberNames(team);
  if (team.parallel) {
    team.parallel.branches = team.parallel.branches.filter((branch) => branch.member !== name);
    if (team.parallel.synthesizer === name) delete team.parallel.synthesizer;
  }
  if (team.allowedInteractions) {
    team.allowedInteractions = team.allowedInteractions.filter((item) => item.from !== name && item.to !== name);
    if (!team.allowedInteractions.length) delete team.allowedInteractions;
  }
  if (team.supervisor?.coordinator === name) team.supervisor.coordinator = first;
  if (team.handoff?.entryMember === name) team.handoff.entryMember = first;
  if (team.peer?.finalizer === name) team.peer.finalizer = first;
}

function renameTeamMember(team, from, to) {
  for (const member of team.members) if (member.name === from) member.name = to;
  for (const branch of team.parallel?.branches || []) if (branch.member === from) branch.member = to;
  for (const item of team.allowedInteractions || []) {
    if (item.from === from) item.from = to;
    if (item.to === from) item.to = to;
  }
  if (team.parallel?.synthesizer === from) team.parallel.synthesizer = to;
  if (team.supervisor?.coordinator === from) team.supervisor.coordinator = to;
  if (team.handoff?.entryMember === from) team.handoff.entryMember = to;
  if (team.peer?.finalizer === from) team.peer.finalizer = to;
}

function goDurationSeconds(text) {
  if (!GO_DURATION_RE.test(text)) return NaN;
  const unit = { ns: 1e-9, us: 1e-6, "µs": 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
  let total = 0;
  for (const [, amount, suffix] of text.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gi)) total += Number(amount) * unit[suffix.toLowerCase()];
  return total;
}

function teamProblems(step, workflow) {
  const problems = [];
  const team = step.collaboration;
  if (!(step.input || "").trim()) problems.push("an input is required, for example {{ .Input }} to pass the run input to the team.");
  if (!step.timeout) problems.push("a timeout is required.");
  else {
    const seconds = goDurationSeconds(step.timeout);
    if (Number.isNaN(seconds)) problems.push(`timeout "${step.timeout}" is not a valid duration.`);
    else if (seconds <= 0 || seconds > TEAM_MAX_STEP_SECONDS) problems.push("timeout must be greater than zero and at most 24h.");
  }
  if (!team || typeof team !== "object") return [...problems, "the team definition is missing."];
  if (!TEAM_STRATEGIES.includes(team.strategy)) problems.push("choose a strategy.");
  const members = Array.isArray(team.members) ? team.members : [];
  if (members.length < 2 || members.length > TEAM_MAX_MEMBERS) problems.push(`needs between 2 and ${TEAM_MAX_MEMBERS} members.`);
  const aliases = new Set();
  const agents = new Map();
  if ([...(team.guidance || "")].length > TEAM_GUIDANCE_MAX) problems.push(`guidance must not exceed ${TEAM_GUIDANCE_MAX} characters.`);
  for (const member of members) {
    const alias = member?.name || "";
    if ([...(member?.description || "")].length > TEAM_DESCRIPTION_MAX) problems.push(`the description of "${alias}" must not exceed ${TEAM_DESCRIPTION_MAX} characters.`);
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f-\u009f]/.test(member?.description || "")) problems.push(`the description of "${alias}" must be a single line without control characters.`);
    if (!TEAM_ALIAS_RE.test(alias)) problems.push(`member name "${alias}" must be a lowercase DNS label of at most 32 characters starting with a letter.`);
    else if (aliases.has(alias)) problems.push(`member name "${alias}" is used more than once.`);
    aliases.add(alias);
    if (member?.externalAgentRef != null) {
      const remote = member.externalAgentRef.name;
      if (member.agentRef?.name) problems.push(`member "${alias}" must set exactly one of an agent and an external agent.`);
      else if (!remote) problems.push(`member "${alias}" needs an external agent.`);
      else if (member.serviceRef?.name) problems.push(`member "${alias}" cannot use an AgentService with an external agent.`);
      else if (agents.has(`external/${remote}`)) problems.push(`members "${agents.get(`external/${remote}`)}" and "${alias}" use the same external agent.`);
      else agents.set(`external/${remote}`, alias);
      continue;
    }
    if (!member?.agentRef?.name) { problems.push(`member "${alias}" needs an agent.`); continue; }
    const key = `${member.agentRef.namespace || workflow.metadata?.namespace || "default"}/${member.agentRef.name}`;
    if (agents.has(key)) problems.push(`members "${agents.get(key)}" and "${alias}" use the same agent.`);
    else agents.set(key, alias);
  }
  const limits = team.limits || {};
  for (const [key, max] of Object.entries(TEAM_LIMIT_MAX)) {
    if (!Number.isInteger(limits[key]) || limits[key] < 1 || limits[key] > max) problems.push(`limits.${key} must be between 1 and ${max}.`);
  }
  if (!Number.isInteger(limits.maxTokens) || limits.maxTokens < 1) problems.push("limits.maxTokens must be greater than zero.");

  const known = (alias) => aliases.has(alias);
  const outgoing = new Map();
  const interactions = Array.isArray(team.allowedInteractions) ? team.allowedInteractions : [];
  if (team.strategy === "Parallel") {
    if (interactions.length) problems.push("Parallel teams do not use allowed interactions.");
  } else {
    if (!interactions.length) problems.push("add at least one allowed interaction.");
    const seen = new Set();
    for (const item of interactions) {
      if (!known(item?.from) || !known(item?.to)) problems.push(`interaction ${item?.from || "?"} → ${item?.to || "?"} references an unknown member.`);
      else if (item.from === item.to) problems.push(`interaction ${item.from} → ${item.to} is a self-interaction.`);
      else {
        const id = `${item.from}\u0000${item.to}`;
        if (seen.has(id)) problems.push(`interaction ${item.from} → ${item.to} is repeated.`);
        seen.add(id);
        outgoing.set(item.from, (outgoing.get(item.from) || 0) + 1);
      }
    }
  }
  const wrongBlocks = TEAM_STRATEGIES.filter((s) => s !== team.strategy && team[TEAM_STRATEGY_BLOCKS[s]] != null);
  if (wrongBlocks.length) problems.push(`remove the ${wrongBlocks.map((s) => TEAM_STRATEGY_BLOCKS[s]).join(", ")} settings; they do not belong to ${team.strategy}.`);
  const requireMember = (label, alias, needsOutgoing) => {
    if (!known(alias)) problems.push(`${label} must be one of the team members.`);
    else if (needsOutgoing && !outgoing.get(alias)) problems.push(`${label} "${alias}" needs at least one allowed interaction from it.`);
  };
  if (team.strategy === "Supervisor") {
    if (!team.supervisor) problems.push("choose a coordinator.");
    else requireMember("the coordinator", team.supervisor.coordinator, true);
  } else if (team.strategy === "Parallel") {
    const branches = team.parallel?.branches;
    if (!Array.isArray(branches) || branches.length < 2 || branches.length > TEAM_LIMIT_MAX.maxParallelTasks) {
      problems.push(`needs between 2 and ${TEAM_LIMIT_MAX.maxParallelTasks} parallel branches.`);
    } else {
      const names = new Set();
      for (const branch of branches) {
        if (!known(branch?.member)) problems.push(`branch "${branch?.member || ""}" must be one of the team members.`);
        else if (names.has(branch.member)) problems.push(`branch "${branch.member}" is repeated.`);
        names.add(branch?.member);
      }
      const synthesizer = team.parallel.synthesizer;
      if (synthesizer && !known(synthesizer)) problems.push("the synthesizer must be one of the team members.");
      else if (synthesizer && names.has(synthesizer)) problems.push("the synthesizer cannot also be a branch.");
    }
    if (!team.parallel) problems.push("add the parallel branches.");
  } else if (team.strategy === "Handoff") {
    if (!team.handoff) problems.push("choose an entry member.");
    else {
      requireMember("the entry member", team.handoff.entryMember, true);
      const max = team.handoff.maxHandoffs;
      if (!Number.isInteger(max) || max < 1 || max > (limits.maxDelegations || 0)) problems.push("maxHandoffs must be between 1 and limits.maxDelegations.");
    }
  } else if (team.strategy === "Peer") {
    if (!team.peer) problems.push("choose a finalizer.");
    else requireMember("the finalizer", team.peer.finalizer, false);
  }
  return problems;
}


function addStep(type) {
  const wf = state.draft;
  recordWorkflowHistory(`add ${type} step`);
  wf.spec.steps = wf.spec.steps || [];
  const base = type === "collaboration" ? "team" : type === "tool" ? "tool" : "step";
  let i = 1;
  while (wf.spec.steps.find((s) => s.name === `${base}-${i}`)) i++;
  const name = `${base}-${i}`;
  const step = type === "collaboration" ? { name, type, input: "{{ .Input }}", timeout: "5m", collaboration: newWorkflowTeam() }
    : type === "tool" ? { name, type, toolRef: { name: "" } } : { name, type, agentRef: { name: "" } };
  wf.spec.steps.push(step);
  const canvas = document.getElementById("wf-canvas");
  const selectedPosition = state.selectedStep && state.canvasPositions[state.selectedStep];
  const x = selectedPosition
    ? selectedPosition.x + 230
    : Math.max(40, ((canvas?.scrollLeft || 0) + (canvas?.clientWidth || 520) / 2) / state.canvasZoom - 90);
  const y = selectedPosition
    ? selectedPosition.y
    : Math.max(40, ((canvas?.scrollTop || 0) + (canvas?.clientHeight || 400) / 2) / state.canvasZoom - 32);
  state.canvasPositions[name] = { x, y };
  saveWorkflowLayout(wf);
  state.selectedSteps.clear();
  state.selectedEdges = [];
  state.selectedSteps.add(name);
  state.selectedStep = name;
  state.selectedEdge = null;
  markDirty();
  renderEditor();
  requestAnimationFrame(() => locateWorkflowSelection("auto"));
}

// ============================================================================
// Inspectors (step / edge)
// ============================================================================
function openInspector() {
  document.querySelector(".layout").classList.add("inspector-open");
}
function closeInspector() {
  document.querySelector(".layout").classList.remove("inspector-open");
  state.selectedStep = null;
  state.selectedEdge = null;
  state.selectedSteps.clear();
  state.selectedEdges = [];
  // unselect in DOM
  document.querySelectorAll(".node.selected, svg.edges path.selected").forEach((n) => n.classList.remove("selected"));
}

// Runs a mutation that rebuilds the inspector, then refocuses the control at the same DOM position.
function keepInspectorFocus(mutate) {
  const controls = () => [...document.querySelectorAll("#inspector-body input, #inspector-body select, #inspector-body textarea, #inspector-body button")];
  const index = controls().indexOf(document.activeElement);
  mutate();
  if (index >= 0) controls()[index]?.focus();
}

// Text input that commits on blur/Enter so renames do not re-render the inspector per keystroke.
function commitTextField(label, value, onCommit, required) {
  const input = document.createElement("input"); input.type = "text"; input.value = value || "";
  // Deferred so focus has already moved to the next control when the inspector is rebuilt.
  input.addEventListener("change", () => {
    const committed = input.value.trim();
    setTimeout(() => keepInspectorFocus(() => onCommit(committed)), 0);
  });
  return field(label, input, { required });
}

function renderWorkflowTeam(host, step) {
  const team = step.collaboration;
  if (!team || team.protocol !== "A2A" || !Array.isArray(team.members) || team.members.some((member) => !member?.name)
      || !team.limits || typeof team.limits !== "object") {
    host.appendChild(previewBanner("info", "This team definition is available in the YAML draft only."));
    return;
  }
  if (state.mode !== "kubernetes") host.appendChild(previewBanner("warn", `${TEAM_UNSUPPORTED_MESSAGE}. Drafts can be edited and exported here.`));
  const edit = (label, mutate, render = false) => editWorkflow(label, mutate, { render });
  host.appendChild(section("Collaboration strategy", "A2A", (body) => {
    body.appendChild(selectField("Strategy", team.strategy || "", [
      ...(TEAM_STRATEGIES.includes(team.strategy) ? [] : [{ value: team.strategy || "", label: team.strategy || "Choose a strategy" }]),
      ...TEAM_STRATEGIES,
    ], (value) => edit("change team strategy", () => { applyTeamStrategy(team, value); }, true)));
    if (TEAM_STRATEGY_HINTS[team.strategy]) {
      const hint = document.createElement("p"); hint.className = "hint"; hint.textContent = TEAM_STRATEGY_HINTS[team.strategy];
      body.appendChild(hint);
    }
  }, true));
  const block = team[TEAM_STRATEGY_BLOCKS[team.strategy]];
  if (!TEAM_STRATEGIES.includes(team.strategy) || !block || typeof block !== "object") {
    host.appendChild(previewBanner("info", "Pick a strategy to generate its settings."));
    return;
  }
  const isParallel = team.strategy === "Parallel";
  if (isParallel && (!Array.isArray(block.branches) || block.branches.some((branch) => !branch?.member))) {
    host.appendChild(previewBanner("info", "This parallel definition is available in the YAML draft only."));
    return;
  }
  const memberNames = teamMemberNames(team);
  host.appendChild(section("Team guidance", `${[...(team.guidance || "")].length} of ${TEAM_GUIDANCE_MAX}`, (body) => {
    body.appendChild(textareaField("Guidance for the team", team.guidance || "",
      (value) => edit("edit team guidance", () => { setOrDel(team, "guidance", value.trim() ? value : undefined); }),
      "For example: ask the analyst to verify the facts, then the reviewer to check tone, before finalizing."));
    const hint = document.createElement("p"); hint.className = "hint";
    hint.textContent = "Sent to every member as trusted instructions on how to resolve the task. It guides the members; it does not change who may call whom.";
    body.appendChild(hint);
  }, true));
  host.appendChild(section("Team members", `${team.members.length} of ${TEAM_MAX_MEMBERS}`, (body) => {
    const workflowNamespace = state.draft.metadata.namespace || "default";
    for (const member of team.members) {
      const branch = isParallel ? team.parallel.branches.find((item) => item.member === member.name) : undefined;
      body.appendChild(commitTextField(`Name of ${member.name}`, member.name, (value) => {
        if (!value || value === member.name || memberNames.includes(value)) { renderEditor(); return; }
        edit("rename team member", () => { renameTeamMember(team, member.name, value); }, true);
      }));
      const external = member.externalAgentRef != null;
      body.appendChild(textField(`Description of ${member.name}`, member.description || "",
        (value) => edit("edit team member description", () => { setOrDel(member, "description", value.trim() ? value : undefined); }),
        false, external ? "Optional; defaults to the skills pinned on the external agent" : "What this member is good at"));
      body.appendChild(selectField(`Kind of ${member.name}`, external ? "external" : "agent", [
        { value: "agent", label: "Agent" }, { value: "external", label: "External agent" },
      ], (value) => edit("change team member kind", () => { setTeamMemberKind(member, value === "external"); }, true)));
      if (external) {
        const remoteLabel = `External agent for ${member.name}`;
        body.appendChild(refSelect(remoteLabel, member.externalAgentRef?.name || "",
          (state.lists.externalagents || []).filter((remote) => (remote.metadata?.namespace || "default") === workflowNamespace),
          (value) => edit("change team external agent", () => { member.externalAgentRef = { name: value }; }), true));
        const hint = document.createElement("p"); hint.className = "hint";
        hint.textContent = "Prompt data is sent to the remote endpoint. Needs an ExternalAgentGrant for this workflow and an operator allow-list entry.";
        body.appendChild(hint);
      }
      const updateAgent = (value) => edit("change team agent", () => { member.agentRef = { ...member.agentRef, name: value }; });
      const agentLabel = `Agent for ${member.name}`;
      if (!external) {
        const memberNamespace = member.agentRef?.namespace || workflowNamespace;
        body.appendChild(memberNamespace === (state.namespace || "default")
          ? refSelect(agentLabel, member.agentRef?.name || "", (state.lists.agents || []).filter(
            (agent) => (agent.metadata?.namespace || state.namespace || "default") === memberNamespace), updateAgent, true)
          : textField(agentLabel, member.agentRef?.name || "", updateAgent, true));
        const namespaces = [...new Set([workflowNamespace, memberNamespace, ...state.namespaces])].filter(Boolean).sort();
        body.appendChild(selectField(`Namespace for ${member.name}`, member.agentRef?.namespace || "", [
          { value: "", label: `Workflow namespace (${workflowNamespace})` }, ...namespaces,
        ], (value) => {
          edit("change team namespace", () => {
            member.agentRef = { ...member.agentRef, namespace: value || undefined };
          }, true);
        }));
        body.appendChild(textField(`AgentService for ${member.name}`, member.serviceRef?.name || "", (value) => {
          edit("change team service", () => { setOrDel(member, "serviceRef", value ? { name: value } : undefined); });
        }));
      }
      if (branch) {
        const label = `Optional branch for ${member.name}`;
        const optional = checkboxField(label, branch.optional, (value) => {
          edit("change optional team branch", () => { branch.optional = value || undefined; });
        });
        optional.querySelector("input").setAttribute("aria-label", label);
        body.appendChild(optional);
      }
      if (team.members.length > 2 && (!branch || team.parallel.branches.length > 2)) {
        const remove = document.createElement("button"); remove.type = "button"; remove.className = "action danger";
        remove.textContent = `Remove ${member.name}`;
        remove.addEventListener("click", () => edit("remove team member", () => { removeTeamMember(team, member.name); }, true));
        body.appendChild(remove);
      }
    }
    const add = document.createElement("button"); add.type = "button"; add.className = "add-row";
    add.textContent = "+ Add member"; add.disabled = team.members.length >= TEAM_MAX_MEMBERS;
    add.addEventListener("click", () => edit("add team member", () => { addTeamMember(team); }, true));
    body.appendChild(add);
  }, true));

  const memberOptions = (allowNone) => [...(allowNone ? [{ value: "", label: "None" }] : []), ...memberNames];
  const roleLabel = { Supervisor: "Coordinator", Handoff: "Entry member", Peer: "Finalizer" }[team.strategy];
  host.appendChild(section(`${team.strategy} settings`, "", (body) => {
    if (team.strategy === "Supervisor") {
      body.appendChild(selectField(roleLabel, block.coordinator || "", memberOptions(false),
        (value) => edit("change team coordinator", () => { team.supervisor.coordinator = value; }), true));
    } else if (team.strategy === "Handoff") {
      body.appendChild(selectField(roleLabel, block.entryMember || "", memberOptions(false),
        (value) => edit("change team entry member", () => { team.handoff.entryMember = value; }), true));
      body.appendChild(numberField("maxHandoffs", block.maxHandoffs, (value) => {
        edit("edit team maxHandoffs", () => { team.handoff.maxHandoffs = value; });
      }, "Between 1 and limits.maxDelegations"));
    } else if (team.strategy === "Peer") {
      body.appendChild(selectField(roleLabel, block.finalizer || "", memberOptions(false),
        (value) => edit("change team finalizer", () => { team.peer.finalizer = value; }), true));
    } else if (team.members.length > 2 || block.synthesizer) {
      body.appendChild(selectField("Synthesizer", block.synthesizer || "", memberOptions(true), (value) => edit("change team synthesizer", () => {
        const previous = team.parallel.synthesizer;
        setOrDel(team.parallel, "synthesizer", value || undefined);
        team.parallel.branches = team.parallel.branches.filter((branch) => branch.member !== value);
        if (previous && previous !== value && !team.parallel.branches.some((branch) => branch.member === previous)) {
          team.parallel.branches.push({ member: previous });
        }
      }, true)));
    } else {
      const note = document.createElement("div"); note.className = "hint";
      note.textContent = "Every member runs as a branch. Add a third member to designate a synthesizer.";
      body.appendChild(note);
    }
  }, true));

  if (!isParallel) {
    host.appendChild(section("Allowed interactions", "Which member may delegate to which. Only these A2A calls are admitted.", (body) => {
      (team.allowedInteractions || []).forEach((item, index) => {
        const row = document.createElement("div"); row.className = "field-grid";
        row.appendChild(selectField(`Interaction ${index + 1} from`, item.from || "", ["", ...memberNames],
          (value) => edit("change team interaction", () => { item.from = value; })));
        row.appendChild(selectField(`Interaction ${index + 1} to`, item.to || "", ["", ...memberNames],
          (value) => edit("change team interaction", () => { item.to = value; })));
        body.appendChild(row);
        const remove = document.createElement("button"); remove.type = "button"; remove.className = "action danger";
        remove.textContent = `Remove interaction ${index + 1}`;
        remove.addEventListener("click", () => edit("remove team interaction", () => {
          team.allowedInteractions.splice(index, 1);
        }, true));
        body.appendChild(remove);
      });
      const add = document.createElement("button"); add.type = "button"; add.className = "add-row";
      add.textContent = "+ Add interaction"; add.disabled = (team.allowedInteractions || []).length >= TEAM_MAX_MEMBERS * (TEAM_MAX_MEMBERS - 1);
      add.addEventListener("click", () => edit("add team interaction", () => {
        const existing = new Set((team.allowedInteractions || []).map((item) => `${item.from}\u0000${item.to}`));
        const pair = memberNames.flatMap((from) => memberNames.filter((to) => to !== from).map((to) => ({ from, to })))
          .find((candidate) => !existing.has(`${candidate.from}\u0000${candidate.to}`)) || { from: "", to: "" };
        (team.allowedInteractions ||= []).push(pair);
      }, true));
      body.appendChild(add);
    }, true));
  }

  const limitHints = { ...Object.fromEntries(Object.entries(TEAM_LIMIT_MAX).map(([key, max]) => [key, `1–${max}`])), maxTokens: "Token allowance for the whole team; at least 1" };
  host.appendChild(section("Team limits", "Hard bounds enforced while the team runs.", (body) => {
    for (const key of ["maxTurns", "maxMessages", "maxDelegations", "maxDelegationDepth", "maxParallelTasks", "maxTokens"]) {
      body.appendChild(numberField(key, team.limits[key], (value) => {
        edit(`edit team ${key}`, () => { team.limits[key] = value; });
      }, limitHints[key]));
    }
  }, true));
}

function renderStepInspector(name) {
  const wf = state.draft;
  const step = (wf.spec.steps || []).find((s) => s.name === name);
  if (!step) { closeInspector(); return; }
  openInspector();
  document.getElementById("inspector-title").textContent = `Step: ${name}`;
  const host = document.getElementById("inspector-body");
  host.innerHTML = "";

  host.appendChild(commitTextField("Name", step.name, (v) => {
    if (!v || v === step.name) return;
    editWorkflow("rename step", () => {
      const old = step.name;
      step.name = v;
      state.canvasPositions[v] = state.canvasPositions[old];
      delete state.canvasPositions[old];
      for (const edge of wf.spec.edges || []) {
        if (edge.from === old) edge.from = v;
        if (edge.to === old) edge.to = v;
      }
      for (const gate of wf.spec.approvalGates || []) {
        if (gate.from === old) gate.from = v;
        if (gate.to === old) gate.to = v;
      }
      if (wf.spec.entryPoint === old) wf.spec.entryPoint = v;
      if (state.selectedSteps.has(old)) { state.selectedSteps.delete(old); state.selectedSteps.add(v); }
      for (const selectedEdge of state.selectedEdges) {
        if (selectedEdge.from === old) selectedEdge.from = v;
        if (selectedEdge.to === old) selectedEdge.to = v;
      }
      state.selectedStep = v;
    }, { coalesceKey: `step:${name}:name`, saveLayout: true, render: true });
  }, true));

  host.appendChild(selectField("Type", step.type || "agent", [
    { value: "agent", label: "agent" }, { value: "tool", label: "tool" },
    { value: "collaboration", label: "Team" },
  ], (v) => {
    editWorkflow("change step type", () => {
      step.type = v;
      if (v !== "collaboration") delete step.collaboration;
      if (v === "tool") { delete step.agentRef; step.toolRef = step.toolRef || { name: "" }; }
      else if (v === "collaboration") {
        delete step.agentRef;
        delete step.toolRef;
        delete step.retry;
        step.timeout ||= "5m";
        step.input ||= "{{ .Input }}";
        step.collaboration ||= newWorkflowTeam();
      }
      else { delete step.toolRef; step.agentRef = step.agentRef || { name: "" }; }
    }, { render: true });
  }));

  if (step.type === "tool") {
    host.appendChild(refSelect("Tool", step.toolRef?.name || "", state.lists.tools || [], (v) => {
      editWorkflow("change step tool", () => { step.toolRef = { name: v }; }, { render: true });
    }, true));
  } else if (step.type === "collaboration") {
    renderWorkflowTeam(host, step);
  } else {
    host.appendChild(refSelect("Agent", step.agentRef?.name || "", state.lists.agents || [], (v) => {
      editWorkflow("change step agent", () => { step.agentRef = { name: v }; }, { render: true });
    }, true));
  }

  host.appendChild(textareaField("Input (Go template)", step.input || "", (v) => {
    editWorkflow("edit step input", () => { step.input = v || undefined; }, { coalesceKey: `step:${name}:input` });
  }, "{{.Steps.foo.Output}}"));

  host.appendChild(textField("Timeout", step.timeout || "", (v) => {
    editWorkflow("edit step timeout", () => { step.timeout = v || undefined; }, { coalesceKey: `step:${name}:timeout` });
  }, false, "60s"));
  host.appendChild(textField("Condition (CEL)", step.condition || "", (v) => {
    editWorkflow("edit step condition", () => { step.condition = v || undefined; }, { coalesceKey: `step:${name}:condition` });
  }));
  host.appendChild(checkboxField("Continue on error", step.continueOnError, (v) => {
    editWorkflow("toggle continue on error", () => { step.continueOnError = v || undefined; });
  }));

  // Retry
  const retrySec = document.createElement("details"); retrySec.className = "form-section";
  retrySec.style.marginTop = "0.6rem";
  const rSum = document.createElement("summary"); rSum.textContent = "Retry policy";
  retrySec.appendChild(rSum);
  const rBody = document.createElement("div"); rBody.className = "section-body";
  const retry = step.retry || {};
  const editRetry = (fieldName, value) => editWorkflow(`edit retry ${fieldName}`, () => {
    step.retry = step.retry || {};
    setOrDel(step.retry, fieldName, value);
  }, { coalesceKey: `step:${name}:retry:${fieldName}` });
  rBody.appendChild(numberField("MaxAttempts", retry.maxAttempts, (v) => editRetry("maxAttempts", v)));
  rBody.appendChild(textField("Backoff", retry.backoff || "", (v) => editRetry("backoff", v || undefined), false, "1s"));
  rBody.appendChild(numberField("BackoffMultiplier", retry.backoffMultiplier, (v) => editRetry("backoffMultiplier", v)));
  rBody.appendChild(textField("MaxBackoff", retry.maxBackoff || "", (v) => editRetry("maxBackoff", v || undefined), false, "30s"));
  retrySec.appendChild(rBody);
  if (step.type !== "collaboration") host.appendChild(retrySec);

  // Memory claim override
  const memSec = document.createElement("details"); memSec.className = "form-section";
  memSec.style.marginTop = "0.6rem";
  const mSum = document.createElement("summary"); mSum.textContent = "Memory claim override";
  memSec.appendChild(mSum);
  const mBody = document.createElement("div"); mBody.className = "section-body";
  const memoryClaim = step.memoryClaim || {};
  mBody.appendChild(refSelect("MemoryClass", memoryClaim.memoryClassName || "", state.lists.memoryclasses || [], (v) => {
    editWorkflow("change step memory class", () => {
      setWorkflowMemoryOption(step, "memoryClassName", v);
    });
  }));
  mBody.appendChild(selectField("AccessMode", memoryClaim.accessMode || "", [
    { value: "", label: "— inherit —" }, { value: "ReadOnly", label: "ReadOnly" }, { value: "ReadWrite", label: "ReadWrite" }, { value: "WriteOnly", label: "WriteOnly" },
  ], (v) => {
    editWorkflow("change step memory access", () => {
      setWorkflowMemoryOption(step, "accessMode", v);
    });
  }));
  memSec.appendChild(mBody);
  host.appendChild(memSec);

  // Set as entry
  const setEntry = document.createElement("button");
  setEntry.className = "action"; setEntry.style.marginRight = "0.4rem";
  setEntry.textContent = (wf.spec.entryPoint === name ? "✓ Entry point" : "Set as entry point");
  setEntry.addEventListener("click", () => {
    editWorkflow("change entry point", () => {
      wf.spec.entryPoint = (wf.spec.entryPoint === name ? undefined : name);
    }, { render: true });
  });
  host.appendChild(setEntry);

  // Delete step
  const del = document.createElement("button"); del.className = "action danger"; del.textContent = "Delete step";
  del.addEventListener("click", () => {
    if (!confirm(`Delete step "${name}"?`)) return;
    editWorkflow("delete step", () => {
      wf.spec.steps = (wf.spec.steps || []).filter((candidate) => candidate.name !== name);
      wf.spec.edges = (wf.spec.edges || []).filter((edge) => edge.from !== name && edge.to !== name);
      wf.spec.approvalGates = (wf.spec.approvalGates || []).filter((gate) => gate.from !== name && gate.to !== name);
      if (wf.spec.entryPoint === name) wf.spec.entryPoint = undefined;
      delete state.canvasPositions[name];
      closeInspector();
    }, { saveLayout: true, render: true });
  });
  host.appendChild(del);
}

function renderEdgeInspector(edge) {
  const wf = state.draft;
  const e = (wf.spec.edges || []).find((x) => x.from === edge.from && x.to === edge.to);
  if (!e) { closeInspector(); return; }
  openInspector();
  document.getElementById("inspector-title").textContent = `Edge: ${edge.from} → ${edge.to}`;
  const host = document.getElementById("inspector-body");
  host.innerHTML = "";

  const stepOptions = (wf.spec.steps || []).map((step) => ({ value: step.name, label: step.name }));
  const changeEndpoint = (endpoint, value) => {
    if (!value || value === e[endpoint]) return;
    editWorkflow(`change edge ${endpoint}`, () => {
      const oldFrom = e.from;
      const oldTo = e.to;
      e[endpoint] = value;
      for (const gate of wf.spec.approvalGates || []) {
        if (gate.from === oldFrom && gate.to === oldTo) {
          gate.from = e.from;
          gate.to = e.to;
        }
      }
      state.selectedEdges = [{ from: e.from, to: e.to }];
      state.selectedEdge = { from: e.from, to: e.to };
    }, { render: true });
  };
  host.appendChild(selectField("From", e.from, stepOptions, (v) => changeEndpoint("from", v), true));
  host.appendChild(selectField("To", e.to, stepOptions, (v) => changeEndpoint("to", v), true));
  host.appendChild(textareaField("Condition (CEL)", e.condition || "", (v) => {
    editWorkflow("edit edge condition", () => { e.condition = v || undefined; }, {
      coalesceKey: `edge:${edge.from}:${edge.to}:condition`,
    });
  }, "e.g. steps.reviewer.output.contains('approved')"));

  // Toggle: approval gate?
  const hasGate = (wf.spec.approvalGates || []).some((g) => g.from === e.from && g.to === e.to);
  host.appendChild(checkboxField("Require approval (creates an approval gate)", hasGate, (v) => {
    editWorkflow("toggle edge approval", () => {
      wf.spec.approvalGates = wf.spec.approvalGates || [];
      if (v) {
        if (!wf.spec.approvalGates.find((gate) => gate.from === e.from && gate.to === e.to)) {
          wf.spec.approvalGates.push({ from: e.from, to: e.to, requiredApprovers: [] });
        }
      } else {
        wf.spec.approvalGates = wf.spec.approvalGates.filter((gate) => !(gate.from === e.from && gate.to === e.to));
      }
    }, { render: true });
  }));

  const del = document.createElement("button"); del.className = "action danger"; del.textContent = "Delete edge";
  del.style.marginTop = "0.6rem";
  del.addEventListener("click", () => {
    editWorkflow("delete edge", () => {
      wf.spec.edges = (wf.spec.edges || []).filter((candidate) => !(candidate.from === edge.from && candidate.to === edge.to));
      wf.spec.approvalGates = (wf.spec.approvalGates || []).filter((gate) => !(gate.from === edge.from && gate.to === edge.to));
      closeInspector();
    }, { render: true });
  });
  host.appendChild(del);
}

// ============================================================================
// Preview tab — chat with agents, run workflows
// ============================================================================
function previewKey() {
  if (!state.draft) return "";
  const ns = state.draft.metadata.namespace || "default";
  const name = state.draft.metadata.name || "";
  return `${state.draft.kind}/${ns}/${name}`;
}

function newConversationID() {
  // Random-ish id that's stable for the lifetime of a chat tab. The runtime
  // uses this to look up prior turns for multi-turn memory.
  const rand = Math.random().toString(36).slice(2, 10);
  return `studio-conv-${Date.now().toString(36)}-${rand}`;
}

function renderPreview(host) {
  host.innerHTML = "";
  if (!state.draft) return;
  const kind = state.draft.kind;
  if (teamBlocked(state.draft)) {
    host.appendChild(previewBanner("warn", `${TEAM_UNSUPPORTED_MESSAGE}.`));
    return;
  }
  if (kind !== "Agent" && kind !== "Workflow" && kind !== "Tool" && kind !== "Evaluation") {
    host.appendChild(previewBanner("info", `Preview is available for Agents, Workflows, Tools, and Evaluations. ${escapeHtml(kind)} can't be invoked.`));
    return;
  }
  if (!state.existsInCluster) {
    host.appendChild(previewBanner("warn", "This draft hasn't been deployed yet. Click Deploy to create it in the cluster, then come back to test it here."));
    return;
  }
  if (state.dirty) {
    host.appendChild(previewBanner("warn", "You have unsaved changes. The preview talks to the deployed version — deploy again to test the latest edits."));
  }
  if (kind === "Agent") renderAgentChat(host);
  else if (kind === "Workflow") renderWorkflowRunner(host);
  else if (kind === "Evaluation") renderEvaluationRunner(host);
  else renderToolTester(host);
}

function previewBanner(kind, text) {
  const div = document.createElement("div");
  div.className = `preview-banner ${kind}`;
  div.innerHTML = text;
  return div;
}

// ----- Agent chat ---------------------------------------------------------
function renderAgentChat(host) {
  const key = previewKey();
  const name = state.draft.metadata.name;
  const ns = state.draft.metadata.namespace || "default";
  const chat = state.preview.chats[key] || (state.preview.chats[key] = {
    sessionID: "",
    conversationID: newConversationID(),
    history: [],
    pending: false,
  });
  // Older preview sessions stored before the conversation-id rollout will not
  // have one yet — mint it lazily so existing tabs upgrade to multi-turn.
  if (!chat.conversationID) chat.conversationID = newConversationID();

  const wrap = document.createElement("div"); wrap.className = "chat-panel";

  const header = document.createElement("div"); header.className = "chat-header";
  const title = document.createElement("div"); title.className = "chat-title";
  const turnCount = chat.history.filter((m) => m.role === "user").length;
  title.innerHTML = `Chat with <strong>${escapeHtml(name)}</strong> <span class="chat-session">${turnCount === 0 ? "new conversation" : turnCount + " turn" + (turnCount === 1 ? "" : "s")}</span>`;
  const clear = document.createElement("button"); clear.className = "action"; clear.textContent = "Clear";
  clear.addEventListener("click", () => {
    state.preview.chats[key] = {
      sessionID: "",
      conversationID: newConversationID(),
      history: [],
      pending: false,
    };
    renderPreview(host.parentElement ? host : document.getElementById("preview-host"));
  });
  header.appendChild(title); header.appendChild(clear);
  wrap.appendChild(header);

  const history = document.createElement("div"); history.className = "chat-history";
  if (chat.history.length === 0) {
    const empty = document.createElement("div"); empty.className = "chat-empty";
    empty.textContent = "Say hi to start the conversation.";
    history.appendChild(empty);
  }
  for (const msg of chat.history) history.appendChild(renderChatMessage(msg));
  if (chat.pending) {
    const typing = document.createElement("div"); typing.className = "chat-message assistant";
    typing.innerHTML = `<div class="chat-bubble typing"><span></span><span></span><span></span></div>`;
    history.appendChild(typing);
  }
  wrap.appendChild(history);

  const inputRow = document.createElement("form"); inputRow.className = "chat-input-row";
  const ta = document.createElement("textarea");
  ta.placeholder = "Type a message… (Enter to send, Shift+Enter for newline)";
  ta.rows = 2;
  ta.disabled = chat.pending;
  const send = document.createElement("button"); send.type = "submit"; send.className = "action primary";
  send.textContent = chat.pending ? "Sending…" : "Send";
  send.disabled = chat.pending;
  inputRow.appendChild(ta); inputRow.appendChild(send);
  wrap.appendChild(inputRow);

  ta.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); inputRow.requestSubmit(); }
  });
  inputRow.addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = ta.value.trim();
    if (!text || chat.pending) return;
    chat.history.push({ role: "user", content: text });
    chat.pending = true;
    renderPreview(document.getElementById("preview-host"));
    try {
      const resp = await apiInvokeAgent(name, {
        message: text,
        sessionID: chat.sessionID || undefined,
        conversationID: chat.conversationID,
      }, ns);
      chat.pending = false;
      if (resp.sessionID) chat.sessionID = resp.sessionID;
      if (resp.conversationID) chat.conversationID = resp.conversationID;
      chat.history.push({
        role: "assistant",
        content: resp.response || "(empty response)",
        toolCalls: resp.toolCalls || 0,
        toolCallsDetail: Array.isArray(resp.toolCallsDetail) ? resp.toolCallsDetail : [],
        usage: resp.usage,
      });
    } catch (err) {
      chat.pending = false;
      chat.history.push({ role: "assistant", error: true, content: String(err.message || err) });
    }
    renderPreview(document.getElementById("preview-host"));
    // Re-focus the textarea after re-render.
    const newTa = document.querySelector(".chat-input-row textarea");
    if (newTa) newTa.focus();
  });

  host.appendChild(wrap);

  // Auto-scroll history to bottom and focus input.
  requestAnimationFrame(() => {
    history.scrollTop = history.scrollHeight;
    if (!chat.pending) ta.focus();
  });
}

function renderChatMessage(msg) {
  const row = document.createElement("div");
  row.className = `chat-message ${msg.role}`;
  const bubble = document.createElement("div");
  bubble.className = `chat-bubble${msg.error ? " error" : ""}`;
  // Assistant replies often use markdown (headings, lists, code blocks).
  // User messages and error bubbles stay as plain text to avoid surprises.
  if (msg.role === "assistant" && !msg.error) {
    bubble.innerHTML = renderMarkdown(msg.content || "");
  } else {
    bubble.textContent = msg.content || "";
  }
  row.appendChild(bubble);
  const meta = [];
  // Per-turn detail list is the single source of truth. msg.toolCalls is a
  // cumulative pod-lifetime counter and must not be used for the badge.
  const detail = Array.isArray(msg.toolCallsDetail) ? msg.toolCallsDetail : [];
  const count = detail.length;
  if (count) {
    const m = document.createElement("div"); m.className = "chat-meta";
    const label = `${count} tool call${count === 1 ? "" : "s"}`;
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "tool-call-badge";
    btn.textContent = label;
    btn.title = "Show tool call details";
    btn.addEventListener("click", () => openToolCallsModal(detail));
    m.appendChild(btn);
    if (msg.usage && msg.usage.totalTokens) {
      m.appendChild(document.createTextNode(` · ${msg.usage.totalTokens} tokens`));
    }
    row.appendChild(m);
  } else if (msg.usage && msg.usage.totalTokens) {
    const m = document.createElement("div"); m.className = "chat-meta";
    m.textContent = `${msg.usage.totalTokens} tokens`;
    row.appendChild(m);
  }
  return row;
}

// ----- Tool tester --------------------------------------------------------
function defaultTesterState() {
  return {
    input: "{}",
    formValues: {},
    mode: "",           // "form" | "raw" — resolved after first schema fetch
    schema: null,
    schemaSource: "",    // "live" | "declared" | "none"
    schemaDescription: "",
    schemaWarning: "",
    schemaError: "",
    schemaLoading: false,
    schemaLoaded: false,
    output: null,
    error: null,
    errorCode: null,
    retryable: undefined,
    durationMs: null,
    executionMode: "",
    protocol: "",
    pending: false,
  };
}

function renderToolTester(host) {
  const key = previewKey();
  const name = state.draft.metadata.name;
  const ns = state.draft.metadata.namespace || "default";
  const spec = state.draft.spec || {};
  const proto = spec.protocol || "MCP";
  const t = state.preview.tools[key] || (state.preview.tools[key] = defaultTesterState());

  const wrap = document.createElement("div"); wrap.className = "tool-tester";

  // Protocol gate: the backend only supports MCP and OpenAPI. Native/browser/
  // shell tools still deploy but there's no in-cluster executor for them.
  if (proto !== "MCP" && proto !== "OpenAPI") {
    wrap.appendChild(previewBanner("info", `Studio can't test <strong>${escapeHtml(proto)}</strong> tools. Only MCP and OpenAPI have a controller-side executor. Wire this tool up through an Agent to test it end-to-end.`));
    host.appendChild(wrap);
    return;
  }

  // Human-approval builtins need an ApprovalBroker to be threaded in — the
  // studio invocation path doesn't have one, so we refuse cleanly rather than
  // letting the executor return a cryptic error.
  const meta = spec.metadata || {};
  const isBuiltinApproval = meta.builtin === "human-approval" || (spec.endpoint || "").startsWith("builtin://");
  if (isBuiltinApproval) {
    wrap.appendChild(previewBanner("info", "<strong>Human-approval</strong> builtin tools need an ApprovalBroker. Test them via an Agent or Workflow that carries the broker context."));
    host.appendChild(wrap);
    return;
  }

  // Kick off schema fetch on first render. We only try once per key; the
  // Refresh button on the panel lets operators re-introspect explicitly.
  if (!t.schemaLoaded && !t.schemaLoading && name) {
    t.schemaLoading = true;
    apiToolSchema(name, ns).then((resp) => {
      t.schema = resp && resp.inputSchema ? resp.inputSchema : null;
      t.schemaSource = resp && resp.source ? resp.source : (t.schema ? "declared" : "none");
      t.schemaDescription = resp && resp.description ? resp.description : "";
      t.schemaWarning = resp && resp.warning ? resp.warning : "";
      t.schemaError = "";
      if (t.schema) seedValuesFromSchema(t.schema, t.formValues);
      // Prefer form mode when we got usable properties.
      if (!t.mode) t.mode = (t.schema && t.schema.properties && Object.keys(t.schema.properties).length > 0) ? "form" : "raw";
    }).catch((err) => {
      t.schema = null; t.schemaSource = "none"; t.schemaError = err.message || String(err);
      if (!t.mode) t.mode = "raw";
    }).finally(() => {
      t.schemaLoading = false;
      t.schemaLoaded = true;
      renderPreview(document.getElementById("preview-host"));
    });
  }
  if (!t.mode) t.mode = t.schemaLoading ? "form" : "raw";

  // Header
  const header = document.createElement("div"); header.className = "chat-header";
  const title = document.createElement("div"); title.className = "chat-title";
  title.innerHTML = `Invoke <strong>${escapeHtml(name)}</strong> <span class="chat-session">${proto}</span>`;
  const headerActions = document.createElement("div"); headerActions.className = "run-actions";
  const refresh = document.createElement("button"); refresh.className = "action"; refresh.textContent = t.schemaLoading ? "Loading…" : "Refresh schema";
  refresh.disabled = t.schemaLoading || !name;
  refresh.addEventListener("click", () => {
    t.schemaLoaded = false; t.schemaError = ""; t.schemaWarning = "";
    renderPreview(document.getElementById("preview-host"));
  });
  const clear = document.createElement("button"); clear.className = "action"; clear.textContent = "Clear";
  clear.addEventListener("click", () => {
    // Reset invocation state but keep the fetched schema so we don't refetch.
    t.input = "{}"; t.formValues = {}; t.output = null; t.error = null; t.errorCode = null;
    t.retryable = undefined; t.durationMs = null; t.executionMode = ""; t.protocol = ""; t.pending = false;
    if (t.schema) seedValuesFromSchema(t.schema, t.formValues);
    renderPreview(document.getElementById("preview-host"));
  });
  headerActions.appendChild(refresh); headerActions.appendChild(clear);
  header.appendChild(title); header.appendChild(headerActions);
  wrap.appendChild(header);

  // Schema meta line
  if (t.schemaLoading) {
    const line = document.createElement("div"); line.className = "chat-meta"; line.textContent = "Fetching schema…";
    wrap.appendChild(line);
  } else if (t.schemaError) {
    wrap.appendChild(previewBanner("warn", `Schema fetch failed: ${escapeHtml(t.schemaError)} — falling back to raw JSON.`));
  } else if (t.schemaSource) {
    const line = document.createElement("div"); line.className = "chat-meta";
    const sourceLabel = t.schemaSource === "live" ? "live from endpoint" : (t.schemaSource === "declared" ? "declared on CR" : "no schema");
    line.innerHTML = `Schema: <span class="schema-source-${escapeHtml(t.schemaSource)}">${escapeHtml(sourceLabel)}</span>` + (t.schemaDescription ? ` · ${escapeHtml(t.schemaDescription)}` : "");
    wrap.appendChild(line);
    if (t.schemaWarning) wrap.appendChild(previewBanner("warn", escapeHtml(t.schemaWarning)));
  }

  // Mode toggle
  const hasForm = t.schema && t.schema.properties && Object.keys(t.schema.properties).length > 0;
  const toggle = document.createElement("div"); toggle.className = "mode-toggle";
  const btnForm = document.createElement("button"); btnForm.type = "button"; btnForm.textContent = "Form";
  btnForm.className = t.mode === "form" ? "active" : ""; btnForm.disabled = !hasForm;
  btnForm.addEventListener("click", () => {
    if (!hasForm) return;
    // Sync raw JSON → formValues when switching in, only if raw parses cleanly.
    if (t.mode === "raw") {
      try {
        const parsed = JSON.parse((t.input || "").trim() || "{}");
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) t.formValues = parsed;
      } catch (_) { /* ignore, keep current formValues */ }
    }
    t.mode = "form"; renderPreview(document.getElementById("preview-host"));
  });
  const btnRaw = document.createElement("button"); btnRaw.type = "button"; btnRaw.textContent = "Raw JSON";
  btnRaw.className = t.mode === "raw" ? "active" : "";
  btnRaw.addEventListener("click", () => {
    // Sync formValues → raw JSON when switching out so operators see current state.
    if (t.mode === "form") t.input = JSON.stringify(t.formValues || {}, null, 2);
    t.mode = "raw"; renderPreview(document.getElementById("preview-host"));
  });
  toggle.appendChild(btnForm); toggle.appendChild(btnRaw);
  wrap.appendChild(toggle);

  // Input form
  const form = document.createElement("form"); form.className = "run-form";
  let ta = null;
  if (t.mode === "form" && hasForm) {
    form.appendChild(renderSchemaForm(t.schema, t.formValues, (vals) => { t.formValues = vals; }));
  } else {
    const label = document.createElement("label"); label.textContent = "Input JSON"; form.appendChild(label);
    ta = document.createElement("textarea");
    ta.className = "run-input"; ta.rows = 8; ta.spellcheck = false;
    ta.value = t.input; ta.placeholder = '{ "name": "world" }'; ta.disabled = t.pending;
    ta.addEventListener("input", () => { t.input = ta.value; });
    form.appendChild(ta);
  }

  const actions = document.createElement("div"); actions.className = "run-actions";
  const btn = document.createElement("button");
  btn.type = "submit"; btn.className = "action primary";
  btn.textContent = t.pending ? "Invoking…" : "Invoke";
  btn.disabled = t.pending;
  actions.appendChild(btn);
  form.appendChild(actions);
  wrap.appendChild(form);

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (t.pending) return;
    let parsed;
    if (t.mode === "form" && hasForm) {
      parsed = t.formValues || {};
    } else {
      try {
        const trimmed = (t.input || "").trim();
        parsed = trimmed === "" ? {} : JSON.parse(trimmed);
        if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
          throw new Error("Input must be a JSON object.");
        }
      } catch (err) {
        t.error = "Input JSON parse error: " + err.message;
        t.output = null; t.errorCode = null; t.retryable = undefined;
        t.durationMs = null; t.executionMode = ""; t.protocol = "";
        renderPreview(document.getElementById("preview-host"));
        return;
      }
    }
    t.pending = true; t.error = null; t.output = null; t.errorCode = null; t.retryable = undefined;
    t.durationMs = null; t.executionMode = ""; t.protocol = "";
    renderPreview(document.getElementById("preview-host"));
    try {
      const resp = await apiInvokeTool(name, { input: parsed }, ns);
      t.output = resp.output === undefined ? null : resp.output;
      t.durationMs = typeof resp.durationMs === "number" ? resp.durationMs : null;
      t.executionMode = resp.executionMode || "";
      t.protocol = resp.protocol || "";
    } catch (err) {
      t.error = err.message || String(err);
      const d = err.detail || {};
      t.errorCode = d.code || null;
      t.retryable = (typeof d.retryable === "boolean") ? d.retryable : undefined;
      t.durationMs = typeof d.durationMs === "number" ? d.durationMs : null;
      t.protocol = d.protocol || "";
    } finally {
      t.pending = false;
      renderPreview(document.getElementById("preview-host"));
    }
  });

  // Result meta line
  if (t.protocol || t.executionMode || (t.durationMs !== null && t.durationMs !== undefined)) {
    const metaLine = document.createElement("div"); metaLine.className = "chat-meta";
    const parts = [];
    if (t.protocol) parts.push(t.protocol);
    if (t.executionMode) parts.push(t.executionMode);
    if (t.durationMs !== null && t.durationMs !== undefined) parts.push(t.durationMs + " ms");
    metaLine.textContent = parts.join(" · ");
    wrap.appendChild(metaLine);
  }

  // Error banner
  if (t.error) {
    const bits = [`<strong>Invocation failed</strong>: ${escapeHtml(t.error)}`];
    if (t.errorCode) bits.push(`<code>${escapeHtml(t.errorCode)}</code>`);
    if (t.retryable !== undefined) bits.push(`retryable: ${t.retryable}`);
    wrap.appendChild(previewBanner("bad", bits.join(" · ")));
  }

  // Output panel
  if (t.output !== null && t.output !== undefined) {
    const outWrap = document.createElement("div"); outWrap.className = "tool-tester-output";
    const h = document.createElement("h4"); h.textContent = "Output"; outWrap.appendChild(h);
    const pre = document.createElement("pre");
    if (typeof t.output === "string") pre.textContent = t.output;
    else pre.textContent = JSON.stringify(t.output, null, 2);
    outWrap.appendChild(pre);
    wrap.appendChild(outWrap);
  }

  host.appendChild(wrap);

  // Focus the raw textarea after render for keyboard-driven workflows.
  if (ta) requestAnimationFrame(() => { if (!t.pending) ta.focus(); });
}

// Single Schema field for the Tool Builder Schema section. Renders a
// read-only JSON view of what the endpoint currently advertises, with an
// Introspect/Refresh button on the label row. The value is bound to
// spec.schema so it persists on save, but the operator can't hand-edit —
// changing the box wouldn't change the tool's actual behavior.
function renderBuilderSchemaField(spec, setOrDel) {
  const key = previewKey();
  state.preview.builderSchemas = state.preview.builderSchemas || {};
  const cache = state.preview.builderSchemas[key] || (state.preview.builderSchemas[key] = { schema: null, raw: null, source: "", description: "", warning: "", error: "", loading: false, fetchedAt: 0 });
  const proto = spec.protocol || "MCP";
  const draftName = state.draft.metadata.name;
  const draftNs = state.draft.metadata.namespace || "default";
  const canFetch = Boolean(state.existsInCluster && draftName && spec.endpoint &&
    state.draftKey === keyOf("tools", draftName, draftNs));
  const required = proto === "OpenAPI";

  const runFetch = (adoptSchema) => {
    cache.loading = true; cache.error = "";
    renderEditor();
    apiToolSchema(draftName, draftNs).then((resp) => {
      cache.schema = resp && resp.inputSchema ? resp.inputSchema : null;
      cache.raw = resp && resp.raw ? resp.raw : null;
      cache.source = resp && resp.source ? resp.source : (cache.schema ? "declared" : "none");
      cache.description = resp && resp.description ? resp.description : "";
      cache.warning = resp && resp.warning ? resp.warning : "";
      cache.fetchedAt = Date.now();
      // Mirror the introspected schema into spec.schema so the saved CR
      // reflects what the endpoint advertises. Only mark dirty when the
      // value actually changes — otherwise merely opening a deployed tool
      // in the builder would light up the "unsaved changes" banner.
      if (adoptSchema && cache.schema) {
        const next = JSON.parse(JSON.stringify(cache.schema));
        if (JSON.stringify(spec.schema) !== JSON.stringify(next)) {
          spec.schema = next;
          markDirty();
        }
      }
    }).catch((err) => {
      cache.schema = null; cache.raw = null; cache.source = "none"; cache.error = err.message || String(err);
      cache.fetchedAt = Date.now();
    }).finally(() => {
      cache.loading = false; renderEditor();
    });
  };

  // Auto-fetch on first render when we have enough context and haven't tried
  // yet. Deferred so we don't mutate state / re-render during the current
  // render cycle. Errors don't auto-retry; the user hits Refresh.
  if (canFetch && !cache.loading && cache.fetchedAt === 0 && !cache.error) {
    setTimeout(() => runFetch(false), 0);
  }

  const wrap = document.createElement("div"); wrap.className = "field";

  // Label + Introspect/Refresh button on the same row.
  const head = document.createElement("div"); head.className = "schema-field-head";
  const lab = document.createElement("label");
  lab.textContent = "Schema";
  if (required) { const r = document.createElement("span"); r.className = "req"; r.textContent = "*"; lab.appendChild(r); }
  head.appendChild(lab);
  const btn = document.createElement("button"); btn.type = "button"; btn.className = "action";
  btn.textContent = cache.loading ? "Fetching…" : (cache.fetchedAt > 0 ? "Refresh" : "Introspect");
  btn.disabled = cache.loading || !canFetch;
  btn.title = canFetch ? "Re-fetch the schema from the endpoint" : "Deploy this Tool before fetching its schema";
  btn.addEventListener("click", () => runFetch(true));
  head.appendChild(btn);
  wrap.appendChild(head);

  // Read-only JSON view. Schema is dictated by the endpoint, not the operator.
  const t = document.createElement("textarea");
  t.rows = 12; t.spellcheck = false; t.readOnly = true;
  t.className = "schema-readonly";
  const displayedSchema = cache.schema || spec.schema;
  t.value = (displayedSchema === undefined || displayedSchema === null) ? "" : JSON.stringify(displayedSchema, null, 2);
  wrap.appendChild(t);

  // Status line below the editor: source badge, description, or hints.
  if (!canFetch) {
    const hint = document.createElement("div"); hint.className = "hint";
    hint.textContent = required
      ? "OpenAPI Tools require a schema before deployment. Add spec.schema in YAML and click Apply YAML, then deploy. Studio can introspect the saved Tool afterward."
      : "Deploy this Tool with a name and endpoint first. Studio then fetches the schema from the saved Tool.";
    wrap.appendChild(hint);
    if (required) {
      const edit = document.createElement("button"); edit.type = "button"; edit.className = "action";
      edit.textContent = "Edit schema in YAML";
      edit.addEventListener("click", () => switchView("yaml"));
      wrap.appendChild(edit);
    }
  } else if (cache.loading && cache.fetchedAt === 0) {
    const hint = document.createElement("div"); hint.className = "hint";
    hint.textContent = "Fetching schema from endpoint…";
    wrap.appendChild(hint);
  } else if (cache.fetchedAt > 0 && !cache.error) {
    const meta = document.createElement("div"); meta.className = "chat-meta";
    const sourceLabel = cache.source === "live" ? "live from endpoint" : (cache.source === "declared" ? "declared on CR" : "no schema");
    meta.innerHTML = `Source: <span class="schema-source-${escapeHtml(cache.source)}">${escapeHtml(sourceLabel)}</span>` + (cache.description ? ` · ${escapeHtml(cache.description)}` : "");
    wrap.appendChild(meta);
  }

  if (cache.warning) wrap.appendChild(previewBanner("warn", escapeHtml(cache.warning)));
  if (cache.error) wrap.appendChild(previewBanner("warn", `Schema fetch failed: ${escapeHtml(cache.error)}`));

  // Raw endpoint payload for reference — full OpenAPI doc or full MCP
  // tools/list entry — collapsed by default.
  if (cache.raw && cache.source === "live") {
    const details = document.createElement("details"); details.className = "schema-raw";
    const summary = document.createElement("summary");
    summary.textContent = proto === "OpenAPI"
      ? "OpenAPI spec returned by endpoint"
      : "MCP tool metadata (tools/list entry)";
    details.appendChild(summary);
    const rawPre = document.createElement("pre");
    rawPre.textContent = JSON.stringify(cache.raw, null, 2);
    details.appendChild(rawPre);
    wrap.appendChild(details);
  }

  return wrap;
}

// ----- Workflow runner ----------------------------------------------------
function renderWorkflowRunner(host) {
  const key = previewKey();
  const name = state.draft.metadata.name;
  const ns = state.draft.metadata.namespace || "default";
  const run = state.preview.runs[key] || (state.preview.runs[key] = { runName: "", status: null, polling: false, input: "", parameters: [], error: "" });

  const wrap = document.createElement("div"); wrap.className = "run-panel";

  // Input form
  const form = document.createElement("div"); form.className = "run-form";
  const formTitle = document.createElement("h3"); formTitle.className = "run-section-title"; formTitle.textContent = "Run inputs";
  form.appendChild(formTitle);

  const inputLabel = document.createElement("label"); inputLabel.className = "run-field-label"; inputLabel.textContent = "Input (string passed to the entry step)";
  const inputTa = document.createElement("textarea"); inputTa.className = "run-input"; inputTa.rows = 3; inputTa.value = run.input;
  inputTa.placeholder = "Optional free-form input for the workflow.";
  inputTa.addEventListener("input", () => { run.input = inputTa.value; });
  form.appendChild(inputLabel); form.appendChild(inputTa);

  const paramsLabel = document.createElement("div"); paramsLabel.className = "run-field-label"; paramsLabel.textContent = "Parameters (key/value)";
  form.appendChild(paramsLabel);
  const paramsHost = document.createElement("div"); paramsHost.className = "run-params";
  renderRunParams(paramsHost, run);
  form.appendChild(paramsHost);

  const runRow = document.createElement("div"); runRow.className = "run-actions";
  const runBtn = document.createElement("button"); runBtn.className = "action primary";
  runBtn.textContent = run.polling ? "Running…" : "Run workflow";
  runBtn.disabled = run.polling;
  runBtn.addEventListener("click", async () => {
    run.error = "";
    run.polling = true;
    run.status = null;
    const params = {};
    for (const [k, v] of run.parameters) if (k) params[k] = v;
    renderPreview(document.getElementById("preview-host"));
    try {
      const created = await apiInvokeWorkflow(name, { input: run.input, parameters: params }, ns);
      run.runName = (created && created.metadata && created.metadata.name) || "";
      if (!run.runName) throw new Error("server did not return a run name");
      startRunPolling(key, run.runName, ns);
    } catch (err) {
      run.polling = false;
      run.error = String(err.message || err);
      renderPreview(document.getElementById("preview-host"));
    }
  });
  runRow.appendChild(runBtn);
  if (run.runName) {
    const openBtn = document.createElement("button"); openBtn.className = "action"; openBtn.textContent = "Open run in sidebar";
    openBtn.addEventListener("click", () => selectResource("workflowruns", run.runName, ns));
    runRow.appendChild(openBtn);
  }
  form.appendChild(runRow);
  if (run.error) {
    const err = document.createElement("div"); err.className = "preview-banner bad"; err.textContent = run.error;
    form.appendChild(err);
  }
  wrap.appendChild(form);

  // Status panel
  if (run.runName || run.status) {
    const statusPanel = document.createElement("div"); statusPanel.className = "run-status";
    const statusTitle = document.createElement("h3"); statusTitle.className = "run-section-title";
    statusTitle.innerHTML = `Latest run <span class="muted">${escapeHtml(run.runName)}</span>`;
    statusPanel.appendChild(statusTitle);
    statusPanel.appendChild(renderRunStatus(run, run.runName, ns));
    wrap.appendChild(statusPanel);
  }

  host.appendChild(wrap);
}

// ----- Evaluation runner --------------------------------------------------
function renderEvaluationRunner(host, rerender) {
  const render = rerender || (() => renderPreview(document.getElementById("preview-host")));
  const key = previewKey();
  const name = state.draft.metadata.name;
  const ns = state.draft.metadata.namespace || "default";
  const spec = state.draft.spec || {};
  const status = state.draft.status || {};
  const run = state.preview.runs[key] || (state.preview.runs[key] = { runName: "", status: null, metadata: null, polling: false, error: "" });

  const wrap = document.createElement("div"); wrap.className = "run-panel";

  const form = document.createElement("div"); form.className = "run-form";
  const formTitle = document.createElement("h3"); formTitle.className = "run-section-title"; formTitle.textContent = "Evaluation target";
  form.appendChild(formTitle);
  form.appendChild(renderEvaluationSpecSummary(spec));

  const runRow = document.createElement("div"); runRow.className = "run-actions";
  const runBtn = document.createElement("button"); runBtn.className = "action primary";
  runBtn.textContent = run.polling ? "Running…" : "Run evaluation";
  runBtn.disabled = run.polling;
  runBtn.addEventListener("click", async () => {
    run.error = "";
    run.polling = true;
    run.status = null;
    run.metadata = null;
    render();
    try {
      const body = evaluationRunManifest(name, ns, spec);
      const created = await apiCreate("evaluationruns", body, ns);
      run.runName = (created && created.metadata && created.metadata.name) || "";
      run.status = (created && created.status) || {};
      run.metadata = (created && created.metadata) || {};
      if (!run.runName) throw new Error("server did not return an evaluation run name");
      await refreshOne("evaluationruns");
      startRunPolling(key, run.runName, ns, "evaluationruns");
    } catch (err) {
      run.polling = false;
      run.error = String(err.message || err);
      render();
    }
  });
  runRow.appendChild(runBtn);
  if (run.runName) {
    const openBtn = document.createElement("button"); openBtn.className = "action"; openBtn.textContent = "Open run in sidebar";
    openBtn.addEventListener("click", () => selectResource("evaluationruns", run.runName, ns));
    runRow.appendChild(openBtn);
  }
  if (status.lastRunRef && status.lastRunRef.name) {
    const openLatest = document.createElement("button"); openLatest.className = "action"; openLatest.textContent = "Open latest observed run";
    openLatest.addEventListener("click", () => selectResource("evaluationruns", status.lastRunRef.name, ns));
    runRow.appendChild(openLatest);
  }
  form.appendChild(runRow);
  if (run.error) {
    const err = document.createElement("div"); err.className = "preview-banner bad"; err.textContent = run.error;
    form.appendChild(err);
  }
  wrap.appendChild(form);

  if (status.phase || status.lastRunRef || (status.runHistory || []).length) {
    const parentPanel = document.createElement("div"); parentPanel.className = "run-status";
    const title = document.createElement("h3"); title.className = "run-section-title"; title.textContent = "Evaluation status";
    parentPanel.appendChild(title);
    parentPanel.appendChild(renderEvaluationSummaryStatus(status));
    wrap.appendChild(parentPanel);
  }

  if (run.runName || run.status) {
    const statusPanel = document.createElement("div"); statusPanel.className = "run-status";
    const statusTitle = document.createElement("h3"); statusTitle.className = "run-section-title";
    statusTitle.innerHTML = `Latest triggered run <span class="muted">${escapeHtml(run.runName)}</span>`;
    statusPanel.appendChild(statusTitle);
    statusPanel.appendChild(renderEvaluationRunStatus({ status: run.status || {}, metadata: run.metadata || { name: run.runName, namespace: ns }, polling: run.polling }, run.runName || key));
    wrap.appendChild(statusPanel);
  }

  host.appendChild(wrap);
}

function evaluationRunManifest(evaluationName, ns, spec) {
  const body = {
    apiVersion: API_VERSION,
    kind: "EvaluationRun",
    metadata: {
      name: studioEvaluationRunName(evaluationName),
      namespace: ns,
      labels: {
        "kognition.dev/created-by": "studio",
        "kognition.dev/evaluation": evaluationName,
      },
    },
    spec: {
      evaluationRef: { name: evaluationName },
      targetRef: clone(spec.targetRef || {}),
      datasetRef: clone(spec.datasetRef || {}),
    },
  };
  if (Array.isArray(spec.graders) && spec.graders.length) body.spec.graders = clone(spec.graders);
  if (Array.isArray(spec.metricThresholds) && spec.metricThresholds.length) body.spec.metricThresholds = clone(spec.metricThresholds);
  return body;
}

function studioEvaluationRunName(evaluationName) {
  const suffix = `-studio-${Date.now().toString(36)}`;
  const base = String(evaluationName || "evaluation").toLowerCase().replace(/[^a-z0-9-]/g, "-").replace(/^-+|-+$/g, "") || "evaluation";
  return `${base.slice(0, Math.max(1, 253 - suffix.length))}${suffix}`;
}

function renderEvaluationSpecSummary(spec) {
  const grid = document.createElement("div"); grid.className = "eval-meta-grid";
  const target = spec.targetRef || {};
  const dataset = spec.datasetRef || {};
  addEvalMeta(grid, "Target", [target.kind || "?", target.name || "?"].filter(Boolean).join("/"));
  addEvalMeta(grid, "Dataset", [dataset.name || "?", dataset.version ? `version ${dataset.version}` : ""].filter(Boolean).join(" · "));
  addEvalMeta(grid, "Graders", Array.isArray(spec.graders) && spec.graders.length ? spec.graders.map((g) => `${g.name || g.type || "grader"}${g.type ? ` (${g.type})` : ""}`).join(", ") : "default built-ins");
  addEvalMeta(grid, "Thresholds", Array.isArray(spec.metricThresholds) && spec.metricThresholds.length ? spec.metricThresholds.map((t) => t.metric).join(", ") : "none");
  return grid;
}

function addEvalMeta(grid, label, value) {
  const k = document.createElement("div"); k.className = "eval-meta-k"; k.textContent = label;
  const v = document.createElement("div"); v.className = "eval-meta-v"; v.textContent = value || "—";
  grid.appendChild(k); grid.appendChild(v);
}

function renderEvaluationSummaryStatus(status) {
  const wrap = document.createElement("div");
  const row = document.createElement("div"); row.className = "run-phase-row";
  const phase = status.phase || "—";
  row.innerHTML = `<span class="run-phase ${phaseClassFor(phase)}">${escapeHtml(phase)}</span>`;
  if (status.lastRunRef && status.lastRunRef.name) row.innerHTML += `<span class="muted"> · latest ${escapeHtml(status.lastRunRef.name)}</span>`;
  if (status.lastRunTime) row.innerHTML += `<span class="muted"> · ${escapeHtml(formatTime(status.lastRunTime))}</span>`;
  wrap.appendChild(row);
  const history = Array.isArray(status.runHistory) ? status.runHistory.slice(-5).reverse() : [];
  if (history.length) {
    const list = document.createElement("div"); list.className = "eval-metrics";
    for (const item of history) {
      const r = document.createElement("div"); r.className = "eval-metric-row";
      r.innerHTML = `<span class="run-phase ${phaseClassFor(item.phase)}">${escapeHtml(item.phase || "—")}</span><span class="run-step-name">${escapeHtml(item.name || "run")}</span><span>${escapeHtml(scoreText(item.score))}</span>`;
      list.appendChild(r);
    }
    wrap.appendChild(list);
  }
  return wrap;
}

function renderEvaluationRunViewer(host) {
  const ns = state.draft.metadata.namespace || "default";
  const name = state.draft.metadata.name || "";
  const spec = state.draft.spec || {};
  const evalRef = spec.evaluationRef && spec.evaluationRef.name;
  const wrap = document.createElement("div"); wrap.className = "run-panel";

  const header = document.createElement("div"); header.className = "run-section-title";
  header.innerHTML = `Evaluation run <strong>${escapeHtml(name)}</strong>`;
  if (evalRef) header.innerHTML += ` <span class="muted">of evaluation ${escapeHtml(evalRef)}</span>`;
  wrap.appendChild(header);
  const summary = document.createElement("div"); summary.className = "run-form";
  summary.appendChild(renderEvaluationSpecSummary(spec));
  if (evalRef) {
    const actions = document.createElement("div"); actions.className = "run-actions";
    const openEval = document.createElement("button"); openEval.className = "action"; openEval.textContent = "Open parent evaluation";
    openEval.addEventListener("click", () => selectResource("evaluations", evalRef, ns));
    actions.appendChild(openEval);
    summary.appendChild(actions);
  }
  wrap.appendChild(summary);

  const statusPanel = document.createElement("div"); statusPanel.className = "run-status";
  const statusTitle = document.createElement("h3"); statusTitle.className = "run-section-title"; statusTitle.textContent = "Results";
  statusPanel.appendChild(statusTitle);
  statusPanel.appendChild(renderEvaluationRunStatus(state.draft, name));
  wrap.appendChild(statusPanel);
  host.appendChild(wrap);
}

function renderEvaluationRunStatus(run, openKeyPrefix) {
  const wrap = document.createElement("div");
  const status = run.status || {};
  const phase = status.phase || (run.polling ? "Pending" : "—");
  const row = document.createElement("div"); row.className = "run-phase-row";
  row.innerHTML = `<span class="run-phase ${phaseClassFor(phase)}">${escapeHtml(phase)}</span>`;
  if (status.startedAt) row.innerHTML += `<span class="muted"> · started ${escapeHtml(formatTime(status.startedAt))}</span>`;
  if (status.completedAt) row.innerHTML += `<span class="muted"> · finished ${escapeHtml(formatTime(status.completedAt))}</span>`;
  wrap.appendChild(row);

  const scoreLine = document.createElement("div"); scoreLine.className = "chat-meta";
  const result = status.passed ? "passed" : (phase === "Failed" ? "failed" : "pending");
  scoreLine.textContent = `Result: ${result} · Score: ${scoreText(status.score)}`;
  wrap.appendChild(scoreLine);
  if (status.message) wrap.appendChild(previewBanner(phase === "Failed" ? "bad" : "info", escapeHtml(status.message)));

  if (Array.isArray(status.metrics) && status.metrics.length) {
    const metrics = document.createElement("div"); metrics.className = "eval-metrics";
    const label = document.createElement("div"); label.className = "run-field-label"; label.textContent = "Grader metrics";
    wrap.appendChild(label);
    for (const metric of status.metrics) {
      const metricRow = document.createElement("div"); metricRow.className = "eval-metric-row";
      metricRow.innerHTML = `<span class="run-phase ${metric.passed ? "success" : "failed"}">${metric.passed ? "pass" : "fail"}</span><span class="run-step-name">${escapeHtml(metric.metric || "metric")}</span><span>${escapeHtml(String(metric.value ?? "—"))}</span>`;
      if (metric.message) metricRow.innerHTML += `<span class="run-step-msg">${escapeHtml(metric.message)}</span>`;
      metrics.appendChild(metricRow);
    }
    wrap.appendChild(metrics);
  }
  if (Array.isArray(status.caseResults) && status.caseResults.length) {
    wrap.appendChild(renderEvaluationCaseResults(status.caseResults, openKeyPrefix));
  }
  if (status.targetOutput) {
    wrap.appendChild(renderCollapsibleBlock("First Target Output", status.targetOutput, `${openKeyPrefix || "evaluation-run"}#target-output`));
  }
  return wrap;
}

function renderEvaluationCaseResults(caseResults, openKeyPrefix) {
  const wrap = document.createElement("div");
  wrap.className = "eval-metrics";
  const label = document.createElement("div");
  label.className = "run-field-label";
  label.textContent = "Case results";
  wrap.appendChild(label);
  for (const result of caseResults) {
    const row = document.createElement("div");
    row.className = "eval-metric-row";
    row.innerHTML = `<span class="run-phase ${result.passed ? "success" : "failed"}">${result.passed ? "pass" : "fail"}</span><span class="run-step-name">${escapeHtml(result.id || "case")}</span><span>${escapeHtml(result.targetJobRef && result.targetJobRef.name ? result.targetJobRef.name : "—")}</span>`;
    if (result.message) row.innerHTML += `<span class="run-step-msg">${escapeHtml(result.message)}</span>`;
    wrap.appendChild(row);
    if (result.targetOutput) {
      wrap.appendChild(renderCollapsibleBlock(`Output: ${result.id || "case"}`, result.targetOutput, `${openKeyPrefix || "evaluation-run"}#case-${result.id || "case"}`));
    }
  }
  return wrap;
}

function scoreText(score) {
  if (score === null || score === undefined || score === "") return "—";
  const n = Number(score);
  if (Number.isNaN(n)) return String(score);
  return n.toFixed(3).replace(/0+$/, "").replace(/\.$/, "");
}

function renderRunParams(host, run) {
  host.innerHTML = "";
  for (let i = 0; i < run.parameters.length; i++) {
    const [k, v] = run.parameters[i];
    const row = document.createElement("div"); row.className = "run-param-row";
    const ki = document.createElement("input"); ki.placeholder = "key"; ki.value = k;
    ki.addEventListener("input", () => { run.parameters[i][0] = ki.value; });
    const vi = document.createElement("input"); vi.placeholder = "value"; vi.value = v;
    vi.addEventListener("input", () => { run.parameters[i][1] = vi.value; });
    const del = document.createElement("button"); del.className = "action"; del.textContent = "×";
    del.addEventListener("click", () => { run.parameters.splice(i, 1); renderRunParams(host, run); });
    row.appendChild(ki); row.appendChild(vi); row.appendChild(del);
    host.appendChild(row);
  }
  const add = document.createElement("button"); add.className = "add-row"; add.textContent = "+ add parameter";
  add.addEventListener("click", () => { run.parameters.push(["", ""]); renderRunParams(host, run); });
  host.appendChild(add);
}

// Detect approval gates that are currently blocking the run. Returns an array
// of {from, to, iteration} objects. A gate is "pending" when its `to` step is
// active but has no stepStatus entry — the controller records
// `kognition.dev/gate-entered.<step>` while it waits for an ApprovalDecision.
function detectPendingApprovalGates(status) {
  const snap = status && status.snapshotSpec;
  const gates = (snap && snap.approvalGates) || [];
  if (!gates.length) return [];
  const active = new Set([
    ...(status.activeSteps || []),
    ...(status.activeStep ? [status.activeStep] : []),
  ]);
  if (!active.size) return [];
  const stepStatuses = status.stepStatuses || [];
  const started = new Set(stepStatuses.map((s) => s.name));
  const out = [];
  for (const g of gates) {
    if (!g || !g.to) continue;
    if (!active.has(g.to)) continue;
    if (started.has(g.to)) continue; // step has already begun executing
    out.push({ from: g.from || "", to: g.to, requiredApprovers: g.requiredApprovers || 0 });
  }
  return out;
}

// Render a prominent banner + Approve/Reject buttons for a single pending
// approval gate. On success, POSTs an ApprovalDecision to the apiserver and
// invokes `onDone` so the caller can re-render.
function renderApprovalGatePrompt(runName, ns, gate, onDone) {
  const box = document.createElement("div");
  box.className = "run-approval-gate";
  const title = document.createElement("div"); title.className = "run-approval-gate-title";
  const arrow = gate.from ? `${escapeHtml(gate.from)} → ` : "";
  title.innerHTML = `⏸ Awaiting approval: ${arrow}<strong>${escapeHtml(gate.to)}</strong>`;
  box.appendChild(title);

  const hint = document.createElement("div"); hint.className = "run-approval-gate-hint";
  hint.textContent = "This step will not execute until an approval is recorded. Choose Approve or Reject below.";
  box.appendChild(hint);

  const reasonRow = document.createElement("div"); reasonRow.className = "run-approval-gate-reason";
  const reasonLbl = document.createElement("label"); reasonLbl.textContent = "Reason (optional)";
  const reasonIn = document.createElement("input"); reasonIn.type = "text"; reasonIn.placeholder = "e.g. reviewed by me";
  reasonRow.appendChild(reasonLbl); reasonRow.appendChild(reasonIn);
  box.appendChild(reasonRow);

  const actions = document.createElement("div"); actions.className = "run-approval-gate-actions";
  const approve = document.createElement("button"); approve.className = "action primary"; approve.textContent = "Approve";
  const reject = document.createElement("button"); reject.className = "action"; reject.textContent = "Reject";
  actions.appendChild(approve); actions.appendChild(reject);
  box.appendChild(actions);

  const err = document.createElement("div"); err.className = "preview-banner bad"; err.style.display = "none"; err.style.marginTop = "8px";
  box.appendChild(err);

  const submit = async (decision) => {
    approve.disabled = true; reject.disabled = true;
    err.style.display = "none"; err.textContent = "";
    const label = decision === "approve" ? "Approving…" : "Rejecting…";
    if (decision === "approve") approve.textContent = label; else reject.textContent = label;
    try {
      // Deterministic name so retries collapse into a single object rather than
      // spamming duplicates that all count toward the same approver.
      const suffix = Math.random().toString(36).slice(2, 8);
      const body = {
        apiVersion: "kognition.dev/v1alpha1",
        kind: "ApprovalDecision",
        metadata: {
          name: `studio-${decision}-${runName}-${gate.to}-${suffix}`.toLowerCase().replace(/[^a-z0-9-]/g, "-").slice(0, 63),
          namespace: ns,
        },
        spec: {
          workflowRunRef: { name: runName },
          step: gate.to,
          decision,
          reason: reasonIn.value || undefined,
        },
      };
      await apiCreate("approvaldecisions", body, ns);
      onDone && onDone();
    } catch (e) {
      approve.disabled = false; reject.disabled = false;
      approve.textContent = "Approve"; reject.textContent = "Reject";
      err.textContent = String(e.message || e);
      err.style.display = "block";
    }
  };
  approve.addEventListener("click", () => submit("approve"));
  reject.addEventListener("click", () => submit("deny"));
  return box;
}

function renderRunStatus(run, openKeyPrefix, ns) {
  const wrap = document.createElement("div");
  const status = run.status || {};
  const metadata = run.metadata || {};
  const runNs = ns || metadata.namespace || state.namespace || "default";
  const prefix = openKeyPrefix || run.runName || metadata.name || "run";
  const phase = status.phase || (run.polling ? "Pending" : "—");
  const phaseClass = phaseClassFor(phase);
  const isTerminal = ["Ready", "Succeeded", "Failed", "Cancelled", "Canceled"].includes(phase);

  const head = document.createElement("div"); head.className = "run-phase-row";
  head.innerHTML = `<span class="run-phase ${phaseClass}">${escapeHtml(phase)}</span>`;
  if (status.startTime) head.innerHTML += `<span class="muted"> · started ${escapeHtml(formatTime(status.startTime))}</span>`;
  if (status.completionTime) head.innerHTML += `<span class="muted"> · finished ${escapeHtml(formatTime(status.completionTime))}</span>`;
  wrap.appendChild(head);

  // Active step is meaningful only while the run is still progressing.
  if (!isTerminal && (status.activeStep || (status.activeSteps || []).length)) {
    const active = status.activeSteps && status.activeSteps.length ? status.activeSteps.join(", ") : status.activeStep;
    const row = document.createElement("div"); row.className = "run-active";
    row.innerHTML = `<span class="muted">Active:</span> ${escapeHtml(active)}`;
    wrap.appendChild(row);
  }

  // Awaiting-approval detection: any active step that is the destination of an
  // approval gate but has no stepStatus yet is paused waiting for a human vote.
  // Surface a banner + Approve/Reject buttons so the user isn't stuck.
  const pendingGates = detectPendingApprovalGates(status);
  const runName = run.runName || metadata.name || "";
  for (const gate of pendingGates) {
    wrap.appendChild(renderApprovalGatePrompt(runName, runNs, gate, () => {
      // Re-render whichever surface we're on so the banner reflects the just-
      // submitted decision. The viewer poller will pick up the controller's
      // updates on its next tick (≤1.5s).
      if (state.view === "preview") {
        const host = document.getElementById("preview-host");
        if (host) renderPreview(host);
      } else if (state.view === "builder") {
        renderEditor();
      }
    }));
  }

  // Surface failed/blocked conditions prominently so the user sees *why* the
  // run failed without diving into the YAML.
  const conditions = status.conditions || [];
  const errCond = conditions.find((c) => c.status === "False" && c.message);
  if (errCond && phase === "Failed") {
    const banner = document.createElement("div");
    banner.className = "preview-banner bad";
    banner.innerHTML = `<strong>${escapeHtml(errCond.reason || "Failed")}:</strong> ${escapeHtml(errCond.message)}`;
    wrap.appendChild(banner);
  }

  // Per-step timeouts come from the snapshot of the workflow spec. We index by
  // name to render the configured timeout next to each step's status.
  const snapshotSteps = (status.snapshotSpec && status.snapshotSpec.steps) || [];
  const stepSpecByName = {};
  for (const s of snapshotSteps) stepSpecByName[s.name] = s;

  const steps = status.stepStatuses || [];
  if (steps.length) {
    const list = document.createElement("div"); list.className = "run-step-list";
    // Compute end-time for each step as: next step's startedAt OR run completionTime.
    const sortedByStart = steps
      .map((s, i) => ({ s, i, t: s.startedAt ? Date.parse(s.startedAt) : NaN }))
      .filter((x) => !isNaN(x.t))
      .sort((a, b) => a.t - b.t);
    const endTimeFor = (idx) => {
      const myStart = steps[idx].startedAt ? Date.parse(steps[idx].startedAt) : NaN;
      if (isNaN(myStart)) return NaN;
      const next = sortedByStart.find((x) => x.t > myStart);
      if (next) return next.t;
      if (status.completionTime) return Date.parse(status.completionTime);
      return NaN;
    };
    for (let i = 0; i < steps.length; i++) {
      list.appendChild(renderStepCard(steps[i], stepSpecByName[steps[i].name], endTimeFor(i), prefix, { runName, ns: runNs, terminal: isTerminal }));
    }
    wrap.appendChild(list);
  }

  const approvals = status.approvalRecords || [];
  if (approvals.length) {
    const list = document.createElement("div"); list.className = "run-approvals";
    const title = document.createElement("div"); title.className = "run-field-label"; title.textContent = "Approvals";
    list.appendChild(title);
    for (const a of approvals) {
      const row = document.createElement("div"); row.className = "run-step-row";
      row.innerHTML = `<span class="run-phase ${phaseClassFor(a.decision || a.state)}">${escapeHtml(a.decision || a.state || "Pending")}</span><span class="run-step-name">${escapeHtml((a.from || "?") + " → " + (a.to || "?"))}</span>`;
      if (a.approver) row.innerHTML += `<span class="run-step-msg">by ${escapeHtml(a.approver)}</span>`;
      list.appendChild(row);
    }
    wrap.appendChild(list);
  }

  if (status.output) {
    const outTitle = document.createElement("div"); outTitle.className = "run-field-label"; outTitle.textContent = "Output";
    wrap.appendChild(outTitle);
    const out = document.createElement("pre"); out.className = "run-output"; out.textContent = status.output;
    wrap.appendChild(out);
  }
  return wrap;
}

function renderStepCard(step, spec, endTimeMs, openKeyPrefix, runCtx) {
  const card = document.createElement("div");
  card.className = "run-step-card";

  const header = document.createElement("div");
  header.className = "run-step-card-head";

  const phaseEl = document.createElement("span");
  phaseEl.className = `run-phase ${phaseClassFor(step.phase)}`;
  phaseEl.textContent = step.phase || "—";
  header.appendChild(phaseEl);

  const nameEl = document.createElement("span");
  nameEl.className = "run-step-name";
  nameEl.textContent = step.name || "(unnamed)";
  header.appendChild(nameEl);

  const metaParts = [];
  if (step.attempts && step.attempts > 1) metaParts.push(`attempt ${step.attempts}`);
  if (step.startedAt) {
    metaParts.push(`started ${formatTime(step.startedAt)}`);
    const startMs = Date.parse(step.startedAt);
    if (!isNaN(startMs) && !isNaN(endTimeMs)) {
      const elapsedSec = Math.max(0, Math.round((endTimeMs - startMs) / 1000));
      metaParts.push(`took ${elapsedSec}s`);
    }
  }
  if (spec && spec.timeout) metaParts.push(`timeout ${spec.timeout}`);
  if (metaParts.length) {
    const meta = document.createElement("span");
    meta.className = "run-step-meta";
    meta.textContent = metaParts.join(" · ");
    header.appendChild(meta);
  }
  card.appendChild(header);

  // TimedOut: explain why up front. The runtime never returned a result within
  // the step's configured timeout window — usually slow LLM, cold pod start,
  // or oversized prompt.
  if ((step.phase || "").toLowerCase() === "timedout") {
    const why = document.createElement("div"); why.className = "run-step-note bad";
    const cfg = spec && spec.timeout ? ` (limit ${spec.timeout})` : "";
    why.textContent = `Step exceeded its timeout${cfg}. Increase the step's timeout, or check the agent's pod logs to see what it was doing.`;
    card.appendChild(why);
  }

  if (spec && (spec.agentRef || spec.toolRef)) {
    const tgt = document.createElement("div"); tgt.className = "run-step-target";
    if (spec.agentRef) tgt.innerHTML = `<span class="muted">agent</span> ${escapeHtml(spec.agentRef.name)}`;
    else if (spec.toolRef) tgt.innerHTML = `<span class="muted">tool</span> ${escapeHtml(spec.toolRef.name)}`;
    card.appendChild(tgt);
  }

  if (step.output) {
    const openKey = `${openKeyPrefix || "run"}#${step.name || ""}#output`;
    card.appendChild(renderCollapsibleBlock("Output", step.output, openKey));
  }
  if (spec && spec.collaboration && runCtx && runCtx.runName && !StudioTutorial.isPractice()) {
    card.appendChild(renderCollaborationPanel(step.name, runCtx, openKeyPrefix));
  }
  return card;
}

// Collaboration traces are cached per run; the preview DOM is rebuilt on every
// poll, so the cache keeps the panel from flashing and bounds the requests.
const collabCache = new Map();
const COLLAB_REFRESH_MS = 3000;

function collabCacheKey(ctx) {
  return `${ctx.ns}/${ctx.runName}`;
}

function refreshCollabTrace(ctx, onUpdate) {
  const key = collabCacheKey(ctx);
  const entry = collabCache.get(key) || { trace: null, error: "", fetchedAt: 0, pending: false, final: false };
  collabCache.set(key, entry);
  if (entry.pending || (entry.final && entry.trace)) return entry;
  if (entry.fetchedAt && Date.now() - entry.fetchedAt < COLLAB_REFRESH_MS) return entry;
  entry.pending = true;
  const url = `/api/workflowruns/${encodeURIComponent(ctx.runName)}/collaboration?namespace=${encodeURIComponent(ctx.ns || "")}`;
  studioFetch(url)
    .then(async (r) => {
      if (!r.ok) {
        let msg = `HTTP ${r.status}`;
        try { msg = (await r.json()).error || msg; } catch (_) { /* keep status text */ }
        throw new Error(msg);
      }
      return r.json();
    })
    .then((trace) => {
      entry.trace = trace;
      entry.error = "";
      // A finished run no longer changes, so stop polling once the ledger was read.
      entry.final = !!ctx.terminal;
    })
    .catch((e) => { entry.error = String((e && e.message) || e); })
    .finally(() => {
      entry.pending = false;
      entry.fetchedAt = Date.now();
      onUpdate();
    });
  return entry;
}

function renderCollaborationPanel(stepName, ctx, openKeyPrefix) {
  const host = document.createElement("div");
  host.className = "collab-panel";
  const paint = () => {
    // A poll tick may have replaced this node; the replacement reads the cache.
    if (!host.isConnected) return;
    const entry = collabCache.get(collabCacheKey(ctx));
    fillCollaborationPanel(host, stepName, entry, openKeyPrefix);
  };
  const entry = refreshCollabTrace(ctx, paint);
  fillCollaborationPanel(host, stepName, entry, openKeyPrefix);
  return host;
}

function collabFmtDuration(startISO, endISO) {
  const a = Date.parse(startISO), b = Date.parse(endISO);
  if (isNaN(a) || isNaN(b)) return "";
  return `${Math.max(0, Math.round((b - a) / 1000))}s`;
}

function fillCollaborationPanel(host, stepName, entry, openKeyPrefix) {
  host.innerHTML = "";
  const title = document.createElement("div");
  title.className = "run-field-label";
  title.textContent = "Collaboration";
  host.appendChild(title);

  const trace = entry && entry.trace;
  const info = trace && (trace.steps || []).find((s) => s.step === stepName);
  const note = (text, cls) => {
    const n = document.createElement("div");
    n.className = `run-step-note ${cls || ""}`.trim();
    n.textContent = text;
    host.appendChild(n);
  };
  if (!trace) {
    note(entry && entry.error ? `Trace unavailable: ${entry.error}` : "Loading trace…", entry && entry.error ? "bad" : "");
    return;
  }
  if (!info) { note("No trace for this step yet."); return; }
  if (!info.available) { note(info.message || "No trace available."); return; }

  const c = info.counters || {}, lim = info.limits || {};
  const of = (used, max) => (max ? `${used} of ${max}` : `${used}`);
  const summary = document.createElement("div");
  summary.className = "collab-summary";
  const parts = [`<span class="collab-strategy">${escapeHtml(info.strategy || "")}</span>`, `<span class="run-phase ${phaseClassFor(info.phase)}">${escapeHtml(info.phase || "")}</span>`];
  if (info.finalBy) parts.push(`final answer by <strong>${escapeHtml(info.finalBy)}</strong>`);
  parts.push(`<span class="muted">turns ${escapeHtml(of(c.turns || 0, lim.maxTurns))} · delegations ${escapeHtml(of(c.delegations || 0, lim.maxDelegations))} · messages ${c.messages || 0} · tokens ${escapeHtml(of(c.tokensUsed || 0, lim.maxTokens))}</span>`);
  summary.innerHTML = parts.join(" ");
  host.appendChild(summary);

  const members = info.members || [];
  const chips = document.createElement("div");
  chips.className = "collab-members";
  for (const m of members) {
    const chip = document.createElement("span");
    let status = "not involved";
    let cls = "idle";
    if (m.final) { status = "answered"; cls = "final"; }
    else if (m.contributed) { status = "contributed"; cls = "contrib"; }
    else if (m.involved) { status = "ran"; cls = "ran"; }
    chip.className = `collab-chip ${cls}`;
    const detail = [`agent ${m.agent || "?"}`, `${m.tasks} task(s)`, `${m.turns} turn(s)`];
    if (m.tokens) detail.push(`${m.tokens} tokens`);
    chip.title = detail.join(" · ");
    chip.innerHTML = `<strong>${escapeHtml(m.alias)}</strong> <span class="collab-role">${escapeHtml(m.role)}</span> <span class="collab-status">${escapeHtml(status)}</span>`;
    chips.appendChild(chip);
  }
  host.appendChild(chips);

  const idle = members.filter((m) => !m.involved).map((m) => m.alias);
  if (info.strategy === "Supervisor" && info.phase === "Succeeded" && members.length > 1 && (c.delegations || 0) === 0 && idle.length) {
    note(`The coordinator answered without delegating, so ${idle.join(", ")} did not take part.`);
  } else if (info.reason && info.phase !== "Succeeded") {
    note(info.reason, "bad");
  }

  const tasks = info.tasks || [];
  const dispatches = info.dispatches || [];
  if (!tasks.length && !dispatches.length) return;
  const details = document.createElement("details");
  details.className = "collab-trace";
  const openKey = `${openKeyPrefix || "run"}#${stepName}#collab-trace`;
  if (state.preview.openBlocks[openKey]) details.open = true;
  details.addEventListener("toggle", () => {
    if (details.open) state.preview.openBlocks[openKey] = true;
    else delete state.preview.openBlocks[openKey];
  });
  const sum = document.createElement("summary");
  sum.textContent = `Trace (${tasks.length} task(s), ${dispatches.length} dispatch(es))`;
  details.appendChild(sum);

  for (const t of tasks) {
    const row = document.createElement("div");
    row.className = "collab-task";
    const route = t.requester ? `${t.requester} → ${t.recipient}` : t.recipient;
    row.innerHTML = `<div class="collab-task-head"><span class="run-phase ${phaseClassFor(t.state)}">${escapeHtml(t.state)}</span> <strong>${escapeHtml(t.id)}</strong> <span>${escapeHtml(route)}</span>${t.final ? ' <span class="collab-chip final">final</span>' : ""}<span class="muted"> · ${t.turns} turn(s)</span></div>`;
    if (t.reason) {
      const why = document.createElement("div"); why.className = "run-step-note bad"; why.textContent = t.reason;
      row.appendChild(why);
    }
    for (const d of dispatches.filter((x) => x.taskId === t.id)) {
      const line = document.createElement("div");
      line.className = "collab-dispatch muted";
      const bits = [`${d.kind} ${d.id} → ${d.recipient}`, d.status];
      if (d.job) {
        bits.push(`job ${d.job.name}`);
        if (d.job.phase) bits.push(d.job.phase);
        if (d.job.totalTokens) bits.push(`${d.job.totalTokens} tokens`);
        const took = collabFmtDuration(d.job.startTime, d.job.completionTime);
        if (took) bits.push(took);
      }
      line.textContent = bits.join(" · ");
      row.appendChild(line);
      if (d.reason || (d.job && d.job.message)) {
        const why = document.createElement("div"); why.className = "run-step-note bad"; why.textContent = d.reason || d.job.message;
        row.appendChild(why);
      }
    }
    const goalKey = `${openKey}#${t.id}#goal`;
    row.appendChild(renderCollapsibleBlock(`Goal${t.goal && t.goal.truncated ? " (truncated)" : ""}`, (t.goal && t.goal.text) || "", goalKey));
    if (t.result) {
      row.appendChild(renderCollapsibleBlock(`Result${t.result.truncated ? " (truncated)" : ""}`, t.result.text || "", `${openKey}#${t.id}#result`));
    }
    details.appendChild(row);
  }
  host.appendChild(details);
}

function renderCollapsibleBlock(label, text, openKey) {
  const wrap = document.createElement("details");
  wrap.className = "run-step-output";
  // Persist open state across status changes that rebuild this DOM node.
  if (openKey && state.preview.openBlocks[openKey]) wrap.open = true;
  if (openKey) {
    wrap.addEventListener("toggle", () => {
      if (wrap.open) state.preview.openBlocks[openKey] = true;
      else delete state.preview.openBlocks[openKey];
    });
  }
  const sum = document.createElement("summary");
  const preview = (text || "").trim().split("\n", 1)[0].slice(0, 120);
  sum.textContent = `${label} (${(text || "").length} chars) — ${preview}${preview.length >= 120 ? "…" : ""}`;
  wrap.appendChild(sum);
  const pre = document.createElement("pre");
  pre.className = "run-output";
  pre.textContent = text;
  wrap.appendChild(pre);
  return wrap;
}

// Read-only run viewer shown when a WorkflowRun is selected from the sidebar.
// Reuses the same run-detail rendering as the inline preview tab so the user
// gets the full picture (per-step duration, attempts, outputs, errors) without
// needing to read YAML.
function renderWorkflowRunViewer(host) {
  const ns = state.draft.metadata.namespace || "default";
  const name = state.draft.metadata.name || "";
  const wfRef = (state.draft.spec && state.draft.spec.workflowRef && state.draft.spec.workflowRef.name) || "";
  const input = state.draft.spec && state.draft.spec.input;
  const params = (state.draft.spec && state.draft.spec.parameters) || {};
  const status = state.draft.status || {};

  const wrap = document.createElement("div"); wrap.className = "run-panel";

  const header = document.createElement("div"); header.className = "run-section-title";
  header.innerHTML = `Run <strong>${escapeHtml(name)}</strong>`;
  if (wfRef) header.innerHTML += ` <span class="muted">of workflow ${escapeHtml(wfRef)}</span>`;
  wrap.appendChild(header);

  // Inputs the run was started with — useful for reproducing.
  if (input || Object.keys(params).length) {
    const inputs = document.createElement("div"); inputs.className = "run-form";
    const t = document.createElement("h3"); t.className = "run-section-title"; t.textContent = "Run inputs";
    inputs.appendChild(t);
    if (input) {
      const lbl = document.createElement("div"); lbl.className = "run-field-label"; lbl.textContent = "Input";
      const pre = document.createElement("pre"); pre.className = "run-output"; pre.textContent = input;
      inputs.appendChild(lbl); inputs.appendChild(pre);
    }
    if (Object.keys(params).length) {
      const lbl = document.createElement("div"); lbl.className = "run-field-label"; lbl.textContent = "Parameters";
      inputs.appendChild(lbl);
      for (const [k, v] of Object.entries(params)) {
        const row = document.createElement("div"); row.className = "run-step-row";
        row.innerHTML = `<span class="run-step-name">${escapeHtml(k)}</span><span class="run-step-msg">${escapeHtml(String(v))}</span>`;
        inputs.appendChild(row);
      }
    }
    wrap.appendChild(inputs);
  }

  if (wfRef) {
    const actions = document.createElement("div"); actions.className = "run-actions";
    const openWf = document.createElement("button"); openWf.className = "action"; openWf.textContent = "Open parent workflow";
    openWf.addEventListener("click", () => selectResource("workflows", wfRef, ns));
    actions.appendChild(openWf);
    wrap.appendChild(actions);
  }

  // Reuse renderRunStatus by faking the {status} shape it expects.
  // Use the run's own name as the open-state prefix so collapsibles persist
  // when the user navigates away and back.
  const statusPanel = document.createElement("div"); statusPanel.className = "run-status";
  const statusTitle = document.createElement("h3"); statusTitle.className = "run-section-title";
  statusTitle.textContent = "Status";
  statusPanel.appendChild(statusTitle);
  statusPanel.appendChild(renderRunStatus({ status, metadata: state.draft.metadata || {}, polling: false, runName: name }, name, ns));
  wrap.appendChild(statusPanel);

  // Quick link to the underlying YAML for power users.
  const tip = document.createElement("div"); tip.className = "preview-banner info";
  tip.innerHTML = `Tip: use the <strong>YAML</strong> tab above for the full WorkflowRun manifest, including annotations like trace id.`;
  wrap.appendChild(tip);

  host.appendChild(wrap);
}

function phaseClassFor(phase) {
  switch ((phase || "").toLowerCase()) {
    case "ready": case "succeeded": case "approved": case "complete": case "completed": return "success";
    case "failed": case "rejected": case "error": case "cancelled": case "canceled": case "timedout": return "failed";
    case "running": case "active": case "in_progress": case "inprogress": return "running";
    case "pending": case "waiting": case "queued": return "pending";
    default: return "neutral";
  }
}

function formatTime(ts) {
  try { return new Date(ts).toLocaleTimeString(); } catch (_) { return String(ts); }
}

function runPresentationKey(resource) {
  const metadata = (resource && resource.metadata) || {};
  return JSON.stringify({
    metadata: { name: metadata.name || "", namespace: metadata.namespace || "" },
    spec: (resource && resource.spec) || {},
    status: (resource && resource.status) || {},
  });
}

function startRunPolling(key, runName, ns, plural = "workflowruns") {
  stopRunPolling(key);
  const tick = async () => {
    try {
      const obj = await apiGet(plural, runName, ns);
      const run = state.preview.runs[key];
      if (!run || run.runName !== runName) { stopRunPolling(key); return; }
      const previousPresentation = runPresentationKey({ metadata: run.metadata, status: run.status });
      const wasPolling = run.polling;
      run.status = obj.status || {};
      run.metadata = obj.metadata || {};
      const phase = run.status.phase;
      if (phase === "Ready" || phase === "Succeeded" || phase === "Failed" || phase === "Cancelled") {
        run.polling = false;
        stopRunPolling(key);
      }
      const presentationChanged = previousPresentation !== runPresentationKey({ metadata: run.metadata, status: run.status }) || wasPolling !== run.polling;
      // Re-render if user is currently looking at preview.
      if (presentationChanged && state.view === "preview" && previewKey() === key) {
        renderPreview(document.getElementById("preview-host"));
      } else if (presentationChanged && state.view === "builder" && previewKey() === key && state.draft && state.draft.kind === "Evaluation") {
        renderEditor();
      }
    } catch (err) {
      const run = state.preview.runs[key];
      if (run) {
        run.error = String(err.message || err);
        run.polling = false;
      }
      stopRunPolling(key);
      if (state.view === "preview" && previewKey() === key) {
        renderPreview(document.getElementById("preview-host"));
      } else if (state.view === "builder" && previewKey() === key && state.draft && state.draft.kind === "Evaluation") {
        renderEditor();
      }
    }
  };
  tick();
  state.previewPollers[key] = setInterval(tick, 1500);
}

function stopRunPolling(key) {
  if (state.previewPollers[key]) {
    clearInterval(state.previewPollers[key]);
    delete state.previewPollers[key];
  }
}

// ============================================================================
// DOM wiring
// ============================================================================
function setCreateMenu(open, restoreFocus = false) {
  const trigger = document.getElementById("btn-create");
  const menu = document.getElementById("create-menu");
  menu.toggleAttribute("hidden", !open);
  trigger.setAttribute("aria-expanded", String(open));
  if (open) {
    document.getElementById("create-search").value = "";
    menu.dataset.category = "All";
    filterCreateResources();
    document.getElementById("create-results").scrollTop = 0;
    document.getElementById("create-search").focus();
  }
  else if (restoreFocus) trigger.focus();
}

function filterCreateResources() {
  const menu = document.getElementById("create-menu");
  const terms = document.getElementById("create-search").value.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const category = menu.dataset.category;
  let count = 0;
  menu.querySelectorAll('[role="menuitem"]').forEach(button => {
    const matchesCategory = category === "All" ||
      (category === "Common" ? button.dataset.common === "true" : button.dataset.category === category);
    const text = `${button.dataset.kind} ${button.textContent}`.toLowerCase();
    button.hidden = !matchesCategory || !terms.every(term => text.includes(term));
    button.tabIndex = -1;
    if (!button.hidden) count++;
  });
  menu.querySelector('[role="menuitem"]:not([hidden])')?.setAttribute("tabindex", "0");
  menu.querySelectorAll("[data-create-category]").forEach(button =>
    button.setAttribute("aria-pressed", String(button.dataset.createCategory === category)));
  document.getElementById("create-count").textContent =
    `${count} resource${count === 1 ? "" : "s"}${category === "All" && !terms.length ? ". Common choices first." : ""}`;
  document.getElementById("create-empty").hidden = count !== 0;
}

function setExplorer(open, restoreFocus = false) {
  const explorer = document.getElementById("resource-explorer");
  const trigger = document.getElementById("btn-explorer");
  const backdrop = document.getElementById("explorer-backdrop");
  explorer.classList.toggle("open", open);
  backdrop.toggleAttribute("hidden", !open);
  trigger.setAttribute("aria-expanded", String(open));
  if (open) document.getElementById("resource-filter").focus();
  else if (restoreFocus) trigger.focus();
}

function closeExplorer() {
  if (window.matchMedia("(max-width: 700px)").matches) setExplorer(false);
}

function filterResources(query) {
  const normalized = query.trim().toLowerCase();
  document.querySelectorAll(".resource-list").forEach((list) => {
    list.querySelectorAll("li").forEach((item) => {
      item.hidden = normalized !== "" && !item.textContent.toLowerCase().includes(normalized);
    });
  });
}

document.addEventListener("DOMContentLoaded", () => {
  document.getElementById("namespace-select").addEventListener("change", async (e) => {
    if (!confirmDiscard()) {
      e.target.value = state.namespace;
      return;
    }
    state.namespace = e.target.value;
    namespaceRevision++;
    stopViewerPolling();
    state.draft = null;
    state.draftKey = null;
    state.existsInCluster = false;
    state.dirty = false;
    state.selectedStep = null;
    state.selectedEdge = null;
    resetWorkflowHistory();
    closeInspector();
    setStatus("", "");
    renderEditor();
    await refreshAll();
    refreshSidebarActive();
  });

  const createTrigger = document.getElementById("btn-create");
  const createMenu = document.getElementById("create-menu");
  createTrigger.addEventListener("click", () => setCreateMenu(createMenu.hasAttribute("hidden")));
  createMenu.addEventListener("click", (e) => {
    const category = e.target.closest("[data-create-category]");
    if (category) {
      createMenu.dataset.category = category.dataset.createCategory;
      filterCreateResources();
      document.getElementById("create-results").scrollTop = 0;
    }
    if (e.target.closest('[role="menuitem"]')) setCreateMenu(false, true);
  });
  document.getElementById("create-search").addEventListener("input", () => {
    filterCreateResources();
    document.getElementById("create-results").scrollTop = 0;
  });
  createMenu.addEventListener("focusin", (e) => {
    const active = e.target.closest('[role="menuitem"]');
    if (!active) return;
    createMenu.querySelectorAll('[role="menuitem"]').forEach(item => { item.tabIndex = item === active ? 0 : -1; });
  });
  createMenu.addEventListener("keydown", (e) => {
    if (e.target.closest("[data-create-category]")) return;
    const items = [...createMenu.querySelectorAll('[role="menuitem"]')].filter(item => !item.hidden);
    if (!items.length) return;
    const index = items.indexOf(document.activeElement);
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const offset = e.key === "ArrowDown" ? 1 : -1;
      items[index === -1 ? (offset === 1 ? 0 : items.length - 1) :
        (index + offset + items.length) % items.length].focus();
    } else if (index !== -1 && (e.key === "Home" || e.key === "End")) {
      e.preventDefault();
      items[e.key === "Home" ? 0 : items.length - 1].focus();
    }
  });
  document.addEventListener("pointerdown", (e) => {
    if (!createMenu.hasAttribute("hidden") && !e.target.closest(".create-control")) setCreateMenu(false);
  });
  document.querySelector(".create-control").addEventListener("focusout", (e) => {
    if (!e.currentTarget.contains(e.relatedTarget)) setCreateMenu(false);
  });

  document.getElementById("btn-explorer").addEventListener("click", () => setExplorer(true));
  document.getElementById("btn-explorer-close").addEventListener("click", () => setExplorer(false, true));
  document.getElementById("explorer-backdrop").addEventListener("click", () => setExplorer(false, true));
  document.getElementById("resource-filter").addEventListener("input", (e) => filterResources(e.target.value));

  document.getElementById("btn-new-agent").addEventListener("click", newAgent);
  document.getElementById("btn-new-workflow").addEventListener("click", newWorkflow);
  document.getElementById("btn-new-tool").addEventListener("click", newTool);
  document.getElementById("btn-new-toolclass").addEventListener("click", newToolClass);
  document.getElementById("btn-new-modelgrant").addEventListener("click", newModelGrant);
  document.getElementById("btn-new-memorygrant").addEventListener("click", newMemoryGrant);
  document.getElementById("btn-new-crossnamespacegrant").addEventListener("click", newCrossNamespaceGrant);
  document.getElementById("btn-new-toolgrant").addEventListener("click", newToolGrant);
  document.getElementById("btn-new-evaluationdataset").addEventListener("click", newEvaluationDataset);
  document.getElementById("btn-new-knowledgebase").addEventListener("click", newKnowledgeBase);
  document.getElementById("btn-new-knowledgebaseclaim").addEventListener("click", newKnowledgeBaseClaim);
  document.getElementById("btn-gallery").addEventListener("click", openGallery);
  const emptyGallery = document.getElementById("btn-empty-gallery");
  if (emptyGallery) emptyGallery.addEventListener("click", openGallery);
  document.querySelectorAll("#gallery-modal [data-modal-close]").forEach((el) =>
    el.addEventListener("click", closeGallery)
  );
  document.querySelectorAll("#tool-calls-modal [data-modal-close]").forEach((el) =>
    el.addEventListener("click", closeToolCallsModal)
  );
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !createMenu.hasAttribute("hidden")) {
      setCreateMenu(false, true);
      return;
    }
    if (e.key === "Escape" && document.getElementById("resource-explorer").classList.contains("open")) {
      setExplorer(false, true);
      return;
    }
    if (e.key === "Escape" && !document.getElementById("gallery-modal").hasAttribute("hidden")) {
      closeGallery();
      return;
    }
    if (e.key === "Escape" && !document.getElementById("tool-calls-modal").hasAttribute("hidden")) {
      closeToolCallsModal();
      return;
    }
    // Cancel an in-flight link drag with Escape.
    if (e.key === "Escape" && state.linkFrom) {
      cancelLinkDrag();
      const svg = document.querySelector("#wf-canvas svg.edges");
      if (svg) renderEdges(svg);
      return;
    }
    const active = document.activeElement;
    const tag = active?.tagName || "";
    const isEditing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || active?.isContentEditable;
    const isWorkflow = state.draft?.kind === "Workflow" && state.view === "builder";
    if (isWorkflow && !isEditing && (e.ctrlKey || e.metaKey)) {
      const key = e.key.toLowerCase();
      if (key === "z") {
        e.preventDefault();
        if (e.shiftKey) redoWorkflowCommand(); else undoWorkflowCommand();
        return;
      }
      if (key === "y") { e.preventDefault(); redoWorkflowCommand(); return; }
      if (key === "d" && state.selectedSteps.size > 0) {
        e.preventDefault(); duplicateWorkflowSelection(); return;
      }
      if (key === "a" && active?.closest?.("#wf-canvas")) {
        e.preventDefault(); selectAllWorkflowItems(); return;
      }
    }
    if (isWorkflow && !isEditing && e.key === " " && active?.closest?.("#wf-canvas") && !e.defaultPrevented) {
      e.preventDefault();
      state.canvasSpacePanning = true;
      return;
    }
    // Delete / Backspace removes the current workflow selection — but only when
    // the user isn't editing a text field, and only for the workflow builder.
    if (e.key === "Delete" || e.key === "Backspace") {
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT"
          || (document.activeElement && document.activeElement.isContentEditable)) {
        return;
      }
      if (!state.draft || state.draft.kind !== "Workflow") return;
      if (state.selectedSteps.size === 0 && state.selectedEdges.length === 0) return;
      e.preventDefault();
      deleteWorkflowSelection();
    }
  });
  document.addEventListener("keyup", (e) => {
    if (e.key === " ") state.canvasSpacePanning = false;
  });
  window.addEventListener("blur", () => { state.canvasSpacePanning = false; });

  document.querySelectorAll(".sidebar .refresh").forEach((b) => {
    b.addEventListener("click", () => refreshOne(b.dataset.refresh));
  });

  document.querySelectorAll(".editor-bar-tabs .tab").forEach((b) => {
    b.addEventListener("click", () => switchView(b.dataset.view));
  });

  document.getElementById("btn-deploy").addEventListener("click", deploy);
  document.getElementById("btn-delete").addEventListener("click", deleteResource);
  document.getElementById("btn-discard").addEventListener("click", discardDraft);
  document.getElementById("btn-apply-yaml").addEventListener("click", applyYAML);
  document.getElementById("inspector-close").addEventListener("click", closeInspector);

  window.addEventListener("beforeunload", (e) => {
    if (!state.dirty) return;
    e.preventDefault();
    e.returnValue = "";
  });

  boot();
});

// ============================================================================
// Gallery
// ============================================================================
async function openGallery() {
  const modal = document.getElementById("gallery-modal");
  const grid = document.getElementById("gallery-grid");
  grid.innerHTML = `<div class="muted" style="padding:1rem">Loading gallery…</div>`;
  modal.removeAttribute("hidden");
  try {
    const { items } = await apiGalleryList();
    renderGalleryGrid(items || []);
  } catch (err) {
    grid.innerHTML = `<div class="banner error">Failed to load gallery: ${escapeHTML(err.message)}</div>`;
  }
}
function closeGallery() {
  document.getElementById("gallery-modal").setAttribute("hidden", "");
}

// ============================================================================
// Tool calls modal
// ============================================================================
function openToolCallsModal(details) {
  const modal = document.getElementById("tool-calls-modal");
  const list = document.getElementById("tool-calls-list");
  const subtitle = document.getElementById("tool-calls-modal-subtitle");
  const items = Array.isArray(details) ? details : [];
  if (subtitle) {
    subtitle.textContent = items.length === 1
      ? "1 tool call. Inputs are what the agent passed in; output is what the tool returned."
      : `${items.length} tool calls, in order. Inputs are what the agent passed in; outputs are what the tools returned.`;
  }
  list.innerHTML = "";
  if (!items.length) {
    list.innerHTML = `<div class="muted" style="padding:0.5rem">No tool call details available.</div>`;
  } else {
    items.forEach((call, idx) => list.appendChild(renderToolCallCard(call, idx)));
  }
  modal.removeAttribute("hidden");
}
function closeToolCallsModal() {
  document.getElementById("tool-calls-modal").setAttribute("hidden", "");
}
function renderToolCallCard(call, idx) {
  const card = document.createElement("div");
  card.className = "tool-call-card";
  if (call && call.error) card.classList.add("has-error");

  const header = document.createElement("div");
  header.className = "tool-call-card-header";
  const name = document.createElement("span");
  name.className = "name";
  name.textContent = `${idx + 1}. ${(call && call.name) || "(unnamed tool)"}`;
  header.appendChild(name);
  const meta = document.createElement("span");
  meta.className = "meta";
  if (call && typeof call.duration_ms === "number") {
    meta.textContent = `${call.duration_ms} ms`;
  }
  header.appendChild(meta);
  card.appendChild(header);

  card.appendChild(renderToolCallField("Input", call && call.args));
  card.appendChild(renderToolCallField("Output", call && call.result));
  if (call && call.error) {
    card.appendChild(renderToolCallField("Error", call.error, true));
  }
  return card;
}
function renderToolCallField(label, value, isError) {
  const wrap = document.createElement("div");
  wrap.className = "field";
  const lab = document.createElement("div");
  lab.className = "field-label";
  lab.textContent = label;
  const pre = document.createElement("pre");
  pre.className = "field-value" + (isError ? " error" : "");
  pre.textContent = formatToolCallValue(value);
  wrap.appendChild(lab);
  wrap.appendChild(pre);
  return wrap;
}
function formatToolCallValue(value) {
  if (value === null || value === undefined) return "(empty)";
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch (_err) {
    return String(value);
  }
}
function renderGalleryGrid(items) {
  const grid = document.getElementById("gallery-grid");
  if (!items.length) {
    grid.innerHTML = `<div class="muted" style="padding:1rem">No gallery items installed.</div>`;
    return;
  }
  grid.innerHTML = "";
  for (const item of items) {
    grid.appendChild(renderGalleryCard(item));
  }
}
function renderGalleryCard(item) {
  const card = document.createElement("div");
  card.className = "gallery-card";

  const head = document.createElement("div");
  head.className = "card-head";
  const icon = document.createElement("span");
  icon.className = "card-icon";
  icon.textContent = item.icon || "✨";
  head.appendChild(icon);
  const titleBox = document.createElement("div");
  titleBox.style.flex = "1";
  const title = document.createElement("div");
  title.className = "card-title";
  title.textContent = item.title || item.id;
  titleBox.appendChild(title);
  const kind = document.createElement("div");
  kind.className = "card-kind";
  if (item.primary) kind.textContent = `${item.primary.kind} • ${item.primary.name}`;
  titleBox.appendChild(kind);
  head.appendChild(titleBox);
  card.appendChild(head);

  const desc = document.createElement("div");
  desc.className = "card-desc";
  desc.textContent = item.description || "";
  card.appendChild(desc);

  if (item.tags && item.tags.length) {
    const tags = document.createElement("div");
    tags.className = "tag-row";
    for (const t of item.tags) {
      const chip = document.createElement("span");
      chip.className = "tag";
      chip.textContent = t;
      tags.appendChild(chip);
    }
    card.appendChild(tags);
  }

  if (item.resources && item.resources.length) {
    const res = document.createElement("div");
    res.className = "resource-row";
    res.textContent = "Includes: " + item.resources.map((r) => `${r.kind}/${r.name}`).join(", ");
    card.appendChild(res);
  }

  const actions = document.createElement("div");
  actions.className = "card-actions";
  const addBtn = document.createElement("button");
  addBtn.className = "action primary";
  addBtn.textContent = `+ Add to ${state.namespace || "default"}`;
  addBtn.addEventListener("click", () => importGalleryItem(item, addBtn, card));
  actions.appendChild(addBtn);
  card.appendChild(actions);

  return card;
}
async function importGalleryItem(item, btn, card) {
  if (!confirmDiscard()) return;
  const ns = state.namespace || "default";
  const original = btn.textContent;
  btn.disabled = true;
  btn.textContent = "Adding…";
  // Clear any previous result banner inside this card.
  card.querySelectorAll(".banner").forEach((n) => n.remove());
  let succeeded = false;
  try {
    const result = await apiGalleryImport(item.id, ns);
    const counts = summarizeResults(result.results || []);
    if (counts.errors > 0) {
      const banner = document.createElement("div");
      banner.className = "banner error";
      banner.textContent = `${counts.errors} resource(s) failed. ${counts.created} created, ${counts.exists} already present.`;
      card.appendChild(banner);
      return;
    }
    succeeded = true;
    const msgParts = [];
    if (counts.created) msgParts.push(`${counts.created} created`);
    if (counts.exists) msgParts.push(`${counts.exists} already present`);
    const msg = `Imported "${item.title || item.id}": ${msgParts.join(", ") || "no changes"}.`;
    setStatus(msg, "success");

    try {
      await refreshAll();
    } catch (e) {
      console.warn("refreshAll after import failed", e);
    }
    const primary = result.primary || item.primary;
    if (primary && primary.kind && primary.name) {
      const plural = KIND_PLURAL[primary.kind.toLowerCase()];
      if (plural) {
        try {
          await selectResource(plural, primary.name, ns, true);
        } catch (e) {
          console.warn("selectResource after import failed", e);
        }
      }
    }
    closeGallery();
  } catch (err) {
    const banner = document.createElement("div");
    banner.className = "banner error";
    banner.textContent = (err && err.message) || String(err);
    card.appendChild(banner);
  } finally {
    // Always restore the button so it can never get stuck on "Adding…".
    btn.disabled = false;
    btn.textContent = succeeded ? "Added ✓" : original;
  }
}
function summarizeResults(results) {
  let created = 0, exists = 0, errors = 0;
  for (const r of results) {
    if (r.status === "created") created++;
    else if (r.status === "exists") exists++;
    else errors++;
  }
  return { created, exists, errors };
}
function escapeHTML(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
