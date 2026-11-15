// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { test, expect } = require("@playwright/test");
const fs = require("node:fs");
const path = require("node:path");
const Ajv2020 = require("ajv/dist/2020");
const addFormats = require("ajv-formats");

const longID = "invocation-" + "0123456789abcdef".repeat(7);
const unsafeText = "<script>window.__unsafe = true</script> & denied";
const decisions = [
  {
    schemaVersion: 1,
    id: "decision-allowed",
    workspace: "default",
    runId: "demo-run",
    timestamp: "2026-08-27T10:00:00Z",
    plane: "tool",
    outcome: "allowed",
    reasonCode: "authorized",
    identity: { agentIdentity: "agent/runner", agentIdentityRef: { kind: "AgentIdentity", name: "runner", namespace: "default" } },
    session: { sessionId: "session-shared", capabilityId: "capability-tool-search", instanceRef: { kind: "AgentInstance", name: "instance-a", namespace: "default" } },
    request: { invocationId: longID, operation: "execute", targetRef: { kind: "Tool", name: "search", namespace: "default" } },
    authorization: { identity: "allowed", role: "allowed", grant: "allowed", policy: "allowed" },
    approval: { status: "not_required", decisionRef: { kind: "ApprovalDecision", name: "search-approval", namespace: "default" } },
    execution: { status: "completed", targetReached: true },
    trace: { traceId: "0123456789abcdef0123456789abcdef", spanId: "0123456789abcdef" },
  },
  {
    schemaVersion: 1,
    id: "decision-denied",
    workspace: "default",
    runId: "demo-run",
    timestamp: "2026-08-27T10:01:00Z",
    plane: "tool",
    outcome: "denied",
    reasonCode: "grant_missing",
    reason: unsafeText,
    identity: { agentIdentity: "agent/runner" },
    session: { sessionId: "session-shared" },
    request: { invocationId: longID, operation: unsafeText, targetRef: { kind: "Tool", name: "restricted-" + "x".repeat(90) } },
    authorization: { identity: "allowed", role: "allowed", grant: "denied", policy: "not_evaluated" },
    execution: { status: "blocked", targetReached: false },
  },
  {
    schemaVersion: 1,
    id: "decision-revoked",
    workspace: "default",
    runId: "another-run",
    timestamp: "2026-08-27T10:02:00Z",
    plane: "memory",
    outcome: "revoked",
    reasonCode: "grant_revoked",
    identity: { agentIdentity: "agent/auditor" },
    session: { sessionId: "session-revoked" },
    request: { invocationId: "invocation-revoked", operation: "recall", targetRef: { kind: "MemoryVolume", name: "project-memory" } },
    authorization: { identity: "allowed", role: "allowed", grant: "revoked", policy: "not_evaluated" },
    execution: { status: "blocked", targetReached: false },
  },
];

test("browser Trust Decision fixtures conform to the production schema", () => {
  const schemaPath = path.resolve(__dirname, "../../../../api/schemas/trust-decision-v1.schema.json");
  const schema = JSON.parse(fs.readFileSync(schemaPath, "utf8"));
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  addFormats(ajv);
  const validate = ajv.compile(schema);

  for (const decision of decisions) {
    expect(validate(decision), `${decision.id}: ${ajv.errorsText(validate.errors)}`).toBeTruthy();
  }
});

test("generic resources are keyboard operable and filters recover after refresh", async ({ page }, testInfo) => {
  const agents = ["alpha-agent", "beta-agent", "gamma-agent"].map((name) => ({
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Agent",
    metadata: { name, namespace: "default", creationTimestamp: "2026-09-08T10:00:00Z" },
    status: { phase: "Ready", replicas: 1 },
  }));

  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [{ title: "Compute", items: [{ label: "Agents", plural: "agents", singular: "agent" }] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/agents") return route.fulfill({ json: { items: agents } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  if (testInfo.project.name === "mobile") await page.locator("#catalog-toggle").click();
  await page.getByRole("button", { name: "Agents" }).click();

  const rows = page.locator("table.list tbody tr");
  await expect(rows).toHaveCount(3);
  await rows.first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(rows.nth(1)).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("#detail")).toBeVisible();
  await expect(page.locator("#detail-close")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(rows.nth(1)).toBeFocused();

  await page.locator("#filter").fill("alpha");
  await expect(rows).toHaveCount(1);
  await page.locator("#refresh").click();
  await expect(rows).toHaveCount(1);
  await page.locator("#filter").fill("");
  await expect(rows).toHaveCount(3);
});

test("mobile resource lists use labeled cards and contain long detail titles", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "mobile", "Mobile resource presentation");
  const longName = `agent-${"identity-segment-".repeat(8)}`;
  const agent = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Agent",
    metadata: { name: longName, namespace: "default", creationTimestamp: "2026-09-08T10:00:00Z" },
    status: { phase: "Ready", replicas: 1 },
  };
  const evaluation = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "Evaluation",
    metadata: { name: "release-quality", namespace: "default", creationTimestamp: "2026-09-08T10:00:00Z" },
    status: { phase: "Succeeded", score: 0.93, passed: true, metrics: [{ name: "quality", passed: true }] },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [{ title: "Workloads", items: [
        { label: "Agents", plural: "agents", singular: "agent" },
        { label: "Evaluations", plural: "evaluations", singular: "evaluation" },
      ] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/agents") return route.fulfill({ json: { items: [agent] } });
    if (url.pathname === "/api/resources/evaluations") return route.fulfill({ json: { items: [evaluation] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await page.locator("#catalog-toggle").click();
  await page.getByRole("button", { name: "Agents" }).click();
  const agentRow = page.locator("table.list tbody tr");
  await expect(agentRow.locator("td").nth(1)).toHaveAttribute("data-label", "Phase");
  await expect(agentRow).toHaveCSS("display", "grid");
  await agentRow.click();
  const detailGeometry = await page.locator("#detail").evaluate((detail) => {
    const header = detail.querySelector(".detail-header").getBoundingClientRect();
    const title = detail.querySelector("#detail-title").getBoundingClientRect();
    return { headerRight: header.right, titleRight: title.right, scrollWidth: detail.scrollWidth, clientWidth: detail.clientWidth };
  });
  expect(detailGeometry.titleRight).toBeLessThanOrEqual(detailGeometry.headerRight);
  expect(detailGeometry.scrollWidth).toBeLessThanOrEqual(detailGeometry.clientWidth);
  await page.locator("#detail-close").click();

  await page.locator("#catalog-toggle").click();
  await page.getByRole("button", { name: "Evaluations" }).click();
  const evaluationRow = page.locator("table.evaluation-list tbody tr");
  await expect(evaluationRow.locator("td").nth(2)).toHaveAttribute("data-label", "Score");
  await expect(evaluationRow).toHaveCSS("display", "grid");
  expect(await page.locator("#table-wrap").evaluate((element) => element.scrollWidth <= element.clientWidth)).toBeTruthy();
});

test("Approval Decisions expose decision context and audit reason", async ({ page }, testInfo) => {
  const approval = {
    apiVersion: "kognition.dev/v1alpha1",
    kind: "ApprovalDecision",
    metadata: { name: "publish-approval", namespace: "default", creationTimestamp: "2026-09-08T10:00:00Z" },
    spec: {
      workflowRunRef: { name: "customer-advisory-run" },
      step: "publish",
      iteration: 2,
      decision: "approve",
      approver: "operator@example.test",
      reason: "Customer advisory reviewed for publication.",
    },
    status: { observed: true, observedAt: "2026-09-08T10:01:00Z" },
  };
  const denial = {
    ...approval,
    metadata: { ...approval.metadata, name: "publish-denial" },
    spec: { ...approval.spec, decision: "deny", reason: "Publication needs another review." },
  };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [{ title: "Governance", items: [
        { label: "Approval Decisions", plural: "approvaldecisions", singular: "approvaldecision" },
      ] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/resources/approvaldecisions") return route.fulfill({ json: { items: [approval, denial] } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  if (testInfo.project.name === "mobile") await page.locator("#catalog-toggle").click();
  await page.getByRole("button", { name: "Approval Decisions" }).click();
  const table = page.locator("table.approval-decision-list");
  await expect(table.locator("thead")).toContainText("Decision");
  const approvedRow = table.locator("tbody tr").filter({ hasText: "publish-approval" });
  await expect(approvedRow).toContainText("Approved");
  await expect(approvedRow).toContainText("customer-advisory-run");
  await expect(approvedRow).toContainText("publish · iteration 2");
  await expect(approvedRow).toContainText("operator@example.test");
  await expect(approvedRow).toContainText("Customer advisory reviewed for publication.");
  await expect(approvedRow.locator("td").nth(2)).toHaveAttribute("data-label", "Workflow / step");
  const deniedRow = table.locator("tbody tr").filter({ hasText: "publish-denial" });
  await expect(deniedRow).toContainText("Denied");
  await expect(deniedRow.locator(".decision-outcome")).toHaveClass(/blocked/);
});

test("mobile catalog behaves as a dismissible navigation overlay", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "mobile", "Mobile navigation behavior");
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [{ title: "Compute", items: [{ label: "Agents", plural: "agents", singular: "agent" }] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    return route.fulfill({ json: { items: [] } });
  });

  await page.goto("/");
  const toggleBox = await page.locator("#catalog-toggle").boundingBox();
  expect(toggleBox.width).toBeGreaterThanOrEqual(44);
  expect(toggleBox.height).toBeGreaterThanOrEqual(44);
  await page.locator("#catalog-toggle").click();
  await expect(page.locator("#sidebar")).toBeVisible();
  const agentButton = page.getByRole("button", { name: "Agents" });
  await expect(agentButton).toBeFocused();
  const agentBox = await agentButton.boundingBox();
  expect(agentBox.height).toBeGreaterThanOrEqual(44);
  await expect(page.locator(".content")).toHaveAttribute("inert", "");
  await page.locator("#catalog-backdrop").click({ position: { x: 360, y: 400 } });
  await expect(page.locator("#sidebar")).toBeHidden();
  await expect(page.locator("#catalog-toggle")).toBeFocused();
  await expect(page.locator(".content")).not.toHaveAttribute("inert", "");
});

test("resource detail is modal and backdrop dismissal restores its row", async ({ page }, testInfo) => {
  const resource = { apiVersion: "kognition.dev/v1alpha1", kind: "Agent", metadata: { name: "alpha-agent", namespace: "default" } };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [{ title: "Compute", items: [{ label: "Agents", plural: "agents", singular: "agent" }] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    return route.fulfill({ json: { items: [resource] } });
  });

  await page.goto("/");
  if (testInfo.project.name === "mobile") await page.locator("#catalog-toggle").click();
  await page.getByRole("button", { name: "Agents" }).click();
  const row = page.locator("table.list tbody tr");
  await row.click();
  await expect(page.locator("#detail")).toHaveAttribute("role", "dialog");
  await expect(page.locator("#detail")).toHaveAttribute("aria-modal", "true");
  await expect(page.locator("main")).toHaveAttribute("inert", "");
  if (testInfo.project.name === "mobile") await page.locator("#detail-close").click();
  else await page.locator("#detail-backdrop").click({ position: { x: 10, y: 400 } });
  await expect(page.locator("#detail")).toBeHidden();
  await expect(row).toBeFocused();
  await expect(page.locator("main")).not.toHaveAttribute("inert", "");
});

test("Trust Decision explorer is filterable, safe, keyboard accessible, and unclipped", async ({ page }, testInfo) => {
  const queryRequests = [];
  let failNext = false;
  let trustRequestCount = 0;
  let releaseFirstTrustResponse;
  const firstTrustResponse = new Promise((resolve) => { releaseFirstTrustResponse = resolve; });

  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", observability: { jaegerUrl: "https://observe.example.test/jaeger", grafanaUrl: "https://grafana.example.test/d/tool-gateway/tool-gateway-operations" }, groups: [{ title: "Governance", items: [{ label: "Trust Decisions", view: "trust-decisions" }] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/trust-decisions") {
      trustRequestCount += 1;
      queryRequests.push(url);
      if (trustRequestCount === 1) await firstTrustResponse;
      if (failNext) {
        failNext = false;
        return route.fulfill({ status: 503, json: { error: unsafeText } });
      }
      return route.fulfill({ json: { items: decisions, truncated: url.searchParams.get("outcome") === "denied" } });
    }
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  if (testInfo.project.name === "mobile") {
    await expect(page.locator("#sidebar")).toBeHidden();
    await page.locator("#catalog-toggle").click();
    await expect(page.locator("#sidebar")).toBeVisible();
  }
  await page.getByRole("button", { name: "Trust Decisions" }).click();
  await expect(page.getByText("Loading trust decisions…")).toBeVisible();
  releaseFirstTrustResponse();
  await expect(page.locator(".trust-decisions-list tbody tr")).toHaveCount(3);
  await expect(page.locator("#resource-filter")).toBeHidden();

  await page.locator("#trust-agent").fill("agent/runner");
  await page.locator("#trust-session").fill("session-shared");
  await page.locator("#trust-plane").selectOption("tool");
  await page.locator("#trust-outcome").selectOption("denied");
  await page.locator("#trust-limit").fill("25");
  await page.getByRole("button", { name: "Apply filters" }).click();
  await expect(page.locator("#trust-overflow")).toBeVisible();
  const filtered = queryRequests.at(-1).searchParams;
  expect(Object.fromEntries(filtered)).toMatchObject({ namespace: "default", agentIdentity: "agent/runner", sessionId: "session-shared", plane: "tool", outcome: "denied", limit: "25" });

  const rows = page.locator(".trust-decisions-list tbody tr");
  await rows.first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(rows.nth(1)).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("#detail")).toBeVisible();
  await expect(page.locator("#detail-close")).toBeFocused();
  await expect(page.getByText(unsafeText, { exact: true }).first()).toBeVisible();
  expect(await page.locator("#detail script").count()).toBe(0);
  expect(await page.locator("#detail").innerText()).not.toMatch(/password|token|secret/i);
  await page.keyboard.press("Escape");
  await expect(rows.nth(1)).toBeFocused();

  failNext = true;
  await page.locator("#refresh").click();
  await expect(page.getByText("Trust decisions could not be loaded.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  expect(await page.locator("#status").innerHTML()).not.toContain("<script>");
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.locator(".trust-decisions-list tbody tr")).toHaveCount(3);

  const geometry = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
    controls: Array.from(document.querySelectorAll("button, input, select")).filter((element) => {
      const rect = element.getBoundingClientRect();
      return rect.width > 0 && rect.height > 0 && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
    }).map((element) => element.id || element.textContent.trim()),
  }));
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.controls).toEqual([]);
  await page.screenshot({ path: testInfo.outputPath(`trust-decisions-${testInfo.project.name}.png`), fullPage: true });
});

test("Trust Focus returns to the Control Plane dashboard", async ({ page }) => {
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/meta") return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [] } });
    if (path === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (path === "/api/trust-decisions") return route.fulfill({ json: { items: [], truncated: false } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/?view=trust&runId=demo-run&focus=1");
  await expect(page.locator("#trust-focus")).toBeVisible();
  await expect(page.locator("#trust-run-proof")).toBeHidden();
  await expect(page.locator("#trust-focus-connection")).toHaveText("Live");
  await page.locator("#trust-open-control-plane").click();

  await expect(page).toHaveURL(/\/?runId=demo-run$/);
  await expect(page.locator("body")).not.toHaveClass(/trust-focus-mode/);
  await expect(page.locator("#trust-focus")).toBeHidden();
  await expect(page.locator("#content-header")).toBeVisible();
  await expect(page.locator("#trust-open-focus")).toBeVisible();
  await page.locator("#trust-open-focus").click();
  await expect(page).toHaveURL(/\/?view=trust&runId=demo-run&focus=1$/);
  await expect(page.locator("#trust-focus")).toBeVisible();
});

test("Trust Focus namespace selector filters evidence and survives navigation", async ({ page }) => {
  const trustRequests = [];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { namespace: "default", mode: "kubernetes", auditNamespace: "kognition-system", groups: [] } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default", "development"] });
    if (url.pathname === "/api/trust-decisions") {
      trustRequests.push(url);
      return route.fulfill({ json: { items: url.searchParams.get("namespace") === "" ? decisions : [], truncated: false } });
    }
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/?view=trust&namespace=development&runId=demo-run&focus=1");
  await expect(page.locator("#mode-badge")).toHaveText("mode: kubernetes · audit: kognition-system");
  await expect(page.locator("#namespace")).toBeVisible();
  await expect(page.locator("#namespace")).toHaveValue("development");
  expect(trustRequests[0].searchParams.get("namespace")).toBe("development");
  await expect(page.locator("#trust-focus-filter-panel > summary")).toBeVisible();
  await page.locator("#trust-focus-filter-panel > summary").click();
  await expect(page.locator("#trust-filters")).toBeVisible();
  await page.locator("#trust-agent").fill("agent/runner");
  await page.locator("#trust-session").fill("session-shared");
  await page.locator("#trust-plane").selectOption("tool");
  await page.locator("#trust-outcome").selectOption("denied");
  await page.locator("#trust-limit").fill("25");
  await page.locator("#trust-filters button[type=submit]").click();
  await expect.poll(() => trustRequests.length).toBeGreaterThan(1);
  expect(Object.fromEntries(trustRequests.at(-1).searchParams)).toMatchObject({
    namespace: "development",
    runId: "demo-run",
    agentIdentity: "agent/runner",
    sessionId: "session-shared",
    plane: "tool",
    outcome: "denied",
    limit: "25",
  });
  await page.locator("#namespace").selectOption("default");
  await expect.poll(() => trustRequests.length).toBeGreaterThan(2);
  expect(trustRequests.at(-1).searchParams.get("namespace")).toBe("default");
  await page.locator("#namespace").selectOption("");
  await expect.poll(() => trustRequests.length).toBeGreaterThan(3);
  expect(trustRequests.at(-1).searchParams.get("namespace")).toBe("");
  const currentRoute = new URL(page.url()).searchParams;
  expect(currentRoute.has("namespace")).toBe(true);
  expect(currentRoute.get("namespace")).toBe("");
  await expect(page.locator("#trust-focus-stream .trust-stream-event")).toHaveCount(3);
  await page.locator("#trust-open-control-plane").click();
  await expect(page).toHaveURL(/namespace=/);
  await expect(page.locator("#namespace")).toHaveValue("");
  await page.locator("#trust-open-focus").click();
  await expect(page).toHaveURL(/namespace=/);
  await expect(page.locator("#namespace")).toBeVisible();
  await expect(page.locator("#namespace")).toHaveValue("");
});

test("direct Kubernetes Trust Focus defaults to all namespaces", async ({ page }) => {
  const trustRequests = [];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { namespace: "default", mode: "kubernetes", groups: [] } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default", "development"] });
    if (url.pathname === "/api/trust-decisions") {
      trustRequests.push(url);
      return route.fulfill({ json: { items: decisions, truncated: false } });
    }
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/?view=trust&focus=1");

  await expect(page.locator("#namespace")).toHaveValue("");
  await expect.poll(() => new URL(page.url()).searchParams.get("namespace")).toBe("");
  await expect(page.locator("#trust-focus-stream .trust-stream-event")).toHaveCount(3);
  await expect(page.locator('#trust-plane-strip [data-plane="model"] small')).toHaveText("No decisions in current view");
  await expect(page.locator('#trust-plane-strip [data-plane="communication"] small')).toHaveText("No decisions in current view");
  expect(trustRequests[0].searchParams.get("namespace")).toBe("");
});

test("Control Plane exposes an all-runs Trust Focus link", async ({ page }) => {
  await page.route("**/api/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/meta") return route.fulfill({ json: { namespace: "default", mode: "standalone", groups: [] } });
    if (path === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (path === "/api/trust-decisions") return route.fulfill({ json: { items: decisions, truncated: false } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/");
  await expect(page.locator("#trust-open-focus")).toBeVisible();
  await page.locator("#trust-open-focus").click();

  await expect(page).toHaveURL(/\/?view=trust&focus=1$/);
  await expect(page.locator("#trust-focus")).toBeVisible();
  await expect(page.locator("#trust-focus-run")).toHaveText("All runs");
  await expect(page.locator("#trust-focus-stream .trust-stream-event")).toHaveCount(3);
});

test("Trust Focus opens directly and preserves selected evidence across auto-refresh", async ({ page }, testInfo) => {
  const queryRequests = [];
  let trustRequestCount = 0;
  const added = {
    ...decisions[0],
    id: "decision-added",
    timestamp: "2026-08-27T10:02:00Z",
    plane: "model",
    outcome: "completed",
    reasonCode: "target_completed",
  };

  await page.clock.install();
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", observability: { jaegerUrl: "https://observe.example.test/jaeger", grafanaUrl: "https://grafana.example.test/d/tool-gateway/tool-gateway-operations" }, groups: [{ title: "Governance", items: [{ label: "Trust Decisions", view: "trust-decisions" }] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/run-proof") {
      expect(url.searchParams.get("runId")).toBe("demo-run");
      return route.fulfill({ json: {
        schemaVersion: 1,
        runId: "demo-run",
        scenario: "Delegated research",
        protectedAction: "Publish result",
        mode: "standalone-fallback",
        passed: true,
        assertions: { total: 4, passed: 4, failed: 0 },
        reportRef: "demo-report.json",
      } });
    }
    if (url.pathname === "/api/trust-decisions") {
      trustRequestCount += 1;
      queryRequests.push(url);
      if (trustRequestCount === 4) return route.fulfill({ status: 503, json: { error: "temporarily unavailable" } });
      const items = trustRequestCount === 1
        ? [decisions[1], decisions[0]]
        : trustRequestCount === 2
          ? [added, decisions[1], decisions[0]]
          : [added, decisions[1]];
      return route.fulfill({ json: { items, truncated: false } });
    }
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/?view=trust&runId=demo-run&focus=1");
  await expect(page.locator("#trust-focus")).toBeVisible();
  await expect(page.locator("#trust-open-control-plane")).toHaveAttribute("href", "/?runId=demo-run");
  await expect(page.locator("#trust-plane-strip [data-plane]")).toHaveCount(4);
  await expect(page.locator("#trust-outcome-summary")).toContainText("Evidence events2");
  await expect(page.locator("#trust-run-proof")).toBeVisible();
  await expect(page.locator("#trust-run-proof")).toContainText("Delegated research");
  await expect(page.locator("#trust-run-proof")).toContainText("Publish result");
  await expect(page.locator("#trust-run-proof")).toContainText("standalone-fallback");
  await expect(page.locator("#trust-run-proof-status")).toHaveText("Passed");
  await expect(page.locator("#trust-run-proof-assertions")).toHaveText("4/4 passed");
  await expect(page.locator("#trust-run-proof-reference")).toHaveText("demo-report.json");
  await expect(page.locator("#trust-focus-stream [data-decision-id]")).toHaveCount(2);
  await expect(page.locator("#table-wrap")).toBeEmpty();
  await expect(page.locator("#trust-focus-evidence")).toContainText("decision-denied");
  await expect(page.locator("#trust-open-trace")).toBeHidden();
  await expect(page.locator("#trust-open-metrics")).toHaveAttribute("href", "https://grafana.example.test/d/tool-gateway/tool-gateway-operations");
  expect(queryRequests[0].searchParams.get("runId")).toBe("demo-run");
  await expect(page.locator("#sidebar")).toBeHidden();
  await expect(page.locator("#trust-focus-filter-panel > summary")).toBeVisible();
  await expect(page.locator("#detail-edit")).toBeHidden();

  const selected = page.locator('#trust-focus-stream [data-decision-id="decision-allowed"]');
  await selected.click();
  await expect(page.locator("#trust-focus-evidence")).toContainText("decision-allowed");
  await expect(page.locator("#trust-open-trace")).toHaveAttribute("href", "https://observe.example.test/jaeger/trace/0123456789abcdef0123456789abcdef");
  const regionIDs = ["trust-plane-strip", "trust-outcome-summary", "trust-focus-stream", "trust-focus-evidence"];
  const before = await page.evaluate((ids) => Object.fromEntries(ids.map((id) => {
    const rect = document.getElementById(id).getBoundingClientRect();
    return [id, { width: rect.width, height: rect.height }];
  })), regionIDs);
  await page.locator("#auto-refresh").check();
  await page.clock.fastForward(5000);
  await expect(page.locator("#trust-focus-stream [data-decision-id]")).toHaveCount(3);
  await expect(page.locator("#trust-focus-evidence")).toContainText("decision-allowed");
  await page.locator('#trust-focus-stream [data-decision-id="decision-added"]').click();
  await expect(page.locator("#trust-focus-evidence .trust-reason").first()).toHaveText("Reason: target_completed");
  await selected.click();
  const after = await page.evaluate((ids) => Object.fromEntries(ids.map((id) => {
    const rect = document.getElementById(id).getBoundingClientRect();
    return [id, { width: rect.width, height: rect.height }];
  })), regionIDs);
  expect(after).toEqual(before);

  await page.clock.fastForward(5000);
  await expect(page.locator("#trust-evidence-state")).toHaveText("Stale · absent from latest refresh");
  await expect(page.locator("#trust-focus-evidence").getByText("Reason: authorized", { exact: true })).toBeVisible();
  await page.clock.fastForward(5000);
  await expect(page.locator("#trust-focus-connection")).toHaveText("Disconnected");
  await expect(page.locator("#trust-evidence-state")).toHaveText("Last known evidence · disconnected");
  await expect(page.locator("#trust-focus-evidence").getByText("Reason: authorized", { exact: true })).toBeVisible();

  const geometry = await page.evaluate(() => ({
    viewportWidth: document.documentElement.clientWidth,
    viewportHeight: document.documentElement.clientHeight,
    scrollWidth: document.documentElement.scrollWidth,
    regions: ["trust-plane-strip", "trust-outcome-summary", "trust-focus-stream", "trust-focus-evidence"].map((id) => {
      const rect = document.getElementById(id).getBoundingClientRect();
      return { id, left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom };
    }),
    footerTop: document.querySelector(".footer").getBoundingClientRect().top,
    clippedControls: Array.from(document.querySelectorAll("button, input, select")).filter((element) => {
      const rect = element.getBoundingClientRect();
      return rect.width > 0 && rect.height > 0 && (rect.left < 0 || rect.right > document.documentElement.clientWidth);
    }).map((element) => element.id || element.textContent.trim()),
  }));
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.viewportWidth);
  expect(geometry.regions.every((region) => region.left >= 0 && region.right <= geometry.viewportWidth)).toBeTruthy();
  expect(geometry.clippedControls).toEqual([]);
  for (const [index, region] of geometry.regions.entries()) {
    for (const other of geometry.regions.slice(index + 1)) {
      const overlapWidth = Math.min(region.right, other.right) - Math.max(region.left, other.left);
      const overlapHeight = Math.min(region.bottom, other.bottom) - Math.max(region.top, other.top);
      expect(overlapWidth > 0 && overlapHeight > 0, `${region.id} overlaps ${other.id}`).toBeFalsy();
    }
  }
  expect(geometry.regions.at(-1).bottom).toBeLessThanOrEqual(geometry.footerTop);
  if (testInfo.project.name !== "mobile") {
    expect(geometry.regions.every((region) => region.top >= 0 && region.bottom <= geometry.viewportHeight)).toBeTruthy();
  }
  await page.screenshot({ path: testInfo.outputPath(`trust-focus-${testInfo.project.name}.png`), fullPage: true });
});

test("Trust Focus distinguishes in-process authority from missing evidence", async ({ page }) => {
  const direct = {
    ...decisions[0],
    plane: "model",
    identity: undefined,
    agentRef: { kind: "Agent", namespace: "default", name: "target", uid: "target-uid", resourceVersion: "3" },
    session: { sessionId: "communication-write", authorityMode: "in_process" },
    request: { operation: "complete", targetRef: { kind: "Model", namespace: "default", name: "target-model" } },
    authorization: { identity: "not_required", role: "not_required", grant: "allowed", policy: "allowed" },
  };
  const schema = JSON.parse(fs.readFileSync(path.resolve(__dirname, "../../../../api/schemas/trust-decision-v1.schema.json"), "utf8"));
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  addFormats(ajv);
  expect(ajv.compile(schema)(JSON.parse(JSON.stringify(direct)))).toBeTruthy();
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { namespace: "default", mode: "standalone", observability: {}, groups: [] } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/trust-decisions") return route.fulfill({ json: { items: [direct], truncated: false } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });
  await page.goto("/?view=trust&runId=demo-run&focus=1");
  const evidence = page.locator("#trust-focus-evidence-body");
  await expect(evidence).toContainText("Agent attribution");
  await expect(evidence).toContainText("Agent/target");
  await expect(evidence).toContainText("Not applicable (in-process Model call)");
  await expect(evidence).not.toContainText("Evidence unavailable");
  await page.screenshot({ path: test.info().outputPath("in-process-evidence.png"), fullPage: true });
  delete direct.agentRef;
  await page.reload();
  await expect(evidence).toContainText("Evidence unavailable");
  await expect(evidence).not.toContainText("Not applicable (in-process Model call)");
});

test("Trust Focus preserves distinct approval evidence states", async ({ page }) => {
  const decision = { ...decisions[0], outcome: "denied", reasonCode: "grant_missing", execution: { status: "blocked", targetReached: false } };
  const schema = JSON.parse(fs.readFileSync(path.resolve(__dirname, "../../../../api/schemas/trust-decision-v1.schema.json"), "utf8"));
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  addFormats(ajv);
  const validate = ajv.compile(schema);
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") return route.fulfill({ json: { namespace: "default", mode: "standalone", observability: {}, groups: [] } });
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/trust-decisions") return route.fulfill({ json: { items: [decision], truncated: false } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });
  for (const status of ["not_evaluated", "not_required", "pending", "approved", "rejected", "unavailable", undefined]) {
    decision.approval = status ? { status } : undefined;
    expect(validate(JSON.parse(JSON.stringify(decision)))).toBeTruthy();
    await page.goto("/?view=trust&runId=demo-run&focus=1");
    const approval = page.locator("#trust-focus-evidence-body dt").filter({ hasText: /^Approval evidence$/ }).locator("xpath=following-sibling::dd[1]");
    await expect(approval).toHaveText(status === "not_evaluated" ? "Not evaluated" : status || "Evidence unavailable");
  }
});

test("Trust Focus omits observability links when URLs are not configured", async ({ page }) => {
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "standalone", observability: {}, groups: [{ title: "Governance", items: [{ label: "Trust Decisions", view: "trust-decisions" }] }] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/trust-decisions") return route.fulfill({ json: { items: [decisions[0]], truncated: false } });
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/?view=trust&runId=demo-run&focus=1");
  await expect(page.locator("#trust-focus-evidence")).toContainText("decision-allowed");
  await expect(page.locator("#trust-open-trace")).toBeHidden();
  await expect(page.locator("#trust-open-metrics")).toBeHidden();
  await expect(page.locator("body")).not.toContainText(/token|secret/i);
});

test("Trust Focus loads directly when the resource catalog is unavailable", async ({ page }) => {
  const trustRequests = [];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/meta") {
      return route.fulfill({ json: { namespace: "default", mode: "kubernetes", observability: {}, groups: [] } });
    }
    if (url.pathname === "/api/namespaces") return route.fulfill({ json: ["default"] });
    if (url.pathname === "/api/trust-decisions") {
      trustRequests.push(url);
      return route.fulfill({ json: { items: [decisions[0]], truncated: false } });
    }
    return route.fulfill({ status: 404, json: { error: "not found" } });
  });

  await page.goto("/?view=trust&runId=demo-run&focus=1");
  await expect(page.locator("#trust-focus")).toBeVisible();
  await expect(page.locator("#trust-focus-connection")).toHaveText("Live");
  await expect(page.locator("#trust-focus-evidence")).toContainText("decision-allowed");
  expect(trustRequests).toHaveLength(1);
  expect(Object.fromEntries(trustRequests[0].searchParams)).toMatchObject({ namespace: "", runId: "demo-run" });
});