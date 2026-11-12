// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Kognition dashboard — vanilla JS, no build step.
"use strict";

const state = {
  meta: null,
  current: null, // { plural, singular, label, view? }
  items: [],
  selected: null, // resource object currently shown in detail
  detailTab: "yaml",
  editing: false,
  autoRefreshTimer: null,
  detailTrigger: null,
  route: { focus: false, runId: "", namespace: null },
  focusStale: false,
  focusDisconnected: false,
  runProof: null,
};

const $ = (id) => document.getElementById(id);

async function api(path) {
  const res = await fetch(path);
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!res.ok) {
    const msg = (body && body.error) || res.statusText || `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return body;
}

function setStatus(message, kind) {
  const el = $("status");
  if (!message) { el.hidden = true; el.textContent = ""; el.className = "status"; return; }
  el.hidden = false;
  el.textContent = message;
  el.className = "status " + (kind || "info");
}

function setFooter(s) { $("footer-status").textContent = s; }

function namespaceParam() {
  const ns = ($("namespace").value || "").trim();
  // Empty string explicitly means "all namespaces"; we still pass it so the
  // backend can distinguish from "no preference".
  return `?namespace=${encodeURIComponent(ns)}`;
}

function age(ts) {
  if (!ts) return "—";
  const t = new Date(ts).getTime();
  if (!Number.isFinite(t)) return "—";
  let s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

function phaseOf(obj) {
  const s = obj && obj.status;
  if (!s || typeof s !== "object") return "";
  const candidates = [s.phase, s.state, s.health, s.status];
  for (const v of candidates) {
    if (typeof v === "string" && v) return v;
  }
  if (Array.isArray(s.conditions) && s.conditions.length) {
    const ready = s.conditions.find((c) => c && (c.type === "Ready" || c.type === "Available"));
    if (ready) return `${ready.type}=${ready.status || ""}`.replace(/=$/, "");
    return s.conditions[0].type || "";
  }
  return "";
}

function isEvaluationResource(plural) {
  return plural === "evaluations" || plural === "evaluationruns" || plural === "evaluationdatasets";
}

function isApprovalDecisionResource(plural) {
  return plural === "approvaldecisions";
}

function latestRunSummary(obj) {
  const s = obj && obj.status;
  if (!s || !Array.isArray(s.runHistory) || !s.runHistory.length) return null;
  const ref = s.lastRunRef && s.lastRunRef.name;
  if (ref) {
    const match = s.runHistory.find((r) => r && r.name === ref);
    if (match) return match;
  }
  return s.runHistory[s.runHistory.length - 1];
}

function evaluationSnapshot(obj) {
  const s = (obj && obj.status) || {};
  const latest = latestRunSummary(obj) || {};
  const phase = s.phase || latest.phase || "";
  const score = typeof s.score === "number" ? s.score : (typeof latest.score === "number" ? latest.score : null);
  const metrics = Array.isArray(s.metrics) && s.metrics.length ? s.metrics : (Array.isArray(latest.metrics) ? latest.metrics : []);
  const passed = derivePassed(s, latest, metrics);
  const startedAt = s.startedAt || latest.startedAt || null;
  const completedAt = s.completedAt || latest.completedAt || null;
  const message = s.message || latest.message || "";
  return { phase, score, metrics, passed, startedAt, completedAt, message, latest };
}

function derivePassed(status, latest, metrics) {
  if (typeof status.passed === "boolean") return status.passed;
  if (typeof latest.passed === "boolean") return latest.passed;
  if (Array.isArray(metrics) && metrics.length) return metrics.every((m) => m && m.passed === true);
  if (status.phase === "Failed" || latest.phase === "Failed") return false;
  return null;
}

function resultLabel(snapshot) {
  if (snapshot.passed === true) return "Passed";
  if (snapshot.passed === false) return "Failed";
  return snapshot.phase || "Pending";
}

function scoreText(score) {
  if (typeof score !== "number" || !Number.isFinite(score)) return "—";
  if (score >= 0 && score <= 1) return `${Math.round(score * 100)}%`;
  return String(Math.round(score * 1000) / 1000);
}

function metricValueText(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  if (value >= 0 && value <= 1) return `${Math.round(value * 100)}%`;
  return String(Math.round(value * 1000) / 1000);
}

function durationText(start, end) {
  if (!start || !end) return "—";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)}s`;
  return `${Math.round(ms / 60000)}m`;
}

function summaryOf(obj) {
  const s = obj && obj.status;
  if (!s || typeof s !== "object") return "";
  if (s.message) return String(s.message);
  if (s.url) return String(s.url);
  if (s.replicas != null) return `replicas: ${s.replicas}`;
  if (Array.isArray(s.conditions) && s.conditions.length && s.conditions[0].message) {
    return String(s.conditions[0].message);
  }
  return "";
}

// ---------- Sidebar ----------

function renderSidebar(groups) {
  const nav = $("sidebar");
  nav.innerHTML = "";
  for (const group of groups) {
    const wrap = document.createElement("div");
    wrap.className = "group";
    const t = document.createElement("div");
    t.className = "group-title";
    t.textContent = group.title;
    wrap.appendChild(t);
    const items = document.createElement("div");
    items.className = "group-items";
    for (const it of group.items) {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "group-item";
      b.textContent = it.label;
      b.dataset.plural = it.plural;
      b.dataset.singular = it.singular;
      b.dataset.label = it.label;
      b.dataset.view = it.view || "";
      b.addEventListener("click", () => selectKind(it));
      items.appendChild(b);
    }
    wrap.appendChild(items);
    nav.appendChild(wrap);
  }
}

function markActive(res) {
  document.querySelectorAll(".group-item").forEach((b) => {
    b.classList.toggle("active", res.view ? b.dataset.view === res.view : b.dataset.plural === res.plural);
  });
}

function setInert(selectors, inert) {
  selectors.forEach((selector) => {
    const element = document.querySelector(selector);
    if (element) element.toggleAttribute("inert", inert);
  });
}

function setCatalogOpen(open, restoreFocus = true) {
  if (open && !$("detail").hidden) closeDetail(false);
  document.body.classList.toggle("catalog-open", open);
  $("catalog-toggle").setAttribute("aria-expanded", String(open));
  $("catalog-backdrop").hidden = !open;
  setInert([".controls", ".content", ".footer"], open);
  if (open) {
    const firstItem = $("sidebar").querySelector("button");
    if (firstItem) firstItem.focus();
  } else if (restoreFocus) {
    $("catalog-toggle").focus();
  }
}

function trapFocus(event, container) {
  const focusable = Array.from(container.querySelectorAll("button:not([hidden]):not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])"))
    .filter((element) => element.getClientRects().length > 0);
  if (!focusable.length) return;
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

// ---------- List view ----------

async function selectKind(res) {
  state.current = res;
  markActive(res);
  const isTrustDecisions = res.view === "trust-decisions";
  $("trust-filters").hidden = !isTrustDecisions;
  $("resource-filter").hidden = isTrustDecisions;
  $("trust-overflow").hidden = true;
  setCatalogOpen(false, false);
  $("kind-title").textContent = res.label;
  $("kind-count").textContent = "";
  $("table-wrap").innerHTML = `<p class="empty">Loading ${res.label.toLowerCase()}…</p>`;
  setStatus("");
  closeDetail();
  await refreshList();
}

async function refreshList() {
  if (!state.current) return;
  if (state.current.view === "trust-decisions") {
    await refreshTrustDecisions();
    return;
  }
  const { plural, label } = state.current;
  try {
    setFooter(`fetching ${plural}…`);
    const data = await api(`/api/resources/${plural}${namespaceParam()}`);
    let items = [];
    if (data && Array.isArray(data.items)) items = data.items;
    else if (Array.isArray(data)) items = data;
    state.items = items;
    const visibleItems = applyFilter(items);
    renderList(visibleItems);
    $("kind-count").textContent = `${visibleItems.length} item${visibleItems.length === 1 ? "" : "s"}`;
    setFooter(`updated ${new Date().toLocaleTimeString()}`);
  } catch (e) {
    const msg = /not found|could not find the requested resource/i.test(e.message)
      ? `${label} is not installed in this cluster.`
      : `Failed to load ${label}: ${e.message}`;
    setStatus(msg, "error");
    $("table-wrap").innerHTML = `<p class="empty">No data.</p>`;
    setFooter(`error`);
  }
}

async function refreshTrustDecisions() {
  const ns = ($("namespace").value || "").trim();
  try {
    if (state.route.focus) setTrustFocusConnection("Refreshing", "loading");
    else renderTrustDecisionState("Loading trust decisions…", "loading");
    renderTrustDecisionOverflow(false);
    setFooter("fetching trust decisions…");
    const data = await api(`/api/trust-decisions?${buildTrustDecisionQuery(ns, readTrustDecisionFilters())}`);
    const items = data && Array.isArray(data.items) ? data.items : [];
    state.items = items;
    if (state.route.focus) {
      const reconciliation = reconcileTrustDecisionSelection(state.selected, items);
      state.selected = reconciliation.selected;
      state.focusStale = reconciliation.stale;
      state.focusDisconnected = false;
      $("table-wrap").replaceChildren();
      renderTrustFocus(items);
      await refreshRunProof();
      setTrustFocusConnection("Live", "live");
    } else {
      renderTrustDecisions(items);
    }
    $("kind-count").textContent = `${items.length} decision${items.length === 1 ? "" : "s"}`;
    renderTrustDecisionOverflow(data && data.truncated);
    setStatus("");
    setFooter(`updated ${new Date().toLocaleTimeString()}`);
  } catch (e) {
    if (state.route.focus) {
      state.focusDisconnected = true;
      setTrustFocusConnection("Disconnected", "disconnected");
      setStatus("");
      setFooter("disconnected");
      renderTrustFocus(state.items);
      return;
    }
    setStatus(`Failed to load Trust Decisions: ${e.message}`, "error");
    renderTrustDecisionState("Trust decisions could not be loaded.", "error", true);
    renderTrustDecisionOverflow(false);
    setFooter("error");
  }
}

function readTrustDecisionFilters() {
  return {
    runId: state.route.focus ? state.route.runId : "",
    agentIdentity: $("trust-agent").value.trim(),
    sessionId: $("trust-session").value.trim(),
    plane: $("trust-plane").value,
    outcome: $("trust-outcome").value,
    since: $("trust-since").value,
    until: $("trust-until").value,
    limit: $("trust-limit").value,
  };
}

function parseDashboardRoute(search) {
  const values = new URLSearchParams(search || "");
  const runId = (values.get("runId") || "").trim();
  const focus = values.get("view") === "trust" && values.get("focus") === "1";
  const namespace = values.has("namespace") ? (values.get("namespace") || "").trim() : null;
  return { focus, runId, namespace };
}

function syncDashboardLinks() {
  const namespace = $("namespace").value;
  const includeNamespace = state.route.namespace !== null
    || (state.meta && namespace !== (state.meta.namespace || "default"));
  const control = new URLSearchParams();
  if (state.route.runId) control.set("runId", state.route.runId);
  if (includeNamespace) control.set("namespace", namespace);
  const focus = new URLSearchParams({ view: "trust" });
  if (state.route.runId) focus.set("runId", state.route.runId);
  if (includeNamespace) focus.set("namespace", namespace);
  focus.set("focus", "1");
  $("trust-open-control-plane").href = `/?${control.toString()}`;
  $("trust-open-focus").href = `/?${focus.toString()}`;
}

function syncDashboardURL() {
  const values = new URLSearchParams(window.location.search);
  values.set("namespace", $("namespace").value);
  window.history.replaceState(null, "", `${window.location.pathname}?${values.toString()}${window.location.hash}`);
}

function buildTrustDecisionQuery(namespace, filters) {
  const values = new URLSearchParams({ namespace });
  for (const key of ["runId", "agentIdentity", "sessionId", "plane", "outcome", "limit"]) {
    if (filters[key]) values.set(key, filters[key]);
  }
  for (const key of ["since", "until"]) {
    if (filters[key]) values.set(key, new Date(filters[key]).toISOString());
  }
  return values.toString();
}

function renderTrustDecisionState(message, kind, retry) {
  const paragraph = document.createElement("p");
  paragraph.className = `empty trust-state ${kind || "empty"}`;
  paragraph.textContent = message;
  if (retry) {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = "Retry";
    button.addEventListener("click", () => refreshTrustDecisions());
    paragraph.appendChild(document.createTextNode(" "));
    paragraph.appendChild(button);
  }
  const wrap = $("table-wrap");
  wrap.innerHTML = "";
  wrap.appendChild(paragraph);
}

function renderTrustDecisionOverflow(truncated) {
  const overflow = $("trust-overflow");
  overflow.hidden = !truncated;
  overflow.textContent = truncated ? "Only a bounded set of matching decisions is shown. Narrow the filters or select a namespace to inspect more evidence." : "";
}

function renderTrustDecisions(items) {
  if (!items.length) {
    $("table-wrap").innerHTML = `<p class="empty">No trust decisions in the selected time window.</p>`;
    return;
  }
  const tbl = document.createElement("table");
  tbl.className = "list trust-decisions-list";
  tbl.innerHTML = `<thead><tr>
    <th>Time</th><th>Plane</th><th>Outcome</th><th>Reason</th><th>Identity</th><th>Operation</th><th>Target</th>
  </tr></thead><tbody></tbody>`;
  const tbody = tbl.querySelector("tbody");
  const newestFirst = sortTrustDecisionsNewestFirst(items);
  for (const decision of newestFirst) {
    const tr = document.createElement("tr");
    tr.tabIndex = 0;
    tr.innerHTML = `<td class="decision-time"></td><td></td><td></td><td></td><td></td><td></td><td class="name"></td>`;
    ["Time", "Plane", "Outcome", "Reason", "Identity", "Operation", "Target"].forEach((label, index) => {
      tr.children[index].dataset.label = label;
    });
    tr.children[0].textContent = decisionTimestampText(decision);
    tr.children[1].textContent = evidenceText(decision.plane);
    const outcome = document.createElement("span");
    outcome.className = `decision-outcome ${decisionOutcomeClass(decision.outcome)}`;
    outcome.textContent = evidenceText(decision.outcome);
    tr.children[2].appendChild(outcome);
    tr.children[3].textContent = evidenceText(decision.reasonCode || decision.reason);
    tr.children[4].textContent = evidenceText(decision.identity && decision.identity.agentIdentity);
    tr.children[5].textContent = evidenceText(decision.request && decision.request.operation);
    const target = decision.request && decision.request.targetRef;
    tr.children[6].textContent = target && target.kind && target.name ? `${target.kind}/${target.name}` : "Evidence unavailable";
    const open = () => openTrustDecisionDetail(decision, items, tr);
    tr.addEventListener("click", open);
    tr.addEventListener("keydown", (event) => handleTrustDecisionRowKey(event, tr, open));
    tbody.appendChild(tr);
  }
  const wrap = $("table-wrap");
  wrap.innerHTML = "";
  wrap.appendChild(tbl);
}

function handleTrustDecisionRowKey(event, row, open) {
  if (event.key === "Enter" || event.key === " ") {
    event.preventDefault();
    open();
    return;
  }
  if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
  event.preventDefault();
  const rows = Array.from(row.parentElement.children);
  const offset = event.key === "ArrowDown" ? 1 : -1;
  const target = rows[rows.indexOf(row) + offset];
  if (target) target.focus();
}

function decisionTime(decision) {
  const value = decision && decision.timestamp ? new Date(decision.timestamp).getTime() : NaN;
  return Number.isFinite(value) ? value : null;
}

function decisionTimestampText(decision) {
  const timestamp = decisionTime(decision);
  return timestamp === null ? "Evidence unavailable" : new Date(timestamp).toLocaleString();
}

function sortTrustDecisionsNewestFirst(decisions) {
  return decisions.slice().sort((left, right) => {
    const leftTime = decisionTime(left);
    const rightTime = decisionTime(right);
    if (leftTime === null || rightTime === null) {
      if (leftTime !== null) return -1;
      if (rightTime !== null) return 1;
    } else if (leftTime !== rightTime) {
      return rightTime - leftTime;
    }
    return String(right.id || "").localeCompare(String(left.id || ""));
  });
}

function reconcileTrustDecisionSelection(selected, decisions) {
  if (!selected) {
    return { selected: sortTrustDecisionsNewestFirst(decisions)[0] || null, stale: false };
  }
  const replacement = decisions.find((decision) => decision && decision.id === selected.id);
  return replacement ? { selected: replacement, stale: false } : { selected, stale: true };
}

function setTrustFocusConnection(message, kind) {
  const connection = $("trust-focus-connection");
  connection.textContent = message;
  connection.className = `trust-focus-connection ${kind || ""}`;
}

function buildTraceURL(baseURL, traceID) {
  if (!baseURL || !traceID) return "";
  return `${String(baseURL).replace(/\/+$/, "")}/trace/${encodeURIComponent(traceID)}`;
}

function syncTrustFocusLinks() {
  const observability = (state.meta && state.meta.observability) || {};
  const traceID = state.selected && state.selected.trace && state.selected.trace.traceId;
  const traceURL = buildTraceURL(observability.jaegerUrl, traceID);
  const traceLink = $("trust-open-trace");
  traceLink.hidden = !traceURL;
  if (traceURL) traceLink.href = traceURL;
  else traceLink.removeAttribute("href");

  const metricsLink = $("trust-open-metrics");
  metricsLink.hidden = !observability.grafanaUrl;
  if (observability.grafanaUrl) metricsLink.href = observability.grafanaUrl;
  else metricsLink.removeAttribute("href");
}

function renderTrustFocus(items) {
  const summary = trustExecutionSummary(items);
  const planes = { tool: 0, memory: 0, model: 0, communication: 0 };
  for (const decision of items) {
    if (Object.prototype.hasOwnProperty.call(planes, decision.plane)) planes[decision.plane] += 1;
  }
  document.querySelectorAll("#trust-plane-strip [data-plane]").forEach((plane) => {
    const count = planes[plane.dataset.plane] || 0;
    plane.querySelector("strong").textContent = String(count);
    plane.querySelector("small").textContent = count ? `decision${count === 1 ? "" : "s"}` : "No decisions in current view";
  });
  $("trust-total").textContent = String(summary.events);
  $("trust-reached-target").textContent = String(summary.reachedTarget);
  $("trust-stopped-before-target").textContent = String(summary.stoppedBeforeTarget);
  $("trust-awaiting-approval").textContent = String(summary.awaitingApproval);
  $("trust-stream-count").textContent = `${items.length} events`;

  const stream = $("trust-focus-stream-body");
  stream.innerHTML = "";
  for (const decision of sortTrustDecisionsNewestFirst(items)) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "trust-stream-event";
    button.dataset.decisionId = decision.id || "";
    button.setAttribute("aria-current", String(!!state.selected && state.selected.id === decision.id));
    const meta = document.createElement("span");
    meta.className = "trust-stream-meta";
    meta.textContent = `${decisionTimestampText(decision)} · ${evidenceText(decision.plane)} · ${requestEvidence(decision)}`;
    const title = document.createElement("strong");
    title.textContent = evidenceText(decision.reasonCode || decision.reason);
    const outcome = document.createElement("span");
    outcome.className = `decision-outcome ${decisionOutcomeClass(decision.outcome)}`;
    outcome.textContent = evidenceText(decision.outcome);
    button.appendChild(meta);
    button.appendChild(title);
    button.appendChild(outcome);
    button.addEventListener("click", () => {
      state.selected = decision;
      state.focusStale = false;
      renderTrustFocus(state.items);
    });
    stream.appendChild(button);
  }
  if (!items.length) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = "No decisions recorded for this run.";
    stream.appendChild(empty);
  }

  const evidenceState = $("trust-evidence-state");
  if (state.focusDisconnected) evidenceState.textContent = "Last known evidence · disconnected";
  else if (state.focusStale) evidenceState.textContent = "Stale · absent from latest refresh";
  else evidenceState.textContent = state.selected ? evidenceText(state.selected.id) : "Waiting for evidence";
  syncTrustFocusLinks();
  const evidenceBody = $("trust-focus-evidence-body");
  if (!state.selected) {
    evidenceBody.innerHTML = '<p class="empty">Waiting for the first trust decision.</p>';
    return;
  }
  const related = relatedTrustDecisions(state.selected, items.some((item) => item.id === state.selected.id) ? items : items.concat(state.selected));
  const selectedFirst = [state.selected, ...related.filter((decision) => decision.id !== state.selected.id)];
  renderTrustDecisionTimeline(selectedFirst, evidenceBody);
}

async function refreshRunProof() {
  state.runProof = null;
  if (state.route.focus && state.route.runId) {
    try {
      state.runProof = normalizeRunProof(await api(`/api/run-proof?runId=${encodeURIComponent(state.route.runId)}`), state.route.runId);
    } catch {
      state.runProof = null;
    }
  }
  renderRunProof(state.runProof);
}

function normalizeRunProof(value, expectedRunID) {
  if (!expectedRunID || !value || value.schemaVersion !== 1 || typeof value.runId !== "string" || value.runId !== expectedRunID || value.runId.length > 128 ||
      typeof value.scenario !== "string" || !value.scenario || value.scenario.length > 256 ||
      typeof value.protectedAction !== "string" || !value.protectedAction || value.protectedAction.length > 256 ||
      typeof value.mode !== "string" || !value.mode || value.mode.length > 64 || typeof value.passed !== "boolean" || !value.assertions) return null;
  const { total, passed, failed } = value.assertions;
  if (![total, passed, failed].every((count) => Number.isInteger(count) && count >= 0) || total !== passed + failed) return null;
  return {
    schemaVersion: 1,
    runId: value.runId,
    scenario: value.scenario,
    protectedAction: value.protectedAction,
    mode: value.mode,
    passed: value.passed,
    assertions: { total, passed, failed },
    reportRef: typeof value.reportRef === "string" && /^[^/\\:?#]{1,256}$/.test(value.reportRef) ? value.reportRef : "",
  };
}

function renderRunProof(proof) {
  const panel = $("trust-run-proof");
  panel.hidden = !proof;
  if (!proof) return;
  $("trust-run-proof-scenario").textContent = proof.scenario;
  $("trust-run-proof-action").textContent = proof.protectedAction;
  $("trust-run-proof-mode").textContent = proof.mode;
  $("trust-run-proof-assertions").textContent = `${proof.assertions.passed}/${proof.assertions.total} passed`;
  const status = $("trust-run-proof-status");
  status.textContent = proof.passed ? "Passed" : "Failed";
  status.className = `decision-outcome ${proof.passed ? "allowed" : "blocked"}`;
  $("trust-run-proof-reference-wrap").hidden = !proof.reportRef;
  $("trust-run-proof-reference").textContent = proof.reportRef;
}

function trustExecutionSummary(items) {
  const summary = { events: items.length, reachedTarget: 0, stoppedBeforeTarget: 0, awaitingApproval: 0 };
  for (const decision of items) {
    if (decision.execution && decision.execution.targetReached === true) summary.reachedTarget += 1;
    if (decision.execution && decision.execution.targetReached === false) summary.stoppedBeforeTarget += 1;
    if (decision.approval && decision.approval.status === "pending") summary.awaitingApproval += 1;
  }
  return summary;
}

function decisionGroupKey(decision) {
  const sessionID = decision && decision.session && decision.session.sessionId;
  const invocationID = decision && decision.request && decision.request.invocationId;
  if (!sessionID && !invocationID) return `decision\u0000${decision && decision.id ? decision.id : "unknown"}`;
  return `${sessionID || ""}\u0000${invocationID || ""}`;
}

function openTrustDecisionDetail(selected, decisions, trigger) {
  setCatalogOpen(false, false);
  state.selected = selected;
  state.detailTrigger = trigger || null;
  state.editing = false;
  const related = relatedTrustDecisions(selected, decisions);
  const sessionID = selected.session && selected.session.sessionId;
  const invocationID = selected.request && selected.request.invocationId;
  $("detail-title").textContent = invocationID || selected.id || "Trust decision";
  $("detail-subtitle").textContent = `Session ${evidenceText(sessionID)} · ${related.length} decision${related.length === 1 ? "" : "s"}`;
  $("detail").classList.add("trust-detail");
  $("detail").hidden = false;
  document.body.classList.add("detail-open");
  $("detail-backdrop").hidden = false;
  setInert([".topbar", "main", ".footer"], true);
  document.querySelectorAll(".detail-tabs .tab").forEach((tab) => { tab.hidden = true; });
  $("detail-edit").hidden = true;
  $("detail-save").hidden = true;
  $("detail-cancel").hidden = true;
  renderTrustDecisionTimeline(related);
  $("detail-close").focus();
}

function relatedTrustDecisions(selected, decisions) {
  return decisions
    .filter((decision) => decisionGroupKey(decision) === decisionGroupKey(selected))
    .sort((left, right) => {
      const leftTime = decisionTime(left);
      const rightTime = decisionTime(right);
      if (leftTime === null || rightTime === null) {
        if (leftTime !== null) return -1;
        if (rightTime !== null) return 1;
      } else if (leftTime !== rightTime) {
        return leftTime - rightTime;
      }
      return String(left.id || "").localeCompare(String(right.id || ""));
    });
}

function renderTrustDecisionTimeline(decisions, target) {
  const timeline = document.createElement("div");
  timeline.className = "trust-timeline";
  for (const decision of decisions) {
    const event = document.createElement("article");
    event.className = "trust-event";
    const heading = document.createElement("header");
    heading.className = "trust-event-header";
    const time = document.createElement("time");
    time.textContent = decisionTimestampText(decision);
    const outcome = document.createElement("span");
    outcome.className = `decision-outcome ${decisionOutcomeClass(decision.outcome)}`;
    outcome.textContent = evidenceText(decision.outcome);
    heading.appendChild(time);
    heading.appendChild(outcome);
    event.appendChild(heading);
    const reason = document.createElement("p");
    reason.className = "trust-reason";
    reason.textContent = `Reason: ${evidenceText(decision.reasonCode || decision.reason)}`;
    event.appendChild(reason);

    const evidence = document.createElement("dl");
    evidence.className = "trust-evidence";
    const inProcess = decision.plane === "model" && decision.session && decision.session.authorityMode === "in_process"
      && !decision.session.capabilityId && !decision.identity
      && decision.authorization && decision.authorization.identity === "not_required"
      && decision.agentRef && decision.agentRef.kind === "Agent" && decision.agentRef.name
      && decision.agentRef.namespace === decision.workspace && decision.agentRef.uid && decision.agentRef.resourceVersion;
    appendEvidence(evidence, inProcess ? "Agent attribution" : "Identity", inProcess ? "In-process execution" : decision.identity && decision.identity.agentIdentity, [
      inProcess && decision.agentRef,
      decision.identity && decision.identity.agentIdentityRef,
      decision.claimedIdentityRef,
    ]);
    appendEvidence(evidence, "Session", decision.session && decision.session.sessionId, [decision.session && decision.session.instanceRef]);
    appendEvidence(evidence, "Capability ID", inProcess ? "Not applicable (in-process Model call)" : decision.session && decision.session.capabilityId);
    appendEvidence(evidence, "Request", requestEvidence(decision), [
      decision.request && decision.request.targetRef,
      decision.request && decision.request.constraintRef,
    ]);
    appendAuthorizationEvidence(evidence, decision.authorization);
    appendEvidence(evidence, "Approval evidence", approvalEvidence(decision.approval), [decision.approval && decision.approval.decisionRef]);
    appendEvidence(evidence, "Execution outcome", executionEvidence(decision.execution));
    appendEvidence(evidence, "Trace correlation", traceEvidence(decision.trace));
    event.appendChild(evidence);
    timeline.appendChild(event);
  }
  const body = target || $("detail-body");
  body.innerHTML = "";
  body.appendChild(timeline);
}

function appendEvidence(list, label, value, refs) {
  const term = document.createElement("dt");
  term.textContent = label;
  const detail = document.createElement("dd");
  detail.textContent = evidenceText(value);
  for (const ref of (refs || []).filter((candidate) => candidate && candidate.kind && candidate.name)) {
    const link = document.createElement("button");
    link.type = "button";
    link.className = "resource-ref";
    link.textContent = `${ref.kind}/${ref.name}`;
    link.addEventListener("click", () => navigateResourceRef(ref));
    detail.appendChild(document.createTextNode(" "));
    detail.appendChild(link);
  }
  list.appendChild(term);
  list.appendChild(detail);
}

function evidenceText(value) {
  if (value === null || value === undefined || value === "") return "Evidence unavailable";
  return String(value);
}

function requestEvidence(decision) {
  const request = decision && decision.request;
  if (!request) return "";
  const target = request.targetRef;
  const targetText = target && target.kind && target.name ? `${target.kind}/${target.name}` : "Evidence unavailable";
  return `${evidenceText(request.operation)} · ${targetText}`;
}

function authorizationEvidence(authorization) {
  if (!authorization) return "";
  return ["identity", "role", "grant", "policy"]
    .map((check) => `${check}: ${evidenceText(authorization[check])}`)
    .join(" · ");
}

function appendAuthorizationEvidence(evidence, authorization) {
  for (const check of ["identity", "role", "grant", "policy"]) {
    const refs = [];
    if (check === "grant") refs.push(authorization && authorization.grantRef);
    if (check === "policy") refs.push(authorization && authorization.policyRef);
    appendEvidence(evidence, `Authorization · ${check}`, authorization && authorization[check], refs);
  }
}

function approvalEvidence(approval) {
  if (approval && approval.status === "not_evaluated") return "Not evaluated";
  return approval && approval.status ? approval.status : "";
}

function executionEvidence(execution) {
  if (!execution) return "";
  const reached = typeof execution.targetReached === "boolean" ? String(execution.targetReached) : "Evidence unavailable";
  return `${evidenceText(execution.status)} · target reached: ${reached}`;
}

function traceEvidence(trace) {
  if (!trace || (!trace.traceId && !trace.spanId)) return "";
  return `trace: ${evidenceText(trace.traceId)} · span: ${evidenceText(trace.spanId)}`;
}

async function navigateResourceRef(ref) {
  const kind = String(ref.kind || "").toLowerCase();
  const resource = state.meta && state.meta.groups
    .flatMap((group) => group.items)
    .find((item) => item.singular === kind);
  if (!resource) {
    setStatus(`${ref.kind}/${ref.name} is not available in this dashboard catalog.`, "info");
    return;
  }
  if (ref.namespace) $("namespace").value = ref.namespace;
  await selectKind(resource);
  const match = state.items.find((item) => item && item.metadata && item.metadata.name === ref.name);
  if (match) openDetail(match);
  else setStatus(`${ref.kind}/${ref.name} is not available in the selected namespace.`, "info");
}

function decisionOutcomeClass(outcome) {
  if (outcome === "allowed" || outcome === "approved" || outcome === "completed") return "allowed";
  if (outcome === "denied" || outcome === "revoked" || outcome === "replayed" || outcome === "rejected" || outcome === "failed") return "blocked";
  if (outcome === "pending") return "pending";
  return "neutral";
}

function applyFilter(items) {
  const q = $("filter").value.trim().toLowerCase();
  if (!q) return items;
  return items.filter((it) => {
    const name = it && it.metadata && it.metadata.name;
    return typeof name === "string" && name.toLowerCase().includes(q);
  });
}

function renderList(items) {
  if (!items.length) {
    $("table-wrap").innerHTML = `<p class="empty">No items.</p>`;
    return;
  }
  if (state.current && isApprovalDecisionResource(state.current.plural)) {
    renderApprovalDecisionList(items);
    return;
  }
  if (state.current && isEvaluationResource(state.current.plural)) {
    renderEvaluationList(items);
    return;
  }
  const tbl = document.createElement("table");
  tbl.className = "list";
  tbl.innerHTML = `<thead><tr>
    <th>Name</th><th>Phase</th><th>Summary</th><th class="age">Age</th>
  </tr></thead><tbody></tbody>`;
  const tbody = tbl.querySelector("tbody");
  for (const obj of items) {
    const meta = (obj && obj.metadata) || {};
    const name = meta.name || "—";
    const created = meta.creationTimestamp || meta.createdAt || meta.created_at || "";
    const ph = phaseOf(obj);
    const sm = summaryOf(obj);
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td class="name"></td>
      <td></td>
      <td></td>
      <td class="age"></td>
    `;
    tr.children[0].textContent = name;
    if (ph) {
      const span = document.createElement("span");
      span.className = `phase ${ph.toLowerCase().replace(/[^a-z0-9]+/g, "-")}`;
      span.textContent = ph;
      tr.children[1].appendChild(span);
    }
    tr.children[2].textContent = sm;
    tr.children[3].textContent = age(created);
    ["Name", "Phase", "Summary", "Age"].forEach((label, index) => { tr.children[index].dataset.label = label; });
    makeResourceRowInteractive(tr, obj);
    tbody.appendChild(tr);
  }
  const wrap = $("table-wrap");
  wrap.innerHTML = "";
  wrap.appendChild(tbl);
}

function renderApprovalDecisionList(items) {
  const tbl = document.createElement("table");
  tbl.className = "list approval-decision-list";
  tbl.innerHTML = `<thead><tr>
    <th>Name</th><th>Decision</th><th>Workflow / step</th><th>Approver / reason</th><th class="age">Age</th>
  </tr></thead><tbody></tbody>`;
  const tbody = tbl.querySelector("tbody");
  for (const obj of items) {
    const meta = (obj && obj.metadata) || {};
    const spec = (obj && obj.spec) || {};
    const decision = String(spec.decision || "pending").toLowerCase();
    const tr = document.createElement("tr");
    tr.innerHTML = `<td class="name"></td><td></td><td></td><td></td><td class="age"></td>`;
    tr.children[0].textContent = meta.name || "—";
    const badge = document.createElement("span");
    const outcome = decision === "approve" ? "approved" : decision === "deny" ? "denied" : decision;
    badge.className = `decision-outcome ${decisionOutcomeClass(outcome)}`;
    badge.textContent = decision === "approve" ? "Approved" : decision === "deny" ? "Denied" : decision || "Pending";
    tr.children[1].appendChild(badge);
    const workflow = spec.workflowRunRef && spec.workflowRunRef.name ? spec.workflowRunRef.name : "—";
    const iteration = Number(spec.iteration || 0);
    appendCellContext(tr.children[2], workflow, spec.step ? `${spec.step}${iteration ? ` · iteration ${iteration}` : ""}` : "—");
    appendCellContext(tr.children[3], spec.approver || "—", spec.reason || "No reason recorded");
    tr.children[4].textContent = age(meta.creationTimestamp || meta.createdAt || meta.created_at || "");
    ["Name", "Decision", "Workflow / step", "Approver / reason", "Age"].forEach((label, index) => { tr.children[index].dataset.label = label; });
    makeResourceRowInteractive(tr, obj);
    tbody.appendChild(tr);
  }
  const wrap = $("table-wrap");
  wrap.innerHTML = "";
  wrap.appendChild(tbl);
}

function appendCellContext(cell, primary, secondary) {
  const main = document.createElement("span");
  main.className = "cell-primary";
  main.textContent = primary;
  const detail = document.createElement("span");
  detail.className = "cell-secondary";
  detail.textContent = secondary;
  cell.append(main, detail);
}

function renderEvaluationList(items) {
  const tbl = document.createElement("table");
  tbl.className = "list evaluation-list";
  tbl.innerHTML = `<thead><tr>
    <th>Name</th><th>Result</th><th>Score</th><th>Metrics</th><th>Duration</th><th class="age">Age</th>
  </tr></thead><tbody></tbody>`;
  const tbody = tbl.querySelector("tbody");
  for (const obj of items) {
    const meta = (obj && obj.metadata) || {};
    const snapshot = evaluationSnapshot(obj);
    const name = meta.name || "—";
    const created = meta.creationTimestamp || meta.createdAt || meta.created_at || "";
    const tr = document.createElement("tr");
    tr.innerHTML = `<td class="name"></td><td></td><td></td><td></td><td></td><td class="age"></td>`;
    tr.children[0].textContent = name;
    const badge = document.createElement("span");
    badge.className = `result-badge ${resultClass(snapshot)}`;
    badge.textContent = resultLabel(snapshot);
    tr.children[1].appendChild(badge);
    tr.children[2].textContent = scoreText(snapshot.score);
    tr.children[3].textContent = metricSummary(snapshot.metrics);
    tr.children[4].textContent = durationText(snapshot.startedAt, snapshot.completedAt);
    tr.children[5].textContent = age(created);
    ["Name", "Result", "Score", "Metrics", "Duration", "Age"].forEach((label, index) => { tr.children[index].dataset.label = label; });
    makeResourceRowInteractive(tr, obj);
    tbody.appendChild(tr);
  }
  const wrap = $("table-wrap");
  wrap.innerHTML = "";
  wrap.appendChild(tbl);
}

function makeResourceRowInteractive(row, resource) {
  row.tabIndex = 0;
  const open = () => openDetail(resource, row);
  row.addEventListener("click", open);
  row.addEventListener("keydown", (event) => handleTrustDecisionRowKey(event, row, open));
}

function resultClass(snapshot) {
  if (snapshot.passed === true) return "pass";
  if (snapshot.passed === false || snapshot.phase === "Failed") return "fail";
  if (snapshot.phase === "Running") return "running";
  return "pending";
}

function metricSummary(metrics) {
  if (!Array.isArray(metrics) || !metrics.length) return "—";
  const passed = metrics.filter((m) => m && m.passed).length;
  return `${passed}/${metrics.length} passing`;
}

// ---------- Detail drawer ----------

function openDetail(obj, trigger) {
  setCatalogOpen(false, false);
  state.selected = obj;
  state.detailTrigger = trigger || null;
  state.editing = false;
  const meta = (obj && obj.metadata) || {};
  const kind = obj && obj.kind ? obj.kind : (state.current ? state.current.label : "Resource");
  $("detail-title").textContent = meta.name || "—";
  $("detail-subtitle").textContent = `${kind} · ${meta.namespace || "—"}`;
  $("detail").classList.remove("trust-detail");
  document.querySelectorAll(".detail-tabs .tab").forEach((tab) => { tab.hidden = false; });
  $("detail").hidden = false;
  document.body.classList.add("detail-open");
  $("detail-backdrop").hidden = false;
  setInert([".topbar", "main", ".footer"], true);
  showDetailTab(state.detailTab);
  $("detail-close").focus();
}

function closeDetail(restoreFocus = true) {
  state.editing = false;
  $("detail").hidden = true;
  document.body.classList.remove("detail-open");
  $("detail-backdrop").hidden = true;
  setInert([".topbar", "main", ".footer"], false);
  $("detail").classList.remove("trust-detail");
  document.querySelectorAll(".detail-tabs .tab").forEach((tab) => { tab.hidden = false; });
  state.selected = null;
  syncEditButtons();
  if (restoreFocus && state.detailTrigger && state.detailTrigger.isConnected) state.detailTrigger.focus();
  state.detailTrigger = null;
}

async function showDetailTab(tab) {
  // Switching tabs always exits edit mode without saving.
  if (state.editing) state.editing = false;
  state.detailTab = tab;
  document.querySelectorAll(".detail-tabs .tab").forEach((t) => {
    t.classList.toggle("active", t.dataset.tab === tab);
  });
  syncEditButtons();
  const body = $("detail-body");
  if (!state.selected) { body.innerHTML = `<pre>no selection</pre>`; return; }

  if (tab === "yaml") {
    body.innerHTML = `<pre id="detail-pre">${escapeHtml(toYaml(state.selected))}</pre>`;
    return;
  }
  if (tab === "results") {
    renderResultsTab(body, state.selected);
    return;
  }
  if (tab === "json") {
    body.innerHTML = `<pre id="detail-pre">${escapeHtml(JSON.stringify(state.selected, null, 2))}</pre>`;
    return;
  }
  if (tab === "events") {
    body.innerHTML = `<pre>loading events…</pre>`;
    try {
      const meta = state.selected.metadata || {};
      const kind = (state.current && state.current.plural) || "agents";
      const data = await api(`/api/events/${kind}/${encodeURIComponent(meta.name)}${namespaceParam()}`);
      const items = (data && data.items) || [];
      if (!items.length) { body.innerHTML = `<p class="empty">No events.</p>`; return; }
      const list = document.createElement("div");
      list.className = "events-list";
      for (const ev of items) {
        const div = document.createElement("div");
        div.className = "event";
        const reason = ev.reason || ev.type || "Event";
        const ts = ev.timestamp || ev.createdAt || "";
        const msg = ev.message || "";
        const head = document.createElement("div");
        head.className = "ev-head";
        const r = document.createElement("span");
        r.className = "ev-reason";
        r.textContent = reason;
        head.appendChild(r);
        if (ev.type) {
          const t = document.createElement("span");
          t.className = "phase " + String(ev.type).toLowerCase();
          t.textContent = ev.type;
          head.appendChild(t);
        }
        const time = document.createElement("span");
        time.className = "ev-time";
        time.textContent = ts;
        head.appendChild(time);
        div.appendChild(head);
        if (msg) {
          const m = document.createElement("div");
          m.className = "ev-msg";
          m.textContent = msg;
          div.appendChild(m);
        }
        list.appendChild(div);
      }
      body.innerHTML = "";
      body.appendChild(list);
    } catch (e) {
      body.innerHTML = `<pre>error: ${escapeHtml(e.message)}</pre>`;
    }
  }
}

function renderResultsTab(body, obj) {
  if (!state.current || !isEvaluationResource(state.current.plural)) {
    body.innerHTML = `<p class="empty">No result summary for this resource kind.</p>`;
    return;
  }
  const snapshot = evaluationSnapshot(obj);
  const status = (obj && obj.status) || {};
  const spec = (obj && obj.spec) || {};
  const wrap = document.createElement("div");
  wrap.className = "results-view";
  wrap.appendChild(resultCards(snapshot));
  wrap.appendChild(metricsPanel(snapshot.metrics));
  if (Array.isArray(status.caseResults) && status.caseResults.length) {
    wrap.appendChild(caseResultsPanel(status.caseResults));
  }
  if (Array.isArray(status.runHistory) && status.runHistory.length) {
    wrap.appendChild(runHistoryPanel(status.runHistory, status.lastRunRef && status.lastRunRef.name));
  }
  if (Array.isArray(spec.metricThresholds) && spec.metricThresholds.length) {
    wrap.appendChild(thresholdPanel(spec.metricThresholds));
  }
  if (status.targetOutput) {
    const panel = document.createElement("section");
    panel.className = "result-panel";
    panel.innerHTML = `<h4>First Target Output</h4>`;
    const pre = document.createElement("pre");
    pre.className = "target-output";
    pre.textContent = status.targetOutput;
    panel.appendChild(pre);
    wrap.appendChild(panel);
  }
  body.innerHTML = "";
  body.appendChild(wrap);
}

function caseResultsPanel(caseResults) {
  const panel = document.createElement("section");
  panel.className = "result-panel";
  panel.innerHTML = `<h4>Case Results</h4>`;
  for (const result of caseResults) {
    const row = document.createElement("div");
    row.className = "metric-row";
    const name = document.createElement("div");
    name.className = "metric-name";
    name.textContent = result.id || "case";
    const output = document.createElement("div");
    output.className = "metric-value";
    output.textContent = result.targetOutput ? String(result.targetOutput).split("\n", 1)[0] : "—";
    const badge = document.createElement("span");
    badge.className = `result-badge ${result.passed ? "pass" : "fail"}`;
    badge.textContent = result.passed ? "pass" : "fail";
    row.appendChild(name);
    row.appendChild(output);
    row.appendChild(badge);
    if (result.message) {
      const msg = document.createElement("div");
      msg.className = "metric-message";
      msg.textContent = result.message;
      row.appendChild(msg);
    }
    panel.appendChild(row);
  }
  return panel;
}

function resultCards(snapshot) {
  const cards = document.createElement("div");
  cards.className = "result-cards";
  const values = [
    ["Result", resultLabel(snapshot), resultClass(snapshot)],
    ["Score", scoreText(snapshot.score), ""],
    ["Metrics", metricSummary(snapshot.metrics), ""],
    ["Duration", durationText(snapshot.startedAt, snapshot.completedAt), ""],
  ];
  for (const [label, value, cls] of values) {
    const card = document.createElement("div");
    card.className = "result-card";
    const k = document.createElement("div");
    k.className = "result-card-label";
    k.textContent = label;
    const v = document.createElement("div");
    v.className = cls ? `result-card-value ${cls}` : "result-card-value";
    v.textContent = value;
    card.appendChild(k);
    card.appendChild(v);
    cards.appendChild(card);
  }
  if (snapshot.message) {
    const msg = document.createElement("div");
    msg.className = "result-message";
    msg.textContent = snapshot.message;
    cards.appendChild(msg);
  }
  return cards;
}

function metricsPanel(metrics) {
  const panel = document.createElement("section");
  panel.className = "result-panel";
  panel.innerHTML = `<h4>Metrics</h4>`;
  if (!Array.isArray(metrics) || !metrics.length) {
    const empty = document.createElement("p");
    empty.className = "empty compact";
    empty.textContent = "No metric results recorded yet.";
    panel.appendChild(empty);
    return panel;
  }
  for (const metric of metrics) {
    const row = document.createElement("div");
    row.className = "metric-row";
    const name = document.createElement("div");
    name.className = "metric-name";
    name.textContent = metric.metric || "metric";
    const value = document.createElement("div");
    value.className = "metric-value";
    value.textContent = metricValueText(metric.value);
    const badge = document.createElement("span");
    badge.className = `result-badge ${metric.passed ? "pass" : "fail"}`;
    badge.textContent = metric.passed ? "pass" : "fail";
    const bar = document.createElement("div");
    bar.className = "metric-bar";
    const fill = document.createElement("span");
    const width = typeof metric.value === "number" && metric.value >= 0 && metric.value <= 1 ? metric.value * 100 : (metric.passed ? 100 : 0);
    fill.style.width = `${Math.max(0, Math.min(100, width))}%`;
    bar.appendChild(fill);
    row.appendChild(name);
    row.appendChild(value);
    row.appendChild(badge);
    row.appendChild(bar);
    if (metric.message) {
      const msg = document.createElement("div");
      msg.className = "metric-message";
      msg.textContent = metric.message;
      row.appendChild(msg);
    }
    panel.appendChild(row);
  }
  return panel;
}

function runHistoryPanel(history, activeName) {
  const panel = document.createElement("section");
  panel.className = "result-panel";
  panel.innerHTML = `<h4>Run History</h4>`;
  const list = document.createElement("div");
  list.className = "run-history";
  for (const run of history.slice().reverse()) {
    const item = document.createElement("div");
    item.className = "run-history-item";
    const name = document.createElement("div");
    name.className = "run-history-name";
    name.textContent = `${run.name || "run"}${run.name === activeName ? " · latest" : ""}`;
    const meta = document.createElement("div");
    meta.className = "run-history-meta";
    meta.textContent = `${run.phase || "—"} · ${scoreText(run.score)} · ${durationText(run.startedAt, run.completedAt)}`;
    item.appendChild(name);
    item.appendChild(meta);
    list.appendChild(item);
  }
  panel.appendChild(list);
  return panel;
}

function thresholdPanel(thresholds) {
  const panel = document.createElement("section");
  panel.className = "result-panel";
  panel.innerHTML = `<h4>Thresholds</h4>`;
  const list = document.createElement("div");
  list.className = "threshold-list";
  for (const t of thresholds) {
    const item = document.createElement("div");
    item.className = "threshold-item";
    const parts = [];
    if (t.min != null) parts.push(`min ${t.min}`);
    if (t.max != null) parts.push(`max ${t.max}`);
    if (t.required) parts.push("required");
    item.textContent = `${t.metric || "metric"}: ${parts.join(", ") || "observed"}`;
    list.appendChild(item);
  }
  panel.appendChild(list);
  return panel;
}

function escapeHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

// Minimal JSON-to-YAML converter (no anchors/tags; handles strings/numbers/bools/null/arrays/objects).
function toYaml(value, indent = 0) {
  const pad = "  ".repeat(indent);
  if (value === null || value === undefined) return "null";
  if (typeof value === "boolean" || typeof value === "number") return String(value);
  if (typeof value === "string") return yamlScalar(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value.map((v) => `${pad}- ${yamlInline(v, indent + 1)}`).join("\n");
  }
  if (typeof value === "object") {
    const keys = Object.keys(value);
    if (keys.length === 0) return "{}";
    return keys.map((k) => {
      const v = value[k];
      const key = yamlKey(k);
      if (v === null || v === undefined) return `${pad}${key}: null`;
      if (typeof v === "object") {
        if (Array.isArray(v) && v.length === 0) return `${pad}${key}: []`;
        if (!Array.isArray(v) && Object.keys(v).length === 0) return `${pad}${key}: {}`;
        return `${pad}${key}:\n${toYaml(v, indent + 1)}`;
      }
      return `${pad}${key}: ${yamlScalar(v)}`;
    }).join("\n");
  }
  return String(value);
}

function yamlInline(v, indent) {
  if (v === null) return "null";
  if (typeof v === "object") {
    const block = toYaml(v, indent);
    if (Array.isArray(v)) return `\n${block}`;
    // For an object inside an array, format as a nested map under the dash.
    const lines = block.split("\n");
    const first = lines.shift();
    const trimmedFirst = first.replace(/^\s+/, "");
    const rest = lines.length ? "\n" + lines.join("\n") : "";
    return `${trimmedFirst}${rest}`;
  }
  return yamlScalar(v);
}

function yamlScalar(s) {
  if (typeof s === "number" || typeof s === "boolean") return String(s);
  const str = String(s);
  if (str === "") return `""`;
  if (/[\n"]/.test(str) || /^\s|\s$/.test(str)) {
    return JSON.stringify(str);
  }
  if (/^(true|false|null|yes|no|on|off|~)$/i.test(str)) return JSON.stringify(str);
  if (/^-?\d+(\.\d+)?$/.test(str)) return JSON.stringify(str);
  if (/^[\[\]{}&*!|>%@`,#:?-]/.test(str)) return JSON.stringify(str);
  return str;
}

function yamlKey(k) {
  if (/^[A-Za-z0-9_.\-/]+$/.test(k)) return k;
  return JSON.stringify(k);
}

// ---------- Inline manifest editor ----------

function syncEditButtons() {
  const editable = !!state.selected && (state.detailTab === "yaml" || state.detailTab === "json");
  const editBtn = $("detail-edit");
  const saveBtn = $("detail-save");
  const cancelBtn = $("detail-cancel");
  if (!editBtn || !saveBtn || !cancelBtn) return;
  if (state.editing) {
    editBtn.hidden = true;
    saveBtn.hidden = false;
    cancelBtn.hidden = false;
  } else {
    editBtn.hidden = false;
    saveBtn.hidden = true;
    cancelBtn.hidden = true;
    editBtn.disabled = !editable;
  }
  // Disable tabs while editing to avoid silently discarding changes.
  document.querySelectorAll(".detail-tabs .tab").forEach((t) => {
    t.disabled = state.editing && t.dataset.tab !== state.detailTab;
  });
}

function startEdit() {
  if (!state.selected) return;
  if (state.detailTab !== "yaml" && state.detailTab !== "json") return;
  const body = $("detail-body");
  const text = state.detailTab === "yaml"
    ? toYaml(state.selected)
    : JSON.stringify(state.selected, null, 2);
  const ta = document.createElement("textarea");
  ta.id = "detail-edit-area";
  ta.className = "manifest-editor";
  ta.spellcheck = false;
  ta.autocapitalize = "off";
  ta.autocomplete = "off";
  ta.value = text;
  body.innerHTML = "";
  body.appendChild(ta);
  ta.focus();
  state.editing = true;
  syncEditButtons();
}

function cancelEdit() {
  if (!state.editing) return;
  state.editing = false;
  // Re-render the read-only view from the cached selected object.
  showDetailTab(state.detailTab);
}

async function saveEdit() {
  if (!state.editing || !state.selected) return;
  const ta = $("detail-edit-area");
  if (!ta) return;
  const tab = state.detailTab;
  const plural = (state.current && state.current.plural) || "agents";
  const meta = state.selected.metadata || {};
  const name = meta.name;
  if (!name) { setStatus("cannot save: missing metadata.name", "error"); return; }

  const saveBtn = $("detail-save");
  const cancelBtn = $("detail-cancel");
  saveBtn.disabled = true;
  cancelBtn.disabled = true;
  setStatus("saving…");

  try {
    const res = await fetch(`/api/resources/${encodeURIComponent(plural)}/${encodeURIComponent(name)}${namespaceParam()}`, {
      method: "PUT",
      headers: { "Content-Type": tab === "yaml" ? "application/yaml" : "application/json" },
      body: ta.value,
    });
    const text = await res.text();
    let body = null;
    try { body = text ? JSON.parse(text) : null; } catch { body = text; }
    if (!res.ok) {
      const msg = (body && body.error) || res.statusText || `HTTP ${res.status}`;
      throw new Error(msg);
    }
    // Update local cache with whatever the server returned.
    if (body && typeof body === "object") state.selected = body;
    state.editing = false;
    showDetailTab(tab);
    setStatus(`${(state.selected.kind || plural).toLowerCase()}/${name} updated`, "info");
    // Refresh the list in case the change affects rendered columns.
    if (state.current) refreshList();
  } catch (e) {
    setStatus(`save failed: ${e.message}`, "error");
  } finally {
    saveBtn.disabled = false;
    cancelBtn.disabled = false;
    syncEditButtons();
  }
}

// ---------- Auto-refresh ----------

function syncAutoRefresh() {
  const on = $("auto-refresh").checked;
  if (state.autoRefreshTimer) {
    clearInterval(state.autoRefreshTimer);
    state.autoRefreshTimer = null;
  }
  if (on) {
    state.autoRefreshTimer = setInterval(() => {
      if (state.current) refreshList();
    }, 5000);
  }
}

// ---------- Bootstrap ----------

async function bootstrap() {
  try {
    const meta = await api("/api/meta");
    state.meta = meta;
    const initialNamespace = state.route.namespace === null
      ? ((state.route.focus && meta.mode === "kubernetes") ? "" : (meta.namespace || "default"))
      : state.route.namespace;
    const auditNamespace = typeof meta.auditNamespace === "string" ? meta.auditNamespace.trim() : "";
    $("mode-badge").textContent = `mode: ${meta.mode}${auditNamespace ? ` · audit: ${auditNamespace}` : ""}`;
    renderSidebar(meta.groups);
    await populateNamespaceOptions(initialNamespace);
    if (state.route.namespace === null && state.route.focus && meta.mode === "kubernetes") {
      state.route.namespace = "";
      syncDashboardURL();
    }
    syncDashboardLinks();
    setFooter("ready");
    if (state.route.focus) {
      document.body.classList.add("trust-focus-mode");
      $("trust-focus").hidden = false;
      $("trust-focus-filter-host").appendChild($("trust-filters"));
      $("trust-focus-run").textContent = state.route.runId ? `Run ${state.route.runId}` : "All runs";
      const trustView = meta.groups.flatMap((group) => group.items).find((item) => item.view === "trust-decisions")
        || { label: "Trust Decisions", view: "trust-decisions" };
      await selectKind(trustView);
    }
  } catch (e) {
    setStatus(`Failed to load dashboard metadata: ${e.message}`, "error");
    setFooter("error");
  }
}

async function populateNamespaceOptions(preferredNamespace) {
  const sel = $("namespace");
  if (!sel) return;
  const current = preferredNamespace !== undefined
    ? preferredNamespace
    : (sel.value || (state.meta && state.meta.namespace) || "default");
  let list = [];
  try {
    const res = await api("/api/namespaces");
    if (Array.isArray(res)) list = res;
  } catch (_) { /* non-fatal */ }
  sel.innerHTML = "";
  if (state.meta && state.meta.mode === "kubernetes") {
    const allOpt = document.createElement("option");
    allOpt.value = "";
    allOpt.textContent = "All namespaces";
    sel.appendChild(allOpt);
  }
  const seen = new Set();
  for (const ns of list) {
    if (seen.has(ns)) continue;
    seen.add(ns);
    const o = document.createElement("option");
    o.value = ns;
    o.textContent = ns;
    sel.appendChild(o);
  }
  if (current && !seen.has(current) && current !== "") {
    const o = document.createElement("option");
    o.value = current;
    o.textContent = current;
    sel.appendChild(o);
  }
  sel.value = current || ((state.meta && state.meta.mode === "kubernetes") ? "" : (state.meta && state.meta.namespace) || "default");
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    approvalEvidence,
    authorizationEvidence,
    buildTraceURL,
    buildTrustDecisionQuery,
    decisionOutcomeClass,
    decisionGroupKey,
    evidenceText,
    executionEvidence,
    handleTrustDecisionRowKey,
    parseDashboardRoute,
    normalizeRunProof,
    reconcileTrustDecisionSelection,
    relatedTrustDecisions,
    renderTrustDecisionTimeline,
    requestEvidence,
    sortTrustDecisionsNewestFirst,
    traceEvidence,
    trustExecutionSummary,
  };
}

if (typeof document !== "undefined") document.addEventListener("DOMContentLoaded", () => {
  state.route = parseDashboardRoute(typeof window === "undefined" ? "" : window.location.search);
  syncDashboardLinks();
  $("trust-open-focus").hidden = state.route.focus;
  $("refresh").addEventListener("click", () => refreshList());
  $("auto-refresh").addEventListener("change", syncAutoRefresh);
  $("namespace").addEventListener("change", () => {
    state.route.namespace = $("namespace").value;
    syncDashboardURL();
    syncDashboardLinks();
    refreshList();
  });
  $("catalog-toggle").addEventListener("click", () => {
    const open = !document.body.classList.contains("catalog-open");
    setCatalogOpen(open);
  });
  $("catalog-backdrop").addEventListener("click", () => setCatalogOpen(false));
  $("trust-filters").addEventListener("submit", (event) => {
    event.preventDefault();
    refreshTrustDecisions();
  });
  $("trust-filter-reset").addEventListener("click", () => {
    $("trust-filters").reset();
    refreshTrustDecisions();
  });
  $("filter").addEventListener("input", () => {
    if (state.current && !state.current.view) renderList(applyFilter(state.items));
  });
  $("detail-close").addEventListener("click", closeDetail);
  $("detail-backdrop").addEventListener("click", closeDetail);
  $("detail-edit").addEventListener("click", startEdit);
  $("detail-save").addEventListener("click", saveEdit);
  $("detail-cancel").addEventListener("click", cancelEdit);
  document.querySelectorAll(".detail-tabs .tab").forEach((t) => {
    t.addEventListener("click", () => showDetailTab(t.dataset.tab));
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Tab") {
      if (!$("detail").hidden) trapFocus(e, $("detail"));
      else if (document.body.classList.contains("catalog-open")) trapFocus(e, $("sidebar"));
    }
    if (e.key === "Escape") {
      if (state.editing) cancelEdit();
      else if (!$("detail").hidden) closeDetail();
      else if (document.body.classList.contains("catalog-open")) setCatalogOpen(false);
    }
  });
  bootstrap();
});
