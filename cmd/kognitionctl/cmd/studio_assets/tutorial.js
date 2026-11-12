// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const StudioTutorial = (() => {
  const steps = [
    ["practice", "Welcome to safe practice", "This is the actual Studio interface with synthetic resources. Nothing is deployed, no credentials are resolved, and no provider is contacted."],
    ["create", "Create your first Agent", "Use Create > Agent. Give it a valid name and select tutorial-model. The namespace is a virtual practice workspace."],
    ["instructions", "Tell the Agent what to do", "Write a short inline instruction, such as: Answer briefly and suggest one next step. This teaches configuration, not prompt quality."],
    ["yaml", "Connect the form to its manifest", "Open YAML and find your name, model and instructions. Return to Builder. Your draft should be unchanged."],
    ["save", "Save and find your Agent", "Choose Save in practice. Find the Agent in the explorer. Only the synthetic store changes."],
    ["preview", "Try a simulated conversation", "Open Preview, type a message and press Send. The fixed, labeled response demonstrates the UI; it is not real model reasoning."],
    ["challenge", "Try a small change independently", "Return to Builder and change the name or inline instructions. Then answer below. Practice edits are temporary; only lesson progress is remembered."],
  ];
  let practice = false;
  let token = "";
  let progress;
  let paused = false;
  let timer;
  let panel;
  let busy = false;
  let needsCheck = false;
  let pendingAction = "";
  let warning = "";
  let lastAnnouncement = "";
  let viewQueue = Promise.resolve();

  function isPractice() { return practice; }

  async function request(path, options = {}) {
    const url = new URL(path, location.href);
    if (url.origin !== location.origin) throw new Error("Practice cannot contact another origin.");
    const headers = new Headers(options.headers || {});
    headers.delete("Authorization");
    headers.set("X-Kognition-Practice", token);
    if (progress) headers.set("X-Kognition-Practice-Generation", String(progress.generation));
    const response = await fetch(url.href, { ...options, headers, credentials: "omit" });
    if (response.ok && options.method && options.method !== "GET" && !url.pathname.startsWith("/api/tutorial")) {
      changed();
    }
    return response;
  }

  async function configure(meta) {
    if (meta.environment !== "practice") {
      if (meta.tutorialAvailable) {
        document.getElementById("btn-learn").hidden = false;
        if (!meta.tutorialDismissed) welcome(meta.tutorialWarning || "");
      }
      return;
    }
    if (!meta.tutorial?.token || !meta.tutorial.progress) throw new Error("Practice session metadata is incomplete.");
    practice = true;
    token = meta.tutorial.token;
    progress = meta.tutorial.progress;
    warning = meta.tutorial.warning || "";
    document.body.classList.add("practice-mode");
    document.title = "Practice | Kognition Studio";
    document.getElementById("btn-gallery").disabled = true;
    document.getElementById("btn-empty-gallery").closest("p").hidden = true;
    document.querySelector("#empty-state h2").textContent = "Create your first Agent safely.";
    document.querySelector("#empty-state p").textContent = "Follow the lesson panel to try the real Studio controls with practice data.";
    const banner = document.createElement("div");
    banner.id = "practice-banner";
    banner.className = "practice-banner";
    banner.setAttribute("role", "note");
    banner.textContent = "PRACTICE ONLY · Synthetic resources and simulated responses · No live deployment, credentials or paid calls";
    document.querySelector(".editor").prepend(banner);
    buildPanel();
    document.addEventListener("keydown", event => {
      if (event.key !== "Escape" || event.defaultPrevented || !practice) return;
      if (!document.getElementById("create-menu").hidden || document.getElementById("resource-explorer").classList.contains("open")
          || !document.getElementById("gallery-modal").hidden || /INPUT|TEXTAREA|SELECT/.test(document.activeElement?.tagName || "")) return;
      event.preventDefault();
      action("pause");
    }, true);
  }

  function welcome(message) {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const modal = document.createElement("div");
    modal.className = "modal tutorial-welcome-modal";
    modal.setAttribute("role", "dialog");
    modal.setAttribute("aria-modal", "true");
    modal.setAttribute("aria-labelledby", "tutorial-welcome-title");
    modal.setAttribute("aria-describedby", "tutorial-welcome-copy");
    const backdrop = document.createElement("div");
    backdrop.className = "modal-backdrop";
    const card = document.createElement("section");
    card.className = "modal-card tutorial-welcome-card";
    const header = document.createElement("header");
    header.className = "modal-header";
    const heading = document.createElement("h2");
    heading.id = "tutorial-welcome-title";
    heading.textContent = "New to Studio?";
    const closeButton = document.createElement("button");
    closeButton.type = "button";
    closeButton.className = "close";
    closeButton.title = "Close";
    closeButton.setAttribute("aria-label", "Close");
    closeButton.textContent = "✕";
    header.append(heading, closeButton);
    const body = document.createElement("div");
    body.className = "modal-body";
    const description = document.createElement("p");
    description.id = "tutorial-welcome-copy";
    description.textContent = "Learn by creating a first Agent in a separate, isolated practice Studio. Your current work stays here. No backend setup or provider budget is needed.";
    const link = document.createElement("a");
    link.className = "action primary tutorial-start";
    link.href = "/tutorial/start";
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.textContent = "Start tutorial (opens practice tab)";
    const dismiss = document.createElement("button");
    dismiss.type = "button";
    dismiss.className = "action tutorial-not-now";
    dismiss.textContent = "Not now";
    const actions = document.createElement("div");
    actions.className = "tutorial-welcome-actions";
    actions.append(link, dismiss);
    const status = document.createElement("p");
    status.setAttribute("role", "status");
    status.textContent = message;
    body.append(description, actions, status);
    card.append(header, body);
    modal.append(backdrop, card);

    function close() {
      document.removeEventListener("keydown", onKeydown, true);
      modal.remove();
      if (opener?.isConnected) opener.focus();
    }
    function onKeydown(event) {
      if (event.key === "Escape") {
        event.preventDefault();
        event.stopPropagation();
        close();
        return;
      }
      if (event.key !== "Tab") return;
      const focusable = [...modal.querySelectorAll("a[href], button:not([disabled])")];
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (!modal.contains(document.activeElement)) {
        event.preventDefault();
        first.focus();
      } else if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }
    closeButton.addEventListener("click", close);
    backdrop.addEventListener("click", close);
    link.addEventListener("click", close);
    dismiss.addEventListener("click", async () => {
      try {
        const response = await fetch("/api/studio/tutorial-dismiss", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
        if (!response.ok) throw new Error("Could not save your preference. Learn remains available.");
      } catch (error) {
        setStatus(error.message, "error");
      }
      close();
    });
    document.addEventListener("keydown", onKeydown, true);
    document.body.append(modal);
    link.focus();
  }

  function button(id, text, callback) {
    const control = document.createElement("button");
    control.id = id;
    control.type = "button";
    control.className = "action";
    control.textContent = text;
    control.addEventListener("click", callback);
    return control;
  }

  function buildPanel() {
    panel = document.createElement("aside");
    panel.className = "tutorial-panel";
    panel.setAttribute("aria-labelledby", "tutorial-heading");
    const heading = document.createElement("h2");
    heading.id = "tutorial-heading";
    heading.textContent = "Your first Agent";
    const count = document.createElement("p");
    count.id = "tutorial-count";
    const title = document.createElement("h3");
    title.id = "tutorial-step-title";
    const copy = document.createElement("p");
    copy.id = "tutorial-copy";
    const status = document.createElement("p");
    status.id = "tutorial-status";
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    status.setAttribute("aria-atomic", "true");
    const answer = document.createElement("select");
    answer.id = "tutorial-answer";
    answer.setAttribute("aria-label", "Did practice create real resources?");
    answer.append(new Option("Did this create real resources?", ""), new Option("No, only practice data changed", "practice"), new Option("Yes, it deployed real resources", "live"));
    answer.addEventListener("change", changed);
    const controls = document.createElement("div");
    controls.className = "tutorial-controls";
    controls.append(
      button("tutorial-start", "I understand. Start practice", () => action("start")),
      button("tutorial-back", "Back", () => action("back")),
      button("tutorial-next", "Next", () => action("next")),
      button("tutorial-show", "Show me", showTarget),
      button("tutorial-skip", "Skip step", () => action("skip")),
      button("tutorial-pause", "Pause", () => action("pause")),
      button("tutorial-resume", "Resume", () => action("resume")),
      button("tutorial-reset", "Restart lesson", () => {
        if (confirm("Reset practice resources and lesson progress only? Real Studio work is unchanged.")) action("reset");
      }),
      button("tutorial-exit", "Exit practice", () => action("exit"))
    );
    panel.append(heading, count, title, copy, answer, controls, status);
    document.querySelector(".layout").appendChild(panel);
    render();
  }

  function render() {
    if (!practice || !panel) return;
    const [id, title, copy] = steps[progress.cursor];
    const done = progress.completed.includes(id);
    const complete = steps.every(step => progress.completed.includes(step[0]));
    document.getElementById("tutorial-count").textContent = `${progress.completed.length} of ${steps.length} tasks completed${progress.skipped.length ? `; ${progress.skipped.length} skipped` : ""}`;
    document.getElementById("tutorial-step-title").textContent = complete ? "Practice complete" : paused ? "Lesson paused" : `${progress.cursor + 1}. ${title}`;
    document.getElementById("tutorial-copy").textContent = complete
      ? "You created, saved and previewed a practice Agent. No real resources or provider calls were made. Return to Studio when you are ready; memory, autonomy, secret-reference and workflow chapters are planned separately."
      : paused ? "Your place is kept. Resume when you are ready; there is no lesson timer." : copy;
    document.getElementById("tutorial-start").hidden = progress.completed.includes("practice");
    document.getElementById("tutorial-back").disabled = progress.cursor === 0 || paused;
    document.getElementById("tutorial-next").disabled = paused || complete || (!done && !progress.skipped.includes(id));
    document.getElementById("tutorial-show").hidden = progress.cursor === 6 || complete;
    document.getElementById("tutorial-show").disabled = paused;
    document.getElementById("tutorial-skip").disabled = paused || complete || progress.cursor === 0;
    document.getElementById("tutorial-pause").hidden = paused || complete;
    document.getElementById("tutorial-resume").hidden = !paused;
    document.getElementById("tutorial-answer").hidden = progress.cursor !== 6 || paused || complete;
    const feedback = progress.cursor === 6 && document.getElementById("tutorial-answer").value === "live"
      ? "Only practice data changed. No real resource or provider call was made."
      : complete ? "All practice tasks completed. Nothing was deployed." : done ? `${title}: task complete. Choose Next when ready.` : "";
    const announcement = [warning, feedback].filter(Boolean).join(" ");
    if (announcement !== lastAnnouncement) {
      document.getElementById("tutorial-status").textContent = announcement;
      lastAnnouncement = announcement;
    }
  }

  function onRender() {
    if (!practice) return;
    document.getElementById("meta-mode").textContent = "PRACTICE";
    document.getElementById("namespace-select").disabled = true;
    document.getElementById("btn-deploy").textContent = "Save in practice";
    render();
  }

  async function update(actionName, snapshot = {}) {
    const response = await request("/api/tutorial", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: actionName, generation: progress.generation, draft: state.draft, view: state.view, answer: document.getElementById("tutorial-answer").value, ...snapshot }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Practice action failed.");
    if (result.progress.generation < progress.generation ||
        (result.progress.generation === progress.generation && result.progress.revision < progress.revision)) return result;
    progress = result.progress;
    warning = result.warning || "";
    paused = result.paused;
    render();
    return result;
  }

  async function action(name) {
    if (busy) {
      if (name === "check") needsCheck = true;
      else pendingAction = name;
      return;
    }
    busy = true;
    try {
      await update(name);
      if (name === "exit") {
        clearTimeout(timer);
        pendingAction = "";
        needsCheck = false;
        window.close();
        document.getElementById("tutorial-status").textContent = "You can close this tab. The original Studio and its work are unchanged.";
        return;
      }
      if (name === "start" || name === "reset") {
        stopViewerPolling();
        state.draft = null;
        state.draftKey = null;
        state.existsInCluster = false;
        state.dirty = false;
        state.preview.chats = {};
        document.getElementById("tutorial-answer").value = "";
        await refreshAll();
        switchView("builder");
      }
    } catch (error) {
      warning = error.message;
      render();
    } finally {
      busy = false;
      if (pendingAction) { const next = pendingAction; pendingAction = ""; action(next); }
      else if (needsCheck) { needsCheck = false; changed(); }
    }
  }

  function changed() {
    if (!practice) return;
    clearTimeout(timer);
    timer = setTimeout(async () => {
      if (busy) { needsCheck = true; return; }
      await action("check");
    }, 180);
  }

  function viewChanged(view) {
    if (!practice || !progress) return;
    const snapshot = { view, draft: state.draft ? clone(state.draft) : null, generation: progress.generation };
    viewQueue = viewQueue.then(async () => {
      if (snapshot.generation !== progress.generation) return;
      await update("check", snapshot);
    }).catch(error => { warning = error.message; render(); });
  }

  async function restore() {
    if (!practice) return;
    const response = await request("/api/tutorial");
    if (!response.ok) throw new Error("Could not load the practice lesson.");
    const session = await response.json();
    progress = session.progress;
    warning = session.warning || "";
    paused = session.paused;
    if (session.draft) {
      state.draft = session.draft;
      state.existsInCluster = session.existsInPractice === true;
      state.draftKey = state.existsInCluster ? keyOf("agents", state.draft.metadata.name, state.draft.metadata.namespace) : null;
      state.dirty = !state.existsInCluster;
      renderEditor();
      warning = warning || "Resumed practice. Custom text stays only in this temporary session; it is not saved with lesson progress.";
    }
    onRender();
  }

  function showTarget() {
    let target;
    switch (progress.cursor) {
      case 0: target = document.getElementById("tutorial-start"); break;
      case 1:
        setCreateMenu(true);
        target = document.getElementById("btn-new-agent");
        break;
      case 2:
        target = document.querySelector('[data-tutorial-target="instructions"] textarea, #resource-spec-instructions-inline');
        break;
      case 3: target = document.querySelector('[data-view="yaml"]'); break;
      case 4: target = document.getElementById("btn-deploy"); break;
      case 5: target = document.querySelector('[data-view="preview"]'); break;
    }
    if (!target || target.hidden || target.closest("[hidden]")) {
      warning = "The control is not available in this view. Return to the Agent builder, or review the step instructions.";
      render();
      return;
    }
    target.closest("details")?.setAttribute("open", "");
    target.scrollIntoView({ block: "nearest", behavior: "auto" });
    target.focus();
  }

  return { isPractice, request, configure, onRender, changed, viewChanged, restore };
})();
