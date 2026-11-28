// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { test, expect } = require("@playwright/test");
const { prepareReviewedEditor, selectRecordedResource, observedResourceView, recordingYAML } = require("../../../../tools/recording/live-production-session");
const { expandBuilderObjects, builderSection } = require("../../../../tools/recording/live-instructional");

function builderField(page, label) {
  return page.locator("#builder-host").getByLabel(new RegExp(`^${label}\\s*\\*?$`));
}

async function openResource(page, plural, name) {
  if (await page.locator("#btn-explorer").isVisible()) await page.locator("#btn-explorer").click();
  const list = page.locator(`#list-${plural}`);
  const details = list.locator("..");
  if (await details.getAttribute("open") === null) await details.locator("summary").click();
  await list.getByText(name, { exact: true }).click();
}

async function authoringAPI(page, { mode = "standalone", namespace = "default", records = [], tutorial = null } = {}) {
  const mutations = [];
  const dismissals = [];
  const stored = new Map(records.map(record => [record.metadata.name, record]));
  await page.route("**/api/**", async route => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { mode, namespace, ...(tutorial ? { tutorialAvailable: true, tutorialDismissed: tutorial.dismissed === true } : {}) } });
    }
    if (url.pathname === "/api/studio/tutorial-dismiss") {
      dismissals.push(request.method());
      return tutorial?.failDismiss ? route.fulfill({ status: 500, json: { error: "boom" } }) : route.fulfill({ json: { dismissed: true } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default", "team-a"] });
    if (url.pathname === "/api/parse-yaml") return route.fulfill({ json: JSON.parse(request.postData()) });
    if (url.pathname === "/api/studio/profile-digest") return route.fulfill({ json: { digest: `sha256:${"a".repeat(64)}` } });
    const match = url.pathname.match(/^\/api\/resources\/([^/]+)(?:\/([^/]+))?$/);
    if (!match) return route.fulfill({ status: 404, json: { error: "not found" } });
    if (["POST", "PUT"].includes(request.method())) {
      const body = request.postDataJSON();
      mutations.push({ method: request.method(), plural: match[1], namespace: url.searchParams.get("namespace"), body });
      body.metadata.resourceVersion = 2;
      stored.set(body.metadata.name, body);
      return route.fulfill({ json: body });
    }
    if (match[2]) {
      const record = stored.get(decodeURIComponent(match[2]));
      return record ? route.fulfill({ json: record }) : route.fulfill({ status: 404, json: { error: "not found" } });
    }
    const kinds = {
      agents: "Agent", workflows: "Workflow",
      tools: "Tool", toolgrants: "ToolGrant", knowledgebases: "KnowledgeBase",
      autonomyclasses: "AutonomyClass", memoryclaims: "MemoryClaim", secretrefs: "SecretRef", prompts: "Prompt",
      memoryclasses: "MemoryClass", modelinvocationprofiles: "ModelInvocationProfile", memoryrecords: "MemoryRecord",
      evaluationruns: "EvaluationRun", workflowruns: "WorkflowRun", externalagents: "ExternalAgent",
    };
    return route.fulfill({ json: { items: [...stored.values()].filter(record => record.kind === kinds[match[1]]) } });
  });
  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  return tutorial ? Object.assign(mutations, { dismissals }) : mutations;
}

test.describe("first-run welcome", () => {
  const welcome = page => page.getByRole("dialog", { name: "New to Studio?" });

  test("is a modal dialog above the page and its buttons receive clicks", async ({ page }) => {
    await authoringAPI(page, { mode: "kubernetes", tutorial: {} });
    const dialog = welcome(page);
    await expect(dialog).toBeVisible();
    await expect(dialog).toHaveAttribute("aria-modal", "true");
    const geometry = await page.evaluate(() => {
      const modal = document.querySelector(".tutorial-welcome-modal");
      const card = modal.querySelector(".modal-card");
      const hit = selector => {
        const element = modal.querySelector(selector);
        const box = element.getBoundingClientRect();
        const top = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
        return element === top || element.contains(top);
      };
      const cover = modal.getBoundingClientRect();
      const background = getComputedStyle(card).backgroundColor;
      return {
        position: getComputedStyle(modal).position, parent: modal.parentElement.tagName,
        covers: cover.width >= innerWidth && cover.height >= innerHeight,
        opaque: !/rgba\(0, 0, 0, 0\)|transparent/.test(background),
        start: hit(".tutorial-start"), notNow: hit(".tutorial-not-now"), close: hit(".close"),
      };
    });
    expect(geometry).toEqual({ position: "fixed", parent: "BODY", covers: true, opaque: true, start: true, notNow: true, close: true });
    await expect(page.locator("#empty-state")).toBeVisible();
  });

  test("Not now records the preference, closes, and stays closed after reload", async ({ page }) => {
    const calls = await authoringAPI(page, { mode: "kubernetes", tutorial: {} });
    await welcome(page).getByRole("button", { name: "Not now" }).click();
    await expect(welcome(page)).toHaveCount(0);
    expect(calls.dismissals).toEqual(["POST"]);
    await page.unroute("**/api/**");
    await authoringAPI(page, { mode: "kubernetes", tutorial: { dismissed: true } });
    await expect(welcome(page)).toHaveCount(0);
    await expect(page.locator("#btn-learn")).toBeVisible();
  });

  test("close button, Escape and backdrop close it for this visit only", async ({ page }) => {
    const calls = await authoringAPI(page, { mode: "kubernetes", tutorial: {} });
    for (const close of [
      () => welcome(page).getByRole("button", { name: "Close" }).click(),
      () => page.keyboard.press("Escape"),
      () => page.locator(".tutorial-welcome-modal .modal-backdrop").click({ position: { x: 4, y: 4 } }),
    ]) {
      await expect(welcome(page)).toBeVisible();
      await expect(welcome(page).getByRole("link", { name: /Start tutorial/ })).toBeFocused();
      await close();
      await expect(welcome(page)).toHaveCount(0);
      await page.reload();
      await expect(page.locator("#status")).toHaveText("ready");
    }
    expect(calls.dismissals).toEqual([]);
  });

  test("Start tutorial opens practice in a new tab and closes the welcome", async ({ page }) => {
    await authoringAPI(page, { mode: "kubernetes", tutorial: {} });
    const link = welcome(page).getByRole("link", { name: /Start tutorial/ });
    await expect(link).toHaveAttribute("href", "/tutorial/start");
    await expect(link).toHaveAttribute("target", "_blank");
    const popup = page.waitForEvent("popup");
    await link.click();
    expect((await popup).url()).toContain("/tutorial/start");
    await expect(welcome(page)).toHaveCount(0);
  });

  test("keeps Tab focus inside the dialog", async ({ page }) => {
    await authoringAPI(page, { mode: "kubernetes", tutorial: {} });
    await expect(welcome(page)).toBeVisible();
    for (let press = 0; press < 8; press++) {
      await page.keyboard.press("Tab");
      expect(await page.evaluate(() => !!document.activeElement.closest(".tutorial-welcome-modal"))).toBe(true);
    }
    for (let press = 0; press < 8; press++) {
      await page.keyboard.press("Shift+Tab");
      expect(await page.evaluate(() => !!document.activeElement.closest(".tutorial-welcome-modal"))).toBe(true);
    }
  });

  test("closes even when the preference cannot be saved and says so", async ({ page }) => {
    await authoringAPI(page, { mode: "kubernetes", tutorial: { failDismiss: true } });
    await welcome(page).getByRole("button", { name: "Not now" }).click();
    await expect(welcome(page)).toHaveCount(0);
    await expect(page.locator("#status")).toContainText("Could not save your preference");
  });
});

test("reviewed manifest recording preserves Workflow intent and fractional Evaluation thresholds", async ({ page }) => {
  const mutations = await authoringAPI(page, { mode: "kubernetes" });
  const fixtures = [
    { apiVersion: "kognition.dev/v1alpha1", kind: "Workflow", metadata: { name: "reviewed-workflow", namespace: "default" },
      spec: { steps: [{ name: "calculate", type: "tool", toolRef: { name: "calculator" },
        agentRef: { name: "principal" }, input: '{"expression":"17 * 23"}' }], maxConcurrentRuns: 1 } },
    { apiVersion: "kognition.dev/v1alpha1", kind: "Evaluation", metadata: { name: "reviewed-evaluation", namespace: "default" },
      spec: { targetRef: { kind: "Agent", name: "target" }, datasetRef: { name: "cases", version: "v1" },
        graders: [{ name: "success", type: "task_success" }],
        metricThresholds: [{ metric: "task_success", min: 0.8, required: true }] } },
  ];
  for (const resource of fixtures) {
    await prepareReviewedEditor(page, resource.kind, true);
    await page.locator('[data-view="yaml"]').click();
    await page.locator("#yaml-area").fill(JSON.stringify(resource));
    await page.getByRole("button", { name: "Apply YAML", exact: true }).click();
    await expect(page.locator("#status")).toHaveText("YAML applied to builder");
    await expect(page.locator("#builder-host :invalid")).toHaveCount(0);
    const before = mutations.length;
    await page.locator("#btn-deploy").click();
    await expect.poll(() => mutations.length).toBe(before + 1);
    expect(mutations.at(-1).body.spec).toEqual(resource.spec);
    await expect(page.locator("#status")).toHaveText(`deployed ${resource.kind} ${resource.metadata.name}`);
  }
});

test("Workflow Builder leaves shared Memory unset until configured and clears it without invalid empty references", async ({ page }) => {
  const mutations = await authoringAPI(page, { mode: "kubernetes", records: [
    { apiVersion: "kognition.dev/v1alpha1", kind: "Agent", metadata: { name: "planner", namespace: "default" }, spec: {} },
    { apiVersion: "kognition.dev/v1alpha1", kind: "MemoryClass", metadata: { name: "default" }, spec: {} },
    { apiVersion: "kognition.dev/v1alpha1", kind: "MemoryClaim", metadata: { name: "existing-claim", namespace: "default" }, spec: {} },
  ] });
  await page.locator("#btn-create").click();
  await page.locator("#btn-new-workflow").click();
  await builderField(page, "Name").fill("without-shared-memory");
  await page.getByRole("button", { name: "+ Agent", exact: true }).first().click();
  await page.locator("#inspector-body").getByLabel(/^Agent\s*\*?$/).selectOption("planner");
  await page.locator('[data-view="yaml"]').click();
  await expect(page.locator("#yaml-area")).not.toHaveValue(/memoryClaim:/);
  await page.locator('[data-view="builder"]').click();
  const memory = builderSection(page, "Memory");
  await memory.locator("summary").click();
  const classes = memory.getByLabel("MemoryClass", { exact: true });
  await expect(classes.locator('option[value="default"]')).toHaveCount(1);
  await expect(classes.locator('option[value="existing-claim"]')).toHaveCount(0);
  await classes.selectOption("default");
  await memory.getByLabel("AccessMode", { exact: true }).selectOption("ReadWrite");
  await page.locator('[data-view="yaml"]').click();
  await expect(page.locator("#yaml-area")).toHaveValue(/memoryClassName: default/);
  await page.locator('[data-view="builder"]').click();
  if (await memory.getAttribute("open") === null) await memory.locator("summary").click();
  await classes.selectOption("");
  await memory.getByLabel("AccessMode", { exact: true }).selectOption("");
  const override = page.locator("#inspector-body details").filter({
    has: page.locator("summary").filter({ hasText: /^Memory claim override$/ }),
  });
  await override.locator("summary").click();
  await override.getByLabel("MemoryClass", { exact: true }).selectOption("default");
  await override.getByLabel("AccessMode", { exact: true }).selectOption("ReadWrite");
  await page.locator('[data-view="yaml"]').click();
  await expect(page.locator("#yaml-area")).toHaveValue(/memoryClassName: default/);
  await page.locator('[data-view="builder"]').click();
  if (await override.getAttribute("open") === null) await override.locator("summary").click();
  await override.getByLabel("MemoryClass", { exact: true }).selectOption("");
  await override.getByLabel("AccessMode", { exact: true }).selectOption("");
  await page.locator("#inspector-close").click();
  await page.getByRole("button", { name: "All API fields", exact: true }).click();
  await expandBuilderObjects(page);
  await page.locator("#btn-deploy").click();
  await expect.poll(() => mutations.length).toBe(1);
  await expect(page.locator("#builder-host :invalid")).toHaveCount(0);
  expect(mutations[0].body.spec.memoryClaim).toBeUndefined();
  expect(mutations[0].body.spec.steps[0].memoryClaim).toBeUndefined();
  expect(mutations[0].body.spec.steps[0].agentRef.name).toBe("planner");
});

const teamAgents = ["analyst", "reviewer", "synthesizer"].map(name => ({
  apiVersion: "kognition.dev/v1alpha1", kind: "Agent", metadata: { name, namespace: "default" }, spec: {},
}));

async function newTeamStep(page, name, opts = {}) {
  const mutations = await authoringAPI(page, { mode: "kubernetes", records: teamAgents, ...opts });
  await page.locator("#btn-create").click();
  await page.locator("#btn-new-workflow").click();
  await builderField(page, "Name").fill(name);
  await page.getByRole("button", { name: "+ Team step", exact: true }).click();
  await page.locator("#inspector-body").getByLabel("maxContentBytes", { exact: true }).fill("131072");
  return { mutations, inspector: page.locator("#inspector-body"), yaml: page.locator("#yaml-area") };
}

async function teamDeployed(page, mutations) {
  await expect(page.locator("#btn-deploy")).toBeEnabled();
  await page.locator("#btn-deploy").click();
  await expect.poll(() => mutations.length).toBe(1);
  return mutations[0].body.spec.steps[0].collaboration;
}

test("Workflow Team Parallel step edits bounded branches and deploys on the Kubernetes runtime", async ({ page }) => {
  const { mutations, inspector } = await newTeamStep(page, "request-review");
  await inspector.getByLabel(/^Agent for member-1/).selectOption("analyst");
  await inspector.getByLabel(/^Agent for member-2/).selectOption("reviewer");
  await inspector.getByLabel("Optional branch for member-2", { exact: true }).check();
  await inspector.getByRole("button", { name: "+ Add member", exact: true }).click();
  await inspector.getByLabel(/^Agent for member-3/).selectOption("synthesizer");
  await inspector.getByLabel("Synthesizer", { exact: true }).selectOption("member-3");
  await expect(inspector.getByRole("checkbox", { name: "Optional branch for member-3" })).toHaveCount(0);
  await page.locator('[data-view="yaml"]').click();
  const yaml = page.locator("#yaml-area");
  await expect(yaml).toHaveValue(/type: collaboration[\s\S]*timeout: "5m"[\s\S]*strategy: Parallel\s+protocol: A2A/);
  await expect(yaml).toHaveValue(/branches:\s+- member: member-1\s+- member: member-2\s+optional: true\s+synthesizer: member-3/);
  await expect(yaml).toHaveValue(/maxParallelTasks: 2\s+maxTokens: 8192/);
  await expect(yaml).not.toHaveValue(/allowedInteractions|toolRef|retry:/);
  await page.locator('[data-view="builder"]').click();
  const team = await teamDeployed(page, mutations);
  expect(team.strategy).toBe("Parallel");
  expect(team.parallel).toEqual({ branches: [{ member: "member-1" }, { member: "member-2", optional: true }], synthesizer: "member-3" });
  expect(team.allowedInteractions).toBeUndefined();
  expect(team.limits.maxContentBytes).toBe(131072);
  await inspector.getByLabel("Type", { exact: true }).selectOption("agent");
  await inspector.getByLabel(/^Agent\s*\*?$/).selectOption("reviewer");
  await page.locator('[data-view="yaml"]').click();
  await expect(yaml).not.toHaveValue(/collaboration|parallel:/);
});

test("Workflow Team Supervisor step delegates from the coordinator and keeps renames consistent", async ({ page }) => {
  const { mutations, inspector, yaml } = await newTeamStep(page, "supervised");
  await inspector.getByLabel("Strategy", { exact: true }).selectOption("Supervisor");
  await inspector.getByRole("button", { name: "+ Add member", exact: true }).click();
  await inspector.getByLabel(/^Agent for member-1/).selectOption("analyst");
  await inspector.getByLabel(/^Agent for member-2/).selectOption("reviewer");
  await inspector.getByLabel(/^Agent for member-3/).selectOption("synthesizer");
  await inspector.getByLabel("Name of member-1", { exact: true }).fill("lead");
  await inspector.getByLabel("Name of member-1", { exact: true }).blur();
  await expect(inspector.getByLabel(/^Coordinator\s*\*?$/)).toHaveValue("lead");
  const team = await teamDeployed(page, mutations);
  expect(team.strategy).toBe("Supervisor");
  expect(team.supervisor).toEqual({ coordinator: "lead" });
  expect(team.allowedInteractions).toEqual([{ from: "lead", to: "member-2" }, { from: "lead", to: "member-3" }]);
  expect(team.parallel).toBeUndefined();
  await page.locator('[data-view="yaml"]').click();
  await expect(yaml).toHaveValue(/strategy: Supervisor/);
});

test("Workflow Team member rename keeps focus on the next field", async ({ page }) => {
  const { inspector } = await newTeamStep(page, "focus");
  await inspector.getByRole("button", { name: "+ Add member", exact: true }).click();
  const name = inspector.getByLabel("Name of member-1", { exact: true });
  await name.fill("lead");
  await page.keyboard.press("Tab");
  await expect(inspector.getByLabel("Name of lead", { exact: true })).toHaveValue("lead");
  await page.keyboard.type("Plans the work");
  await expect(inspector.getByLabel(/^Description of lead/)).toHaveValue("Plans the work");
});

test("Workflow Team Handoff and Peer steps generate their interaction graph", async ({ page }) => {
  const { mutations, inspector } = await newTeamStep(page, "handoff-flow");
  await inspector.getByLabel("Strategy", { exact: true }).selectOption("Handoff");
  await inspector.getByLabel(/^Agent for member-1/).selectOption("analyst");
  await inspector.getByLabel(/^Agent for member-2/).selectOption("reviewer");
  await expect(inspector.getByLabel(/^Entry member\s*\*?$/)).toHaveValue("member-1");
  await inspector.getByLabel("Interaction 1 to", { exact: true }).selectOption("member-2");
  await inspector.getByLabel("Strategy", { exact: true }).selectOption("Peer");
  await expect(inspector.getByLabel(/^Finalizer\s*\*?$/)).toHaveValue("member-1");
  await expect(inspector.getByLabel("Interaction 2 from", { exact: true })).toHaveValue("member-2");
  const team = await teamDeployed(page, mutations);
  expect(team.strategy).toBe("Peer");
  expect(team.peer).toEqual({ finalizer: "member-1" });
  expect(team.allowedInteractions).toEqual([{ from: "member-1", to: "member-2" }, { from: "member-2", to: "member-1" }]);
  expect(team.handoff).toBeUndefined();
});

test("Workflow Team problems panel reports admission-rule violations", async ({ page }) => {
  const { inspector } = await newTeamStep(page, "incomplete-team");
  await inspector.getByLabel("Strategy", { exact: true }).selectOption("Handoff");
  await inspector.getByLabel("maxHandoffs", { exact: true }).fill("3");
  // The mobile inspector overlays the graph tabs; dispatch the click directly.
  await page.locator('[data-graph-panel="problems"]').dispatchEvent("click");
  const panel = page.locator("#workflow-graph-panel");
  await expect(panel.getByText(/maxHandoffs must be between 1 and limits.maxDelegations/)).toBeVisible();
  await expect(panel.getByText(/member "member-1" needs an agent/)).toBeVisible();
});

test("Workflow Team requires an explicit user-defined content byte limit", async ({ page }) => {
  const { mutations, inspector } = await newTeamStep(page, "content-limit");
  await inspector.getByLabel(/^Agent for member-1/).selectOption("analyst");
  await inspector.getByLabel(/^Agent for member-2/).selectOption("reviewer");
  const limit = inspector.getByLabel("maxContentBytes", { exact: true });
  for (const invalid of ["", "0", "-1"]) {
    await limit.fill(invalid);
    await page.locator("#btn-deploy").click();
    await expect(page.locator("#status")).toContainText("limits.maxContentBytes");
    expect(mutations).toHaveLength(0);
  }
  await limit.fill("262144");
  const team = await teamDeployed(page, mutations);
  expect(team.limits.maxContentBytes).toBe(262144);
});

test("Workflow Team member can reference an ExternalAgent and drops agent-only fields", async ({ page }) => {
  const remote = { apiVersion: "kognition.dev/v1alpha1", kind: "ExternalAgent", metadata: { name: "partner", namespace: "default" }, spec: {} };
  const { mutations, inspector } = await newTeamStep(page, "with-remote", { records: [...teamAgents, remote] });
  await inspector.getByLabel(/^Agent for member-1/).selectOption("analyst");
  await inspector.getByLabel("Kind of member-2", { exact: true }).selectOption("external");
  await expect(inspector.getByLabel(/^Agent for member-2/)).toHaveCount(0);
  await expect(inspector.getByLabel("AgentService for member-2", { exact: true })).toHaveCount(0);
  await expect(inspector.getByText(/Prompt data is sent to the remote endpoint/)).toBeVisible();
  await inspector.getByLabel(/^External agent for member-2/).selectOption("partner");
  const team = await teamDeployed(page, mutations);
  expect(team.members[0]).toEqual({ name: "member-1", agentRef: { name: "analyst" } });
  expect(team.members[1]).toEqual({ name: "member-2", externalAgentRef: { name: "partner" } });
});

test("Workflow Team problems panel reports incomplete and duplicate external members", async ({ page }) => {
  const { inspector } = await newTeamStep(page, "bad-remote");
  await inspector.getByLabel("Kind of member-1", { exact: true }).selectOption("external");
  await inspector.getByLabel("Kind of member-2", { exact: true }).selectOption("external");
  await page.locator('[data-graph-panel="problems"]').dispatchEvent("click");
  const panel = page.locator("#workflow-graph-panel");
  await expect(panel.getByText(/member "member-1" needs an external agent/)).toBeVisible();
  await expect(panel.getByText(/member "member-2" needs an external agent/)).toBeVisible();
});

test("Workflow Team steps can be drafted but not deployed outside the Kubernetes runtime", async ({ page }) => {
  const mutations = await authoringAPI(page, { mode: "standalone", records: teamAgents });
  await page.locator("#btn-create").click();
  await page.locator("#btn-new-workflow").click();
  await builderField(page, "Name").fill("draft-only");
  await page.getByRole("button", { name: "+ Team step", exact: true }).click();
  await expect(page.locator("#btn-deploy")).toBeDisabled();
  await expect(page.locator("#inspector-body").getByText(/run only on the Kubernetes runtime/)).toBeVisible();
  await page.locator('[data-view="preview"]').click();
  await expect(page.getByText("Team steps run only on the Kubernetes runtime.", { exact: true })).toBeVisible();
  expect(mutations).toEqual([]);
});

test("recording manifests use readable native YAML without changing quoted data, false, zero or multiline instructions", async ({ page }) => {
  await authoringAPI(page);
  const resource = { apiVersion: "kognition.dev/v1alpha1", kind: "Agent",
    metadata: { name: "yaml-recording", namespace: "default" }, spec: {
      instructions: { inline: "First line.\nSecond line: preserve this.\n" },
      runtime: { maxTokens: 256 }, replicas: 0, enabled: false,
      literal: '{"expression":"17 * 23"}', version: "1.0", empty: [] } };
  await prepareReviewedEditor(page, "Agent", true);
  await page.locator('[data-view="yaml"]').click();
  const text = await recordingYAML(page, resource);
  expect(text).toMatch(/^apiVersion: kognition.dev\/v1alpha1\nkind: Agent\n/);
  expect(text).not.toMatch(/^\s*\{/);
  expect(text).toContain("replicas: 0");
  expect(text).toContain("enabled: false");
  expect(text).toContain("inline: |");
  await page.locator("#yaml-area").fill(text);
  await expect(page.locator("#yaml-area")).toHaveValue(text);
  expect(await page.evaluate(text => toYaml({
    apiVersion: "kognition.dev/v1alpha1", kind: "Agent", spec: { value: text },
  }), resource.spec.literal)).toContain('value: "{\\"expression\\"');
});

test("instructional recorder expands lazy Builder fields without skipping shifting details", async ({ page }) => {
  await authoringAPI(page);
  await createResource(page, "#btn-new-prompt");
  await expandBuilderObjects(page);
  await expect(page.locator('#builder-host details.schema-object:not([open])')).toHaveCount(0);
  await page.locator('[data-path="metadata.name"]').fill("builder-prompt");
  await page.locator('[data-path="spec.content"]').fill("Be concise.");
  await page.getByRole("textbox", { name: "Labels entry key", exact: true }).fill("kognition.dev/recording-run");
  await page.locator('details.schema-object').filter({ has: page.locator('summary').filter({ hasText: /^Labels$/ }) })
    .getByRole("button", { name: "Add entry", exact: true }).click();
  await page.locator('[data-path="metadata.labels.kognition.dev/recording-run"]').fill("builder-test");
  await page.locator('[data-view="yaml"]').click();
  await expect(page.locator("#yaml-area")).toHaveValue(/content: Be concise\./);
});
test("recording selects newly generated runs through a real read-only sidebar refresh", async ({ page }) => {
  const mutations = await authoringAPI(page, { mode: "kubernetes" });
  for (const [kind, plural] of [["EvaluationRun", "evaluationruns"], ["WorkflowRun", "workflowruns"]]) {
    const name = `generated-${plural}`;
    const resource = { apiVersion: "kognition.dev/v1alpha1", kind,
      metadata: { name, namespace: "default", uid: `${kind}-uid` },
      spec: {}, status: { phase: "Failed", message: "Expected controlled negative fixture.",
        conditions: [{ type: "Ready", status: "False", reason: "ControlledNegative", message: "Expected controlled negative fixture." }] } };
    const created = await page.evaluate(async ({ plural, resource }) => {
      const response = await fetch(`/api/resources/${plural}?namespace=default`, {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(resource),
      });
      return response.ok;
    }, { plural, resource });
    expect(created).toBe(true);
    const before = mutations.length;
    await selectRecordedResource(page, plural, name);
    await expect(page.locator("#editor-title")).toContainText(name);
    await expect(observedResourceView(page, plural)).toHaveCount(1);
    await expect(observedResourceView(page, plural)).toContainText("Expected controlled negative fixture.");
    expect(mutations.length).toBe(before);
  }
});

test("instructional recorder opens an advanced Tool Builder section by its visible title", async ({ page }) => {
  await authoringAPI(page);
  await createResource(page, "#btn-new-tool");
  const section = builderSection(page, "Legacy");
  await expect(section).toHaveCount(1);
  await section.locator("summary").click();
  await expect(section.locator("textarea")).toBeVisible();
});
test("AutonomyClass builder deploys a cluster-scoped policy with explicit false and zero", async ({ page }) => {
  // TC-0322
  const mutations = await authoringAPI(page, { mode: "kubernetes" });
  await createResource(page, "#btn-new-autonomyclass");
  await builderField(page, "Name").fill("assisted");
  await expect(builderField(page, "Namespace")).toHaveCount(0);
  await builderField(page, "Level").fill("0");
  await builderField(page, "Requires Approval").selectOption("false");
  await page.locator("#builder-host summary").filter({ hasText: /^Approval Policy$/ }).click();
  await builderField(page, "Tool Invocation").selectOption("always");
  await expect(builderField(page, "Cost Threshold")).toBeDisabled();
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("deployed AutonomyClass assisted");
  expect(mutations).toHaveLength(1);
  expect(mutations[0]).toMatchObject({
    namespace: "",
    body: { kind: "AutonomyClass", metadata: { name: "assisted" }, spec: { level: 0, requiresApproval: false, approvalPolicy: { toolInvocation: "always" } } },
  });
  expect(mutations[0].body.metadata).not.toHaveProperty("namespace");
});

test("MemoryClaim builder supports typed class selection and access-mode lists", async ({ page }) => {
  // TC-0323
  const mutations = await authoringAPI(page, { records: [{ kind: "MemoryClass", metadata: { name: "episodic", namespace: "default" }, spec: { backendType: "in-memory" } }] });
  await createResource(page, "#btn-new-memoryclaim");
  await builderField(page, "Name").fill("conversation");
  await builderField(page, "Memory Class Name").fill("episodic");
  await builderField(page, "Type").selectOption("episodic");
  await builderField(page, "Capacity").fill("100Mi");
  await page.getByRole("button", { name: "Add Access Modes item", exact: true }).click();
  await page.locator('[data-path="spec.accessModes.0"]').selectOption("ReadWrite");
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("deployed MemoryClaim conversation");
  expect(mutations[0].body.spec).toEqual({ memoryClassName: "episodic", type: "episodic", capacity: "100Mi", accessModes: ["ReadWrite"] });
});

test("SecretRef builder authors references without resolving or persisting secret values", async ({ page }) => {
  // TC-0324
  const mutations = await authoringAPI(page);
  await createResource(page, "#btn-new-secretref");
  await builderField(page, "Name").fill("model-credential");
  await builderField(page, "Backend Type").selectOption("env");
  await builderField(page, "Backend Ref").fill("MODEL_API_KEY");
  await builderField(page, "Key").fill("api-key");
  await expect(builderField(page, "Backend Type").locator("option")).toHaveText(["Not set", "internal", "env"]);
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("deployed SecretRef model-credential");
  expect(mutations[0].body.spec).toEqual({ backendType: "env", backendRef: "MODEL_API_KEY", key: "api-key" });
  expect(await page.evaluate(() => ({ local: { ...localStorage }, session: { ...sessionStorage } }))).toEqual({ local: {}, session: {} });
});

test("catalog builders preserve imported unknown fields and empty values when editing", async ({ page }) => {
  // TC-0337
  const original = {
    apiVersion: "kognition.dev/v1alpha1", kind: "Prompt",
    metadata: { name: "instructions", namespace: "default", resourceVersion: 1, annotations: { unchanged: "" } },
    spec: { content: "Original", parameters: [], metadata: { empty: "" }, future: { enabled: false, count: 0, nullable: null, empty: [] } },
  };
  const mutations = await authoringAPI(page, { records: [original] });
  await openResource(page, "prompts", "instructions");
  await builderField(page, "Content").fill("Revised");
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await expect(page.locator("#yaml-area")).toHaveValue(/future/);
  await page.getByRole("tab", { name: "Builder", exact: true }).click();
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("deployed Prompt instructions");
  expect(mutations[0].method).toBe("PUT");
  expect(mutations[0].body.spec).toEqual({ ...original.spec, content: "Revised" });
  expect(mutations[0].body.metadata.annotations).toEqual({ unchanged: "" });
});

test("Studio discovers all authoring families and excludes protected generic creation", async ({ page }) => {
  // TC-0320, TC-0321
  await authoringAPI(page);
  await page.locator("#btn-create").click();
  for (const kind of ["autonomyclass", "memoryclaim", "secretref", "prompt", "model", "modelclass", "modelinvocationprofile",
    "knowledgebaseclass", "memoryclass", "workspace", "cognitivequota", "cognitivelimitrange", "agentpolicy",
    "cognitiveadmissionpolicy", "agentrole", "agentrolebinding", "agentcommunicationpolicy", "trustprofile",
    "solution", "agentjob", "agentservice", "intentgateway", "agentautoscaler", "knowledgebaseindexschedule"]) {
    await expect(page.locator(`#btn-new-${kind}`)).toHaveCount(1);
  }
  for (const kind of ["runtimeapproval", "controlplanerole", "controlplanerolebinding", "modelautoscaler", "memoryrecord"]) {
    await expect(page.locator(`#btn-new-${kind}`)).toHaveCount(0);
  }
  await expect(page.locator("#list-modelinvocationprofiles")).toHaveCount(1);
});

test("generated records are read-only and do not send mutation requests", async ({ page }) => {
  // TC-0335, TC-0336
  const mutations = await authoringAPI(page, { records: [{
    apiVersion: "kognition.dev/v1alpha1", kind: "MemoryRecord", metadata: { name: "record", namespace: "default" },
    spec: { content: "A recorded observation" }, status: { phase: "Ready" },
  }] });
  await openResource(page, "memoryrecords", "record");
  await expect(page.locator("#btn-deploy")).toBeDisabled();
  await expect(page.locator("#btn-delete")).toBeDisabled();
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await expect(page.locator("#yaml-area")).toHaveJSProperty("readOnly", true);
  await expect(page.locator("#btn-apply-yaml")).toBeDisabled();
  expect(mutations).toEqual([]);
});

test("launching execution requires explicit confirmation and then becomes read-only", async ({ page }) => {
  // TC-0334
  const mutations = await authoringAPI(page);
  await createResource(page, "#btn-new-agentinstance");
  await builderField(page, "Name").fill("one-shot");
  await page.locator('[data-path="spec.agentRef.name"]').fill("planner");
  await builderField(page, "Revision").fill("1");
  expect(mutations).toEqual([]);
  page.once("dialog", dialog => dialog.dismiss());
  await page.locator("#btn-deploy").click();
  expect(mutations).toEqual([]);
  page.once("dialog", dialog => dialog.accept());
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("deployed AgentInstance one-shot");
  expect(mutations).toHaveLength(1);
  await expect(page.locator("#btn-deploy")).toBeDisabled();
});

test("creating a referenced resource preserves the calling draft without implicit deployment", async ({ page }) => {
  // TC-0338
  const mutations = await authoringAPI(page);
  await createResource(page, "#btn-new-memoryclaim");
  await builderField(page, "Name").fill("conversation");
  await builderField(page, "Type").selectOption("episodic");
  await page.getByRole("button", { name: "Create referenced Memory Class", exact: true }).click();
  await builderField(page, "Name").fill("local-memory");
  await builderField(page, "Backend Type").fill("in-memory");
  expect(mutations).toEqual([]);
  await page.locator("#btn-deploy").click();
  await expect(page.locator("#status")).toContainText("deployed MemoryClass local-memory");
  await page.getByRole("button", { name: "Return to MemoryClaim draft", exact: true }).click();
  await expect(builderField(page, "Name")).toHaveValue("conversation");
  await expect(builderField(page, "Type")).toHaveValue("episodic");
  await expect(builderField(page, "Memory Class Name")).toHaveValue("local-memory");
  expect(mutations).toHaveLength(1);
});

test("Model profile references can pin the canonical digest without a provider request", async ({ page }) => {
  // TC-0325
  const mutations = await authoringAPI(page, { records: [{
    apiVersion: "kognition.dev/v1alpha1", kind: "ModelInvocationProfile",
    metadata: { name: "portable", namespace: "default" }, spec: { operation: "chat", defaults: { maxCompletionTokens: 128 } },
  }] });
  await createResource(page, "#btn-new-model");
  await builderField(page, "Name").fill("assistant");
  await page.locator("#builder-host summary").filter({ hasText: /^Invocation Profile Ref$/ }).click();
  await page.locator('[data-path="spec.invocationProfileRef.name"]').fill("portable");
  await page.getByRole("button", { name: "Pin selected profile digest", exact: true }).click();
  await expect(page.locator('[data-path="spec.invocationProfileRef.specDigest"]')).toHaveValue(`sha256:${"a".repeat(64)}`);
  expect(mutations).toEqual([]);
});

test("immutable profile revisions preserve the original namespace and do not update the source", async ({ page }) => {
  // TC-0325
  const mutations = await authoringAPI(page, { mode: "kubernetes", namespace: "", records: [{
    apiVersion: "kognition.dev/v1alpha1", kind: "ModelInvocationProfile",
    metadata: { name: "portable", namespace: "team-a", uid: "original-profile" },
    spec: { operation: "chat", defaults: { maxCompletionTokens: 128 } },
  }] });
  await openResource(page, "modelinvocationprofiles", "portable");
  await expect(page.locator("#btn-deploy")).toBeDisabled();
  await page.getByRole("button", { name: "Create new profile revision", exact: true }).click();
  await expect(builderField(page, "Namespace")).toHaveValue("team-a");
  await expect(builderField(page, "Name")).toHaveValue("");
  await expect(page.locator('[data-path="spec.defaults.maxCompletionTokens"]')).toHaveValue("128");
  await expect(page.locator("#btn-deploy")).toBeEnabled();
  expect(mutations).toEqual([]);
});

test("all newly authorable resource families render structured controls within the viewport", async ({ page }) => {
  // TC-0320, TC-0339
  await authoringAPI(page);
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  page.on("dialog", dialog => dialog.accept());
  for (const kind of ["autonomyclass", "memoryclaim", "secretref", "prompt", "model", "modelclass", "modelinvocationprofile",
    "knowledgebaseclass", "memoryclass", "workspace", "cognitivequota", "cognitivelimitrange", "agentpolicy",
    "cognitiveadmissionpolicy", "agentrole", "agentrolebinding", "agentcommunicationpolicy", "trustprofile",
    "solution", "agentjob", "agentservice", "intentgateway", "agentautoscaler", "knowledgebaseindexschedule",
    "agentidentity", "memory", "memoryvolume"]) {
    await createResource(page, `#btn-new-${kind}`);
    await expect(page.locator("#resource-metadata-name")).toBeVisible();
    await expect(page.locator("#builder-host")).not.toContainText("No visual builder");
    const clipped = await page.locator("#builder-host input, #builder-host select, #builder-host textarea, #builder-host button").evaluateAll(controls =>
      controls.filter(control => {
        const rect = control.getBoundingClientRect();
        return rect.width > 0 && rect.height > 0 && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
      }).map(control => control.id || control.textContent));
    expect(clipped, kind).toEqual([]);
  }
  expect(errors).toEqual([]);
});

test("resource authorization failures are visible instead of looking like empty lists", async ({ page }) => {
  // TC-0321
  await authoringAPI(page);
  await page.route("**/api/resources/secretrefs*", route => route.fulfill({ status: 403, json: { error: "forbidden" } }));
  await page.reload();
  await expect(page.locator("#status")).toContainText("Some resource lists failed");
  if (await page.locator("#btn-explorer").isVisible()) await page.locator("#btn-explorer").click();
  const details = page.locator("#list-secretrefs").locator("..");
  if (await details.getAttribute("open") === null) await details.locator("summary").click();
  await expect(page.locator("#list-secretrefs")).toContainText("403");
  await expect(page.locator("#list-secretrefs")).not.toContainText("none");
});

async function createResource(page, selector) {
  await page.locator("#btn-create").click();
  await page.locator(selector).click();
}

test("new resources open Builder instead of inheriting Preview or YAML, while cancelled creation preserves the current view", async ({ page }) => {
  const agent = { apiVersion: "kognition.dev/v1alpha1", kind: "Agent",
    metadata: { name: "support", namespace: "default" }, spec: { modelRef: { name: "model" } } };
  await authoringAPI(page, { records: [agent] });
  await openResource(page, "agents", "support");
  await page.locator('[data-view="preview"]').click();
  await expect(page.locator(".chat-panel")).toBeVisible();
  await createResource(page, "#btn-new-tool");
  await expect(page.locator("#builder-host")).toBeVisible();
  await expect(page.locator("#preview-host")).toBeHidden();
  await expect(builderField(page, "Name")).toHaveValue("");
  await page.locator('[data-view="yaml"]').click();
  page.once("dialog", dialog => dialog.dismiss());
  await createResource(page, "#btn-new-agent");
  await expect(page.locator("#yaml-area")).toBeVisible();
  await expect(page.locator("#editor-kind-pill")).toHaveText("Tool");
  page.once("dialog", dialog => dialog.accept());
  await createResource(page, "#btn-new-agent");
  await expect(page.locator("#builder-host")).toBeVisible();
  await expect(page.locator("#yaml-area")).toBeHidden();
});

for (const [kind, plural, spec] of [
  ["Agent", "agents", { modelRef: { name: "model" } }],
  ["MemoryClaim", "memoryclaims", { memoryClassName: "default", type: "episodic", capacity: "16Mi", accessModes: ["ReadWrite"] }],
  ["Tool", "tools", { protocol: "OpenAPI", endpoint: "https://calculator.example.test", className: "http" }],
  ["ToolGrant", "toolgrants", { agentRef: { name: "support" }, toolRef: { name: "calculator" }, verbs: ["invoke"] }],
]) {
  test(`${kind} metadata has explained key/value controls and persists labels and annotations`, async ({ page }) => {
    const resource = { apiVersion: "kognition.dev/v1alpha1", kind,
      metadata: { name: "metadata-example", namespace: "default" }, spec };
    const calls = await authoringAPI(page, { records: [resource] });
    await openResource(page, plural, resource.metadata.name);
    for (const [title, key, value] of [
      ["Labels", "app.kubernetes.io/part-of", "tutorial"],
      ["Annotations", "tutorial.kognition.dev/description", "Support assistant tutorial"],
    ]) {
      if (kind === "MemoryClaim") {
        const box = page.locator("details.schema-object").filter({ has: page.locator("summary").and(page.getByText(title, { exact: true })) });
        await box.locator("summary").click();
        await expect(box).toContainText(`No ${title.toLowerCase()} configured.`);
        await expect(box.getByText("New entry key", { exact: true })).toBeVisible();
        await box.getByRole("textbox", { name: `${title} entry key`, exact: true }).fill(key);
        await box.getByRole("button", { name: "Add entry", exact: true }).click();
        await page.locator(`[data-path="metadata.${title.toLowerCase()}.${key}"]`).fill(value);
      } else {
        const box = page.locator("#builder-host .field").filter({ has: page.locator("label").and(page.getByText(title, { exact: true })) }).first();
        await expect(box).toContainText(`No ${title.toLowerCase()} configured.`);
        await box.getByRole("button", { name: "+ add entry", exact: true }).click();
        await box.getByRole("textbox", { name: `${title} key 1`, exact: true }).fill(key);
        await box.getByRole("textbox", { name: `${title} value 1`, exact: true }).fill(value);
      }
    }
    await page.locator("#btn-deploy").click();
    await expect.poll(() => calls.length).toBe(1);
    expect(calls[0].body.metadata.labels).toEqual({ "app.kubernetes.io/part-of": "tutorial" });
    expect(calls[0].body.metadata.annotations).toEqual({ "tutorial.kognition.dev/description": "Support assistant tutorial" });
    await page.locator('[data-view="yaml"]').click();
    await expect(page.locator("#yaml-area")).toHaveValue(/app\.kubernetes\.io\/part-of: tutorial/);
    await expect(page.locator("#yaml-area")).toHaveValue(/tutorial\.kognition\.dev\/description: Support assistant tutorial/);
  });
}

test("Tool schema is fetched only after deployment and real introspection errors remain visible", async ({ page }) => {
  let fetches = 0;
  const mutations = await authoringAPI(page);
  await page.route("**/api/tools/*/schema?*", route => {
    fetches++;
    return route.fulfill({ status: 502, json: { error: "calculator endpoint unavailable" } });
  });
  await createResource(page, "#btn-new-tool");
  await builderField(page, "Name").fill("calculator");
  await builderField(page, "Endpoint").fill("https://calculator.example.test");
  const schema = page.locator("#builder-host details.form-section").filter({ has: page.locator("summary").filter({ hasText: /^Schema$/ }) });
  await schema.locator("summary").click();
  await expect(schema).toContainText("Deploy this Tool");
  await expect(schema.getByRole("button", { name: "Introspect", exact: true })).toBeDisabled();
  expect(fetches).toBe(0);
  await page.locator("#btn-deploy").click();
  await expect.poll(() => mutations.length).toBe(1);
  await expect.poll(() => fetches).toBe(1);
  if (await schema.getAttribute("open") === null) await schema.locator("summary").click();
  await expect(schema).toContainText("Schema fetch failed: calculator endpoint unavailable");
});

test("a new OpenAPI Tool can supply its required schema in YAML before deployment", async ({ page }) => {
  const mutations = await authoringAPI(page);
  let fetches = 0;
  const schema = { type: "object", properties: { expression: { type: "string" } } };
  await page.route("**/api/tools/*/schema?*", route => {
    fetches++;
    return route.fulfill({ json: { source: "live", inputSchema: schema } });
  });
  await createResource(page, "#btn-new-tool");
  await builderField(page, "Name").fill("calculator");
  const endpoint = "https://calculator.example.test";
  await builderField(page, "Endpoint").fill(endpoint);
  await builderField(page, "Protocol").selectOption("OpenAPI");
  const edit = page.getByRole("button", { name: "Edit schema in YAML", exact: true });
  await expect(edit).toBeVisible();
  await expect(page.getByRole("button", { name: "Introspect", exact: true })).toBeDisabled();
  await edit.click();
  const yaml = page.locator("#yaml-area");
  await expect(yaml).toHaveValue(/protocol: OpenAPI/);
  await yaml.fill(JSON.stringify({ kind: "Tool", metadata: { name: "calculator", namespace: "default" },
    spec: { protocol: "OpenAPI", endpoint, schema } }));
  await page.getByRole("button", { name: "Apply YAML", exact: true }).click();
  await expect(page.locator("#status")).toHaveText("YAML applied to builder");
  expect(fetches).toBe(0);
  await page.locator("#btn-deploy").click();
  await expect.poll(() => fetches).toBe(1);
  expect(mutations).toHaveLength(1);
  expect(mutations[0]).toMatchObject({ method: "POST", plural: "tools", body: { spec: { protocol: "OpenAPI", schema } } });
});

test("inline KnowledgeBase source data preserves paragraphs in the schema Builder", async ({ page }) => {
  const resource = { apiVersion: "kognition.dev/v1alpha1", kind: "KnowledgeBase",
    metadata: { name: "product-docs", namespace: "default" },
    spec: { knowledgeBaseClassName: "inline", sources: [{ type: "inline", data: "Original passage." }] } };
  const calls = await authoringAPI(page, { records: [resource] });
  await openResource(page, "knowledgebases", resource.metadata.name);
  await page.getByRole("button", { name: "All API fields", exact: true }).click();
  await expandBuilderObjects(page);
  const corpus = "# Orbital Mark 7\n\nBrazil South is not bookable yet.\nTarget: November 2026.";
  await page.locator('[data-path="spec.sources.0.data"]').fill(corpus);
  page.once("dialog", dialog => dialog.accept());
  await page.locator("#btn-deploy").click();
  await expect.poll(() => calls.length).toBe(1);
  expect(calls[0].body.spec.sources[0].data).toBe(corpus);
});

test("long inline Agent instructions are a wrapping textarea in the schema Builder and keep their line breaks", async ({ page }) => {
  const long = "You help fictional Cedar Labs with Orbital Mark 7. " + "Answer briefly and cite the approved passage. ".repeat(12);
  const resource = { apiVersion: "kognition.dev/v1alpha1", kind: "Agent",
    metadata: { name: "support", namespace: "default" },
    spec: { modelRef: { name: "model" }, instructions: { inline: long } } };
  const calls = await authoringAPI(page, { records: [resource] });
  await openResource(page, "agents", resource.metadata.name);
  await page.getByRole("button", { name: "All API fields", exact: true }).click();
  await expandBuilderObjects(page);
  const control = page.locator('[data-path="spec.instructions.inline"]');
  await expect(control).toHaveJSProperty("tagName", "TEXTAREA");
  expect(await control.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  const edited = `${long}\n\nNever reveal customer data.`;
  await control.fill(edited);
  page.once("dialog", dialog => dialog.accept());
  await page.locator("#btn-deploy").click();
  await expect.poll(() => calls.length).toBe(1);
  expect(calls[0].body.spec.instructions.inline).toBe(edited);
});

test("ModelGrant builder creates a complete manifest without clipping", async ({ page }) => {
  let created;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/agents") {
      return route.fulfill({ json: { items: [{ metadata: { name: "planner", namespace: "default" } }] } });
    }
    if (url.pathname === "/api/resources/models") {
      return route.fulfill({ json: { items: [{ metadata: { name: "reasoning-model", namespace: "default" } }] } });
    }
    if (url.pathname === "/api/resources/modelgrants" && request.method() === "POST") {
      created = request.postDataJSON();
      return route.fulfill({ json: created });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-modelgrant");

  const identity = page.locator("#builder-host details").filter({ hasText: "Identity" }).first();
  await identity.locator('input[type="text"]').nth(0).fill("planner-model-access");
  const grant = page.locator("#builder-host details").filter({ hasText: "Grant" }).first();
  await grant.locator("select").nth(0).selectOption("planner");
  await grant.locator("select").nth(1).selectOption("reasoning-model");
  await page.locator("#btn-deploy").click();

  await expect(page.locator("#status")).toContainText("deployed ModelGrant planner-model-access");
  expect(created).toMatchObject({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "ModelGrant",
    metadata: { name: "planner-model-access", namespace: "default" },
    spec: { agentRef: { name: "planner" }, modelRef: { name: "reasoning-model" } },
  });

  const geometry = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
    clippedControls: Array.from(document.querySelectorAll("button, input, select")).filter((element) => {
      const rect = element.getBoundingClientRect();
      return getComputedStyle(element).visibility !== "hidden" && rect.width > 0 && rect.height > 0
        && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
    }).map((element) => element.id || element.textContent.trim()),
  }));
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.clippedControls).toEqual([]);
});

test("MemoryGrant builder creates a complete manifest without clipping", async ({ page }) => {
  let created;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/agents") {
      return route.fulfill({ json: { items: [{ metadata: { name: "planner", namespace: "default" } }] } });
    }
    if (url.pathname === "/api/resources/memoryclaims") {
      return route.fulfill({ json: { items: [{ metadata: { name: "conversation-memory", namespace: "default" } }] } });
    }
    if (url.pathname === "/api/resources/memorygrants" && request.method() === "POST") {
      created = request.postDataJSON();
      return route.fulfill({ json: created });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-memorygrant");

  const identity = page.locator("#builder-host details").filter({ hasText: "Identity" }).first();
  await identity.locator('input[type="text"]').nth(0).fill("planner-memory-access");
  const grant = page.locator("#builder-host details").filter({ hasText: "Grant" }).first();
  await grant.locator("select").nth(0).selectOption("planner");
  await grant.locator("select").nth(1).selectOption("conversation-memory");
  await grant.locator("select").nth(2).selectOption("ReadWrite");
  await page.locator("#btn-deploy").click();

  await expect(page.locator("#status")).toContainText("deployed MemoryGrant planner-memory-access");
  expect(created).toMatchObject({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "MemoryGrant",
    metadata: { name: "planner-memory-access", namespace: "default" },
    spec: {
      agentRef: { name: "planner" },
      memoryClaimRef: { name: "conversation-memory" },
      accessMode: "ReadWrite",
    },
  });

  const geometry = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
    clippedControls: Array.from(document.querySelectorAll("button, input, select")).filter((element) => {
      const rect = element.getBoundingClientRect();
      return getComputedStyle(element).visibility !== "hidden" && rect.width > 0 && rect.height > 0
        && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
    }).map((element) => element.id || element.textContent.trim()),
  }));
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.clippedControls).toEqual([]);
});

test("CrossNamespaceGrant builder preserves repeatable resources without clipping", async ({ page }) => {
  let created;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default", "platform", "team-a"] });
    if (url.pathname === "/api/resources/crossnamespacegrants" && request.method() === "POST") {
      created = request.postDataJSON();
      return route.fulfill({ json: created });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-crossnamespacegrant");

  const identity = page.locator("#builder-host details").filter({ hasText: "Identity" }).first();
  await identity.locator('input[type="text"]').nth(0).fill("share-platform-resources");
  const grant = page.locator("#builder-host details").filter({ hasText: "Grant" }).first();
  await grant.locator(".field").filter({ hasText: "From namespace" }).locator("input").fill("platform");
  await grant.locator(".field").filter({ hasText: "To namespace" }).locator("input").fill("team-a");
  const resourceRows = grant.locator(".kv-row");
  await resourceRows.nth(0).locator("input").nth(0).fill("Model");
  await resourceRows.nth(0).locator("input").nth(1).fill("reasoning-model");
  await grant.getByRole("button", { name: "+ Add resource" }).click();
  await resourceRows.nth(1).locator("input").nth(0).fill("Tool");
  await resourceRows.nth(1).locator("input").nth(1).fill("*");
  await page.locator("#btn-deploy").click();

  await expect(page.locator("#status")).toContainText("deployed CrossNamespaceGrant share-platform-resources");
  expect(created).toMatchObject({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "CrossNamespaceGrant",
    metadata: { name: "share-platform-resources", namespace: "default" },
    spec: {
      from: "platform",
      to: "team-a",
      resources: [
        { kind: "Model", name: "reasoning-model" },
        { kind: "Tool", name: "*" },
      ],
    },
  });

  const geometry = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
    clippedControls: Array.from(document.querySelectorAll("button, input, select")).filter((element) => {
      const rect = element.getBoundingClientRect();
      return getComputedStyle(element).visibility !== "hidden" && rect.width > 0 && rect.height > 0
        && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
    }).map((element) => element.id || element.textContent.trim()),
  }));
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.clippedControls).toEqual([]);
});

test("ToolGrant builder preserves selector constraints limits and conditions", async ({ page }) => {
  let created;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/tools") return route.fulfill({ json: { items: [{ metadata: { name: "web-search", namespace: "default" } }] } });
    if (url.pathname === "/api/resources/toolgrants" && request.method() === "POST") {
      created = request.postDataJSON();
      return route.fulfill({ json: created });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-toolgrant");

  const identity = page.locator("#builder-host details").filter({ hasText: "Identity" }).first();
  await identity.locator('input[type="text"]').nth(0).fill("researcher-search");
  let subject = page.locator("#builder-host details").filter({ hasText: "Subject" }).first();
  await subject.locator(".field").filter({ hasText: "Agent targeting" }).locator("select").selectOption("selector");
  subject = page.locator("#builder-host details").filter({ hasText: "Subject" }).first();
  const selectorField = subject.locator(".field").filter({ hasText: "Agent selector" });
  await selectorField.getByRole("button", { name: "+ add entry" }).click();
  await selectorField.locator(".kv-row input").nth(0).fill("role");
  await selectorField.locator(".kv-row input").nth(1).fill("researcher");

  const access = page.locator("#builder-host details").filter({ hasText: "Tool access" }).first();
  await access.locator(".field").filter({ hasText: "Tool" }).locator("select").selectOption("web-search");
  await access.getByRole("checkbox", { name: "inspect" }).check();

  const constraints = page.locator("#builder-host details").filter({ hasText: "Argument constraints" }).first();
  await constraints.locator("summary").click();
  await constraints.locator("textarea").fill(JSON.stringify({
    type: "object",
    properties: { query: { type: "string", maxLength: 200 } },
  }));
  await constraints.locator("textarea").press("Tab");

  const limits = page.locator("#builder-host details").filter({ hasText: "Rate limits" }).first();
  await limits.locator("summary").click();
  await limits.locator(".field").filter({ hasText: "Calls per minute" }).locator("input").fill("10");
  await limits.locator(".field").filter({ hasText: "Calls per hour" }).locator("input").fill("100");
  await limits.locator(".field").filter({ hasText: "Calls per day" }).locator("input").fill("500");

  const conditions = page.locator("#builder-host details").filter({ hasText: "Conditions" }).first();
  await conditions.locator("summary").click();
  await conditions.locator(".field").filter({ hasText: "Valid from" }).locator("input").fill("2026-01-01T00:00:00Z");
  await conditions.locator(".field").filter({ hasText: "Valid until" }).locator("input").fill("2026-12-31T23:59:59Z");
  const labels = conditions.locator(".field").filter({ hasText: "Required agent labels" });
  await labels.getByRole("button", { name: "+ add entry" }).click();
  await labels.locator(".kv-row input").nth(0).fill("clearance");
  await labels.locator(".kv-row input").nth(1).fill("standard");
  await page.locator("#btn-deploy").click();

  await expect(page.locator("#status")).toContainText("deployed ToolGrant researcher-search");
  expect(created).toEqual({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "ToolGrant",
    metadata: { name: "researcher-search", namespace: "default" },
    spec: {
      agentSelector: { role: "researcher" },
      toolRef: { name: "web-search" },
      verbs: ["invoke", "inspect"],
      argumentConstraints: {
        type: "object",
        properties: { query: { type: "string", maxLength: 200 } },
      },
      rateLimits: { callsPerMinute: 10, callsPerHour: 100, callsPerDay: 500 },
      conditions: {
        validFrom: "2026-01-01T00:00:00Z",
        validUntil: "2026-12-31T23:59:59Z",
        requiredAgentLabels: { clearance: "standard" },
      },
    },
  });

  const geometry = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
    clippedControls: Array.from(document.querySelectorAll("button, input, select, textarea")).filter((element) => {
      const rect = element.getBoundingClientRect();
      return getComputedStyle(element).visibility !== "hidden" && rect.width > 0 && rect.height > 0
        && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
    }).map((element) => element.id || element.textContent.trim()),
  }));
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.clippedControls).toEqual([]);
});

test("ToolGrant builder emits a direct agent reference without a selector", async ({ page }) => {
  let created;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/agents") return route.fulfill({ json: { items: [{ metadata: { name: "researcher-a", namespace: "default" } }] } });
    if (url.pathname === "/api/resources/tools") return route.fulfill({ json: { items: [{ metadata: { name: "web-search", namespace: "default" } }] } });
    if (url.pathname === "/api/resources/toolgrants" && request.method() === "POST") {
      created = request.postDataJSON();
      return route.fulfill({ json: created });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-toolgrant");

  const identity = page.locator("#builder-host details").filter({ hasText: "Identity" }).first();
  await identity.locator('input[type="text"]').nth(0).fill("direct-search");
  const subject = page.locator("#builder-host details").filter({ hasText: "Subject" }).first();
  await subject.locator(".field").filter({ hasText: "Agent" }).last().locator("select").selectOption("researcher-a");
  const access = page.locator("#builder-host details").filter({ hasText: "Tool access" }).first();
  await access.locator(".field").filter({ hasText: "Tool" }).locator("select").selectOption("web-search");
  await access.getByRole("checkbox", { name: "invoke" }).uncheck();
  await access.getByRole("checkbox", { name: "discover" }).check();
  await page.locator("#btn-deploy").click();

  await expect(page.locator("#status")).toContainText("deployed ToolGrant direct-search");
  expect(created).toEqual({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "ToolGrant",
    metadata: { name: "direct-search", namespace: "default" },
    spec: {
      agentRef: { name: "researcher-a" },
      toolRef: { name: "web-search" },
      verbs: ["discover"],
    },
  });
  expect(created.spec).not.toHaveProperty("agentSelector");
});

test("Create picker searches with the keyboard and restores focus", async ({ page }) => {
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await page.locator("#btn-create").click();
  await expect(page.locator("#create-menu")).toBeVisible();
  await expect(page.getByRole("searchbox", { name: "Search resource types" })).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.locator("#btn-new-agent")).toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(page.locator("#btn-new-workflow")).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.locator('#create-menu [role="menuitem"]').last()).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.locator("#create-menu")).toBeHidden();
  await expect(page.locator("#btn-create")).toBeFocused();
  await expect(page.locator("#btn-create")).toHaveAttribute("aria-expanded", "false");
  await page.locator("#btn-create").click();
  await page.keyboard.press("ArrowDown");
  await expect(page.locator("#btn-new-agent")).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.locator("#create-menu")).toBeHidden();
});

test("Create picker keeps the complete catalog searchable and categories consistent", async ({ page }) => {
  const mutations = await authoringAPI(page);
  await page.locator("#btn-create").click();
  const picker = page.getByRole("dialog", { name: "Create a resource" });
  const search = picker.getByRole("searchbox");
  const catalog = await (await page.request.get("/resource-catalog.json")).json();
  const supported = catalog.filter(resource => ["author", "launch"].includes(resource.treatment));
  const items = picker.getByRole("menuitem");
  await expect(items).toHaveCount(supported.length);
  for (const resource of supported) {
    const item = page.locator(`#btn-new-${resource.kind.toLowerCase()}`);
    await expect(item.locator(".create-item-description")).not.toHaveText("");
    await expect(item.locator(".create-item-category")).toContainText(resource.group);
  }
  await picker.getByRole("button", { name: "Common", exact: true }).click();
  await expect(items).toHaveCount(8);
  await expect(page.locator("#btn-new-agent")).toBeVisible();
  await picker.getByRole("button", { name: "Operate", exact: true }).click();
  await expect(items).toHaveCount(supported.filter(resource => resource.group === "Operate").length);
  await expect(page.locator("#btn-new-knowledgebaseclaim")).toBeVisible();
  await picker.getByRole("button", { name: "All", exact: true }).click();
  await search.fill("MEMORY claim");
  await expect(items).toHaveCount(1);
  await expect(page.locator("#btn-new-memoryclaim")).toBeVisible();
  await search.fill("CrossNamespaceGrant");
  await expect(items).toHaveCount(1);
  await expect(page.locator("#btn-new-crossnamespacegrant")).toBeVisible();
  await search.fill("credential");
  await expect(page.locator("#btn-new-secretref")).toBeVisible();
  await search.fill("no-such-resource");
  await expect(items).toHaveCount(0);
  await expect(picker.getByText("No matching resources", { exact: true })).toBeVisible();
  await search.press("ArrowDown");
  await expect(search).toBeFocused();
  await search.fill("AgentJob");
  await search.press("ArrowUp");
  await expect(page.locator("#btn-new-agentjob")).toBeFocused();
  await expect(page.locator("#btn-new-agentjob")).toContainText("Build / Run");
  await page.keyboard.press("Enter");
  await expect(picker).toBeHidden();
  await expect(page.locator("#editor-kind-pill")).toHaveText("AgentJob");
  expect(mutations).toEqual([]);
  await page.locator("#btn-create").click();
  await expect(search).toHaveValue("");
  await expect(items).toHaveCount(supported.length);
  await page.locator("#btn-gallery").focus();
  await expect(picker).toBeHidden();
});

test("Create picker keeps search and filters visible while results scroll within the viewport", async ({ page }) => {
  await authoringAPI(page);
  await page.locator("#btn-create").click();
  const picker = page.locator("#create-menu");
  const before = await picker.boundingBox();
  const viewport = page.viewportSize();
  expect(before.x).toBeGreaterThanOrEqual(0);
  expect(before.y).toBeGreaterThanOrEqual(0);
  expect(before.x + before.width).toBeLessThanOrEqual(viewport.width);
  expect(before.y + before.height).toBeLessThanOrEqual(viewport.height);
  await picker.getByRole("menuitem").last().focus();
  await page.keyboard.press("End");
  await expect(picker.getByRole("searchbox")).toBeInViewport();
  await expect(picker.getByRole("button", { name: "Common", exact: true })).toBeInViewport();
  await expect(picker.getByRole("menuitem").last()).toBeInViewport();
  const width = await picker.evaluate(element => ({ client: element.clientWidth, scroll: element.scrollWidth }));
  expect(width.scroll).toBeLessThanOrEqual(width.client);
});

test("mobile Explorer opens, filters resources, and selects one", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "mobile", "Mobile drawer behavior");
  const agent = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Agent",
    metadata: { name: "planner", namespace: "default" },
    spec: {},
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/agents") return route.fulfill({ json: { items: [agent] } });
    if (url.pathname === "/api/resources/agents/planner") return route.fulfill({ json: agent });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await page.locator("#btn-explorer").click();
  await expect(page.locator("#resource-explorer")).toHaveClass(/open/);
  await expect(page.locator("#resource-filter")).toBeFocused();
  await page.locator("#resource-filter").fill("planner");
  await page.getByText("planner", { exact: true }).click();
  await expect(page.locator("#resource-explorer")).not.toHaveClass(/open/);
  await expect(page.locator("#editor-title")).toContainText("planner");
});

test("workflow node layout persists locally without dirtying the manifest", async ({ page }) => {
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "layout-workflow", namespace: "default" },
    spec: {
      steps: [
        { name: "plan", type: "agent", agentRef: { name: "planner" } },
        { name: "execute", type: "agent", agentRef: { name: "executor" } },
      ],
      edges: [{ from: "plan", to: "execute" }],
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/layout-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("layout-workflow", { exact: true }).click();
  const plan = page.locator('.node[data-step="plan"]');
  await plan.scrollIntoViewIfNeeded();
  const initialLeft = Number.parseFloat(await plan.evaluate((node) => node.style.left));
  const box = await plan.boundingBox();
  await page.mouse.move(box.x + 30, box.y + 25);
  await page.mouse.down();
  await page.mouse.move(box.x + 100, box.y + 65, { steps: 4 });
  await page.mouse.up();
  const movedLeft = Number.parseFloat(await plan.evaluate((node) => node.style.left));

  expect(movedLeft).toBeGreaterThan(initialLeft + 40);
  await expect(page.locator("#btn-discard")).toBeDisabled();
  await expect(page.locator("#editor-title")).not.toContainText("*");

  await page.reload();
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("layout-workflow", { exact: true }).click();
  await expect(page.locator('.node[data-step="plan"]')).toHaveCSS("left", `${movedLeft}px`);
});

test("workflow commands undo and redo semantic changes with dirty state", async ({ page }) => {
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "history-workflow", namespace: "default" },
    spec: {
      steps: [
        { name: "plan", type: "agent", agentRef: { name: "planner" } },
        { name: "execute", type: "agent", agentRef: { name: "executor" } },
      ],
      edges: [{ from: "plan", to: "execute" }],
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/history-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("history-workflow", { exact: true }).click();
  const toolbar = page.locator(".canvas-toolbar");
  await toolbar.getByRole("button", { name: "+ Agent", exact: true }).click();

  await expect(page.locator("#wf-canvas .node")).toHaveCount(3);
  await expect(page.locator("#btn-discard")).toBeEnabled();
  await expect(page.locator("#editor-title")).toContainText("*");

  if (page.viewportSize().width <= 700) await page.locator("#inspector-close").click();
  await toolbar.getByRole("button", { name: "+ Agent", exact: true }).click();
  await expect(page.locator("#wf-canvas .node")).toHaveCount(4);
  await expect.poll(async () => page.locator('.node[data-step="step-2"]').evaluate((node) => {
    const nodeBounds = node.getBoundingClientRect();
    const canvasBounds = node.closest("#wf-canvas").getBoundingClientRect();
    const centerX = nodeBounds.left + nodeBounds.width / 2;
    const centerY = nodeBounds.top + nodeBounds.height / 2;
    return centerX >= canvasBounds.left && centerX <= canvasBounds.right
      && centerY >= canvasBounds.top && centerY <= canvasBounds.bottom;
  })).toBe(true);

  await toolbar.getByRole("button", { name: "Undo graph change" }).click();
  await expect(page.locator("#wf-canvas .node")).toHaveCount(3);
  await toolbar.getByRole("button", { name: "Undo graph change" }).click();
  await expect(page.locator("#wf-canvas .node")).toHaveCount(2);
  await expect(page.locator("#btn-discard")).toBeDisabled();
  await expect(page.locator("#editor-title")).not.toContainText("*");

  await page.keyboard.press("Control+Shift+Z");
  await expect(page.locator("#wf-canvas .node")).toHaveCount(3);
  await expect(page.locator("#btn-discard")).toBeEnabled();
  await expect(page.locator("#editor-title")).toContainText("*");
});

test("workflow minimap navigates large graphs without dirtying the manifest", async ({ page }) => {
  const steps = Array.from({ length: 9 }, (_, index) => ({
    name: `step-${index + 1}`,
    type: index === 8 ? "tool" : "agent",
    ...(index === 8 ? { toolRef: { name: "publisher" } } : { agentRef: { name: `agent-${index + 1}` } }),
  }));
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "large-workflow", namespace: "default" },
    spec: {
      entryPoint: "step-1",
      steps,
      edges: steps.slice(1).map((step, index) => ({ from: steps[index].name, to: step.name })),
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/large-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("large-workflow", { exact: true }).click();

  const canvas = page.locator("#wf-canvas");
  const minimap = page.locator("#workflow-minimap");
  const viewport = minimap.locator(".workflow-minimap-viewport");
  await expect(minimap).toBeVisible();
  await expect(minimap.locator(".workflow-minimap-node")).toHaveCount(9);
  await expect(minimap.locator(".workflow-minimap-edges line")).toHaveCount(8);

  const initialScrollLeft = await canvas.evaluate((element) => element.scrollLeft);
  const initialViewportX = Number(await viewport.getAttribute("x"));
  const minimapBounds = await minimap.boundingBox();
  await minimap.click({ position: { x: minimapBounds.width * 0.78, y: minimapBounds.height * 0.5 } });
  await expect.poll(async () => canvas.evaluate((element) => element.scrollLeft)).toBeGreaterThan(initialScrollLeft + 20);
  await expect.poll(async () => Number(await viewport.getAttribute("x"))).toBeGreaterThan(initialViewportX + 20);

  const pointerScrollLeft = await canvas.evaluate((element) => element.scrollLeft);
  await minimap.focus();
  await page.keyboard.press("ArrowLeft");
  await expect.poll(async () => canvas.evaluate((element) => element.scrollLeft)).toBeLessThan(pointerScrollLeft);
  await expect(page.locator("#btn-discard")).toBeDisabled();
  await expect(page.locator("#editor-title")).not.toContainText("*");
});

test("workflow problems and outline navigate the graph without dirtying it", async ({ page }) => {
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "graph-assistant-workflow", namespace: "default" },
    spec: {
      entryPoint: "plan",
      steps: [
        { name: "plan", type: "agent", agentRef: { name: "planner" } },
        { name: "execute", type: "agent" },
        { name: "orphan", type: "tool", toolRef: { name: "reporter" } },
      ],
      edges: [
        { from: "plan", to: "execute" },
        { from: "plan", to: "execute" },
        { from: "ghost", to: "execute" },
      ],
      approvalGates: [{ from: "plan", to: "orphan" }],
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/graph-assistant-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("graph-assistant-workflow", { exact: true }).click();

  await page.getByRole("tab", { name: "Problems (5)" }).click();
  const unreachable = page.getByRole("button", { name: /orphan is unreachable/ });
  await unreachable.click();
  await expect(page.locator('.node[data-step="orphan"]')).toHaveClass(/selected/);
  await expect(page.locator('.node[data-step="orphan"]')).toBeFocused();
  if (page.viewportSize().width <= 700) await page.locator("#inspector-close").click();

  await page.getByRole("tab", { name: "Outline" }).click();
  await page.getByRole("button", { name: "plan → execute · edge" }).first().click();
  await expect(page.locator('svg.edges path[data-from="plan"][data-to="execute"]').first()).toHaveClass(/selected/);
  await expect(page.locator('svg.edges path[data-from="plan"][data-to="execute"]').first()).toBeFocused();
  await expect(page.locator("#btn-discard")).toBeDisabled();
  await expect(page.locator("#editor-title")).not.toContainText("*");
});

test("workflow problems explain cyclic budget and self-loop requirements", async ({ page }) => {
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "cyclic-workflow", namespace: "default" },
    spec: {
      entryPoint: "plan",
      steps: [
        { name: "plan", type: "agent", agentRef: { name: "planner" } },
        { name: "review", type: "agent", agentRef: { name: "reviewer" } },
        { name: "archive", type: "tool", toolRef: { name: "archiver" } },
      ],
      edges: [
        { from: "plan", to: "review" },
        { from: "review", to: "plan" },
        { from: "archive", to: "archive" },
      ],
      budgets: { maxIterations: 0 },
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/cyclic-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("cyclic-workflow", { exact: true }).click();
  await page.getByRole("tab", { name: /Problems/ }).click();

  await expect(page.getByRole("button", { name: /archive to archive cannot connect a step to itself/i })).toBeVisible();
  await expect(page.getByRole("button", { name: /cycle.*MaxIterations must be greater than 0/i })).toBeVisible();
  await expect(page.locator("#btn-discard")).toBeDisabled();
});

test("workflow inspector edits are coalesced and edge endpoints are constrained", async ({ page }) => {
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "inspector-workflow", namespace: "default" },
    spec: {
      steps: [
        { name: "plan", type: "agent", agentRef: { name: "planner" } },
        { name: "review", type: "agent", agentRef: { name: "reviewer" } },
        { name: "execute", type: "agent", agentRef: { name: "executor" } },
      ],
      edges: [{ from: "plan", to: "execute" }],
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/inspector-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("inspector-workflow", { exact: true }).click();

  await page.locator('.node[data-step="plan"]').click();
  const condition = page.getByLabel("Condition (CEL)");
  await condition.type("approved");
  await expect(condition).toHaveValue("approved");
  await page.locator("#inspector-close").click();
  await page.locator(".canvas-toolbar").getByRole("button", { name: "Undo graph change" }).click();
  await page.locator('.node[data-step="plan"]').click();
  await expect(page.getByLabel("Condition (CEL)")).toHaveValue("");

  await page.locator("#inspector-close").click();
  const edge = page.getByRole("button", { name: "Edge from plan to execute" });
  await edge.focus();
  await edge.press("Enter");
  const from = page.getByLabel("From*", { exact: true });
  await expect(from).toHaveValue("plan");
  await expect(from.locator("option")).toHaveCount(3);
  await from.selectOption("review");
  await expect(page.locator('svg.edges path[data-from="review"][data-to="execute"]')).toHaveCount(1);
  await page.locator("#inspector-close").click();
  await page.locator(".canvas-toolbar").getByRole("button", { name: "Undo graph change" }).click();
  await expect(page.locator('svg.edges path[data-from="plan"][data-to="execute"]')).toHaveCount(1);
});

test("workflow approval gate edits are undoable and text input is coalesced", async ({ page }) => {
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "approval-history-workflow", namespace: "default" },
    spec: {
      steps: [
        { name: "plan", type: "agent", agentRef: { name: "planner" } },
        { name: "review", type: "agent", agentRef: { name: "reviewer" } },
        { name: "execute", type: "agent", agentRef: { name: "executor" } },
      ],
      edges: [{ from: "plan", to: "execute" }],
      approvalGates: [{ from: "plan", to: "execute", requiredApprovers: [] }],
    },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows") return route.fulfill({ json: { items: [workflow] } });
    if (url.pathname === "/api/resources/workflows/approval-history-workflow") return route.fulfill({ json: workflow });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  const openGates = async () => {
    await page.locator("summary").filter({ hasText: "Approval Gates" }).click();
  };
  const undo = async () => {
    await page.locator(".canvas-toolbar").getByRole("button", { name: "Undo graph change" }).click();
  };

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (page.viewportSize().width <= 700) await page.locator("#btn-explorer").click();
  await page.getByText("approval-history-workflow", { exact: true }).click();
  await openGates();

  await page.getByLabel("From step").selectOption("review");
  await expect(page.getByLabel("From step")).toHaveValue("review");
  await undo();
  await openGates();
  await expect(page.getByLabel("From step")).toHaveValue("plan");

  await page.getByLabel("Timeout").type("15m");
  await expect(page.getByLabel("Timeout")).toHaveValue("15m");
  await undo();
  await openGates();
  await expect(page.getByLabel("Timeout")).toHaveValue("");

  await page.getByRole("button", { name: "+ add gate" }).click();
  await expect(page.getByRole("button", { name: "Remove gate" })).toHaveCount(2);
  await undo();
  await openGates();
  await expect(page.getByRole("button", { name: "Remove gate" })).toHaveCount(1);

  await page.getByRole("button", { name: "Remove gate" }).click();
  await expect(page.getByRole("button", { name: "Remove gate" })).toHaveCount(0);
  await undo();
  await openGates();
  await expect(page.getByRole("button", { name: "Remove gate" })).toHaveCount(1);
});

test("namespace change clears a dirty draft only after confirmation", async ({ page }) => {
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "kubernetes", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default", "team-a"] });
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-agent");
  const name = page.locator("#builder-host details").filter({ hasText: "Identity" }).first().locator('input[type="text"]').first();
  await name.fill("draft-agent");
  page.once("dialog", (dialog) => dialog.dismiss());
  await page.locator("#namespace-select").selectOption("team-a");
  await expect(page.locator("#namespace-select")).toHaveValue("default");
  await expect(name).toHaveValue("draft-agent");

  page.once("dialog", (dialog) => dialog.accept());
  await page.locator("#namespace-select").selectOption("team-a");
  await expect(page.locator("#namespace-select")).toHaveValue("team-a");
  await expect(page.locator("#empty-state")).toBeVisible();
  await expect(page.locator("#btn-deploy")).toBeDisabled();
});

test("namespace change ignores a resource selection still loading", async ({ page }) => {
  const oldPrompt = { apiVersion: "kognition.dev/v1alpha1", kind: "Prompt",
    metadata: { name: "old-prompt", namespace: "default" }, spec: { content: "Old workspace" } };
  await authoringAPI(page, { mode: "kubernetes", records: [oldPrompt] });
  let releaseResponse;
  const pendingResponse = new Promise(resolve => { releaseResponse = resolve; });
  await page.route("**/api/resources/prompts/old-prompt?*", async route => {
    await pendingResponse;
    return route.fulfill({ json: oldPrompt });
  });
  const loaded = page.waitForResponse(response => response.url().includes("/api/resources/prompts/old-prompt?"));
  try {
    await openResource(page, "prompts", "old-prompt");
    await expect(page.locator("#status")).toContainText("loading");
    await page.locator("#namespace-select").selectOption("team-a");
  } finally {
    releaseResponse();
  }
  await (await loaded).finished();
  await expect(page.locator("#empty-state")).toBeVisible();
  await expect(page.locator("#btn-deploy")).toBeDisabled();
});

for (const view of ["builder", "yaml"]) test(`deploy preserves ${view} edits made before the server replies`, async ({ page }) => {
  await authoringAPI(page);
  let releaseResponse;
  const pendingResponse = new Promise(resolve => { releaseResponse = resolve; });
  const updates = [];
  await page.route("**/api/resources/prompts**", async route => {
    const request = route.request();
    if (!["POST", "PUT"].includes(request.method())) return route.fallback();
    const body = request.postDataJSON();
    updates.push({ method: request.method(), body });
    if (request.method() === "POST") await pendingResponse;
    return route.fulfill({ json: { ...body, metadata: { ...body.metadata, uid: "prompt-uid", resourceVersion: 2 } } });
  });
  await createResource(page, "#btn-new-prompt");
  await builderField(page, "Name").fill("pending-prompt");
  await builderField(page, "Content").fill("First revision");
  const deploy = page.locator("#btn-deploy");
  const yamlArea = page.locator("#yaml-area");
  const pendingYAML = "kind: Prompt\nmetadata: {name: pending-prompt}\nspec: {content: Second revision}\n";
  try {
    await deploy.click();
    await expect(deploy).toBeDisabled();
    if (view === "yaml") {
      await page.locator('[data-view="yaml"]').click();
      await yamlArea.fill(pendingYAML);
    } else {
      await builderField(page, "Content").fill("Second revision");
    }
  } finally {
    releaseResponse();
  }
  await expect(page.locator("#status")).toContainText("newer edits not deployed");
  if (view === "yaml") {
    await expect(yamlArea).toHaveValue(pendingYAML);
    expect(updates.map(({ method, body }) => [method, body.spec.content])).toEqual([["POST", "First revision"]]);
    return;
  }
  await expect(builderField(page, "Content")).toHaveValue("Second revision");
  await deploy.click();
  await expect(page.locator("#status")).toHaveText("deployed Prompt pending-prompt");
  expect(updates.map(({ method, body }) => [method, body.spec.content, body.metadata.resourceVersion])).toEqual([
    ["POST", "First revision", undefined], ["PUT", "Second revision", 2],
  ]);
});

test("Gallery import cannot silently replace a dirty draft", async ({ page }) => {
  let importCalls = 0;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/gallery") return route.fulfill({ json: { items: [{ id: "starter", title: "Starter agent", description: "A starter", resources: [] }] } });
    if (url.pathname === "/api/gallery/import") {
      importCalls++;
      return route.fulfill({ json: { results: [] } });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await createResource(page, "#btn-new-agent");
  const name = page.locator("#builder-host details").filter({ hasText: "Identity" }).first().locator('input[type="text"]').first();
  await name.fill("draft-agent");
  await page.locator("#btn-gallery").click();
  page.once("dialog", (dialog) => dialog.dismiss());
  await page.getByRole("button", { name: /Add to default/ }).click();
  expect(importCalls).toBe(0);
  await expect(page.locator("#gallery-modal")).toBeVisible();
  await page.locator("#gallery-modal .modal-header .close").click();
  await expect(name).toHaveValue("draft-agent");
});

test("loading a deployed Tool stays clean until schema introspection is requested", async ({ page }, testInfo) => {
  const tool = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Tool",
    metadata: { name: "calculator", namespace: "default" },
    spec: { endpoint: "http://calculator.default.svc/mcp" },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/tools") return route.fulfill({ json: { items: [tool] } });
    if (url.pathname === "/api/resources/tools/calculator") return route.fulfill({ json: tool });
    if (url.pathname === "/api/tools/calculator/schema") {
      return route.fulfill({ json: { source: "live", inputSchema: { type: "object", properties: { expression: { type: "string" } } } } });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  if (testInfo.project.name === "mobile") await page.locator("#btn-explorer").click();
  await page.getByText("calculator", { exact: true }).click();
  await expect(page.locator("#editor-title")).toContainText("calculator");
  await expect(page.locator("#btn-discard")).toBeDisabled();
  await expect(page.locator("#editor-title")).not.toContainText("*");

  await page.locator("#builder-host details").filter({ hasText: /^Schema/ }).first().locator(":scope > summary").click();
  await page.getByRole("button", { name: /Introspect|Refresh/ }).click();
  await expect(page.locator("#btn-discard")).toBeEnabled();
  await expect(page.locator("#editor-title")).toContainText("*");
});

test("Workflow approval prompt remains stable while waiting for a decision", async ({ page }) => {
  let workflowRunGets = 0;
  let approvalDecision = null;
  let approved = false;
  const workflow = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "write-blog-post-workflow", namespace: "default" },
    spec: {
      steps: [
        { name: "write", agentRef: { name: "writer" } },
        { name: "publish", agentRef: { name: "publisher" } },
      ],
      edges: [{ from: "write", to: "publish" }],
      approvalGates: [{ from: "write", to: "publish" }],
    },
  };
  const waitingRun = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "WorkflowRun",
    metadata: { name: "write-blog-post-run", namespace: "default" },
    spec: { workflowRef: { name: workflow.metadata.name } },
    status: {
      phase: "Running",
      activeStep: "publish",
      snapshotSpec: workflow.spec,
      stepStatuses: [{ name: "write", phase: "Succeeded", output: "draft" }],
      conditions: [{ type: "Ready", status: "False", reason: "WaitingApproval" }],
    },
  };

  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflows" && request.method() === "GET") {
      return route.fulfill({ json: { items: [workflow] } });
    }
    if (url.pathname === "/api/resources/workflows/write-blog-post-workflow") {
      return route.fulfill({ json: workflow });
    }
    if (url.pathname === "/api/invoke/workflow/write-blog-post-workflow") {
      return route.fulfill({ json: waitingRun });
    }
    if (url.pathname === "/api/resources/workflowruns/write-blog-post-run") {
      workflowRunGets++;
      if (!approved) return route.fulfill({ json: waitingRun });
      return route.fulfill({ json: {
        ...waitingRun,
        status: {
          ...waitingRun.status,
          phase: "Succeeded",
          activeStep: "",
          stepStatuses: [
            ...waitingRun.status.stepStatuses,
            { name: "publish", phase: "Succeeded", output: "published" },
          ],
          approvalRecords: [{ step: "publish", decision: "approve", approver: "studio-user" }],
        },
      } });
    }
    if (url.pathname === "/api/resources/approvaldecisions" && request.method() === "POST") {
      approvalDecision = request.postDataJSON();
      approved = true;
      return route.fulfill({ json: approvalDecision });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await page.evaluate(() => selectResource("workflows", "write-blog-post-workflow", "default"));
  await page.locator("#tab-preview").click();
  await page.getByRole("button", { name: "Run workflow" }).click();

  const prompt = page.locator(".run-approval-gate");
  const reason = prompt.locator('input[type="text"]');
  await expect(prompt).toBeVisible();
  await reason.fill("reviewing before publish");
  await reason.focus();
  await prompt.evaluate((element) => { element.dataset.stabilityMarker = "original"; });
  const pollCountAtDecision = workflowRunGets;

  await expect.poll(() => workflowRunGets).toBeGreaterThanOrEqual(pollCountAtDecision + 2);
  await expect(prompt).toHaveAttribute("data-stability-marker", "original");
  await expect(reason).toHaveValue("reviewing before publish");
  await expect(reason).toBeFocused();

  await prompt.getByRole("button", { name: "Approve" }).click();
  await expect.poll(() => approvalDecision).not.toBeNull();
  expect(approvalDecision).toMatchObject({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "ApprovalDecision",
    metadata: { namespace: "default" },
    spec: {
      workflowRunRef: { name: "write-blog-post-run" },
      step: "publish",
      decision: "approve",
      reason: "reviewing before publish",
    },
  });
  expect(approvalDecision.spec).not.toHaveProperty("approver");
  await expect(prompt).toHaveCount(0);
  await expect(page.locator(".run-phase").filter({ hasText: "Succeeded" }).first()).toBeVisible();
});

test("selected WorkflowRun approval prompt remains stable while polling", async ({ page }) => {
  let workflowRunGets = 0;
  const waitingRun = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "WorkflowRun",
    metadata: { name: "write-blog-post-run", namespace: "default" },
    spec: { workflowRef: { name: "write-blog-post-workflow" } },
    status: {
      phase: "Pending",
      activeStep: "publish",
      snapshotSpec: {
        steps: [
          { name: "write", agentRef: { name: "writer" } },
          { name: "publish", agentRef: { name: "publisher" } },
        ],
        approvalGates: [{ from: "write", to: "publish" }],
      },
      stepStatuses: [{ name: "write", phase: "Succeeded", output: "draft" }],
      conditions: [{ type: "Ready", status: "False", reason: "WaitingApproval" }],
    },
  };

  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { mode: "standalone", namespace: "default" } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/workflowruns/write-blog-post-run") {
      workflowRunGets++;
      return route.fulfill({ json: waitingRun });
    }
    if (url.pathname.startsWith("/api/resources/")) return route.fulfill({ json: { items: [] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#status")).toHaveText("ready");
  await page.evaluate(() => selectResource("workflowruns", "write-blog-post-run", "default"));

  const prompt = page.locator(".run-approval-gate");
  const reason = prompt.locator('input[type="text"]');
  await expect(prompt).toBeVisible();
  await reason.fill("viewer review in progress");
  await reason.focus();
  await prompt.evaluate((element) => { element.dataset.stabilityMarker = "original"; });
  const pollCountAtDecision = workflowRunGets;

  await expect.poll(() => workflowRunGets).toBeGreaterThanOrEqual(pollCountAtDecision + 2);
  await expect(prompt).toHaveAttribute("data-stability-marker", "original");
  await expect(reason).toHaveValue("viewer review in progress");
  await expect(reason).toBeFocused();
});