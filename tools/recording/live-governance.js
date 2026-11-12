// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const assert = require("node:assert/strict");
const { expect } = require("@playwright/test");
const { RANCHER_TARGET } = require("./live-authoring");
const { PLURALS, verifyIntent, parseContinuationOptions, readContinuation, runProduction } = require("./live-production-session");

const CONTENT = "GOVERNANCE DEMO ONLY: approved test bulletin.";
const POLICY = Object.freeze({ batch: "governance-denials",
  kinds: ["Tool", "ToolGrant", "Workflow", "Agent", "ModelGrant", "AgentRole", "AgentRoleBinding", "TrustProfile"] });

function fixtures(runId) {
  if (!/^rancher-gov-[a-z0-9-]+$/.test(runId) || runId.length > 32) {
    throw new Error("Governance takes require a fresh bounded rancher-gov-* run ID");
  }
  const prefix = `full-${runId}`;
  const principal = `${prefix}-actor`;
  const resource = (kind, suffix, spec) => ({
    apiVersion: "kognition.dev/v1alpha1", kind,
    metadata: { name: `${prefix}-${suffix}`, namespace: "tutorial-support",
      labels: { "kognition.dev/recording-run": runId } }, spec,
  });
  const tool = resource("Tool", "receiver", {
    className: "story-http", protocol: "OpenAPI",
    endpoint: "http://story-integrations.tutorial-support.svc.cluster.local:8081/publish",
    description: "Isolated governed publication test alias; no external customer communication.",
    metadata: { allowPrivateNetwork: "true" },
    inputSchema: { type: "object", properties: {
      runID: { type: "string" }, content: { type: "string", maxLength: 4096 },
    }, required: ["runID", "content"] },
    sandbox: { timeout: "20s" },
  });
  const grant = resource("ToolGrant", "grant", {
    agentRef: { name: principal }, toolRef: { name: tool.metadata.name }, verbs: ["invoke"],
    argumentConstraints: { type: "object", properties: {
      runID: { type: "string" }, content: { const: CONTENT },
    }, required: ["runID", "content"], additionalProperties: false },
    rateLimits: { callsPerMinute: 10 },
  });
  const flow = resource("Workflow", "flow", {
    steps: [{ name: "publish", type: "tool", agentRef: { name: principal },
      toolRef: { name: tool.metadata.name }, timeout: "20s", retry: { maxAttempts: 1 },
      input: '{"runID":{{ printf "%q" .Params.publicationID }},"content":{{ printf "%q" .Params.content }}}' }],
    maxConcurrentRuns: 1, historyLimit: 10,
  });
  const gate = (suffix, quorum, timeout) => resource("Workflow", suffix, {
    steps: [
      { name: "prepare", type: "tool", agentRef: { name: principal },
        toolRef: { name: "story-calculator" }, input: '{"expression":"17 * 23"}', timeout: "20s" },
      { name: "publish", type: "tool", agentRef: { name: principal },
        toolRef: { name: "story-publish" }, timeout: "20s", retry: { maxAttempts: 1 },
        input: '{"runID":{{ printf "%q" .Params.publicationID }},"content":{{ printf "%q" .Params.content }}}' },
    ],
    edges: [{ from: "prepare", to: "publish" }], maxConcurrentRuns: 1, historyLimit: 10,
    approvalGates: [{ from: "prepare", to: "publish", requiredApprovers: quorum, timeout }],
  });
  const actor = resource("Agent", "actor", {
    modelRef: { name: "tutorial-support-model" },
    instructions: { inline: "This zero-replica Agent is only the authorization principal for direct Tool-only tutorial fixtures. Do not invoke it." },
    runtime: { class: "default", timeout: "30s", maxTokens: 128 }, replicas: 0,
  });
  const modelGrant = resource("ModelGrant", "model", {
    agentRef: { name: principal }, modelRef: { name: "tutorial-support-model" },
  });
  const role = resource("AgentRole", "role");
  delete role.spec;
  role.rules = [{ resources: ["Tool"], verbs: ["get", "invoke", "discover", "inspect"] }];
  const binding = resource("AgentRoleBinding", "role", {
    roleRef: { name: role.metadata.name }, subjects: [{ kind: "Agent", name: principal }],
  });
  const calculatorGrant = resource("ToolGrant", "calculator", {
    agentRef: { name: principal }, toolRef: { name: "story-calculator" }, verbs: ["invoke"],
  });
  const publicationGrant = resource("ToolGrant", "publication", {
    agentRef: { name: principal }, toolRef: { name: "story-publish" }, verbs: ["invoke"],
    argumentConstraints: grant.spec.argumentConstraints,
  });
  return { tool, grant, flow, actor, modelGrant, role, binding, calculatorGrant, publicationGrant,
    rejection: gate("reject", 1, "10m"), quorum: gate("quorum", 2, "60s") };
}

function verifyGrantOutcome(run, events, publication, expected, publicationID) {
  assert.equal(publication.runID, publicationID);
  assert.equal(run.status.phase, expected === "allowed" ? "Ready" : "Failed");
  const step = run.status.stepStatuses.find(item => item.name === "publish");
  assert(step);
  assert.equal(step.attempts, 1, "Governance probes must not retry effects");
  assert.equal(publication.publicationCount, expected === "allowed" ? 1 : 0);
  if (expected === "allowed") {
    assert.equal(publication.content, CONTENT);
    const actual = JSON.parse(step.output);
    assert.equal(actual.runID, publicationID);
    assert.equal(actual.publicationCount, 1);
  } else {
    const failures = events.filter(event => event.reason === "ToolStepFailed" &&
      event.involvedObject?.uid === run.metadata.uid);
    const pattern = expected === "arguments" ? /argument constraints denied invocation/i : /no ToolGrant permits/i;
    assert(failures.some(event => pattern.test(event.message)), "Actual UID-scoped events must prove this authorization denial");
    assert(!step.output, "A denial cannot masquerade as a successful output");
  }
}

async function runGovernance(input) {
  const options = parseContinuationOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  if (options.target !== RANCHER_TARGET) throw new Error("Governance recording requires the exact Rancher target");
  const desired = fixtures(options.runId);
  const previous = readContinuation(options, POLICY.batch, "governance-");
  return runProduction(options, POLICY, async session => {
    const receipt = async id => JSON.parse(await session.kube(["exec", "deployment/story-integrations",
      "-n", session.namespace, "-c", "publication", "--", "python", "-c",
      "import urllib.request,sys; print(urllib.request.urlopen(sys.argv[1], timeout=10).read().decode())",
      `http://127.0.0.1:8081/publications/${id}`]));
    session.report.coverage = {
      entireScenesCompleted: [], scenes: ["S10", "S23", "S24", "S35"],
      qualifications: [
        "Direct Tool steps use a fresh exact zero-replica authorization principal and do not invoke its model.",
        "All publications are isolated fictional test records in the owned receiver.",
        "One authenticated scripted vote is not two-person approval; this take proves insufficient quorum and timeout, not successful distinct-person quorum.",
        "Tool, Memory and Communication RuntimeApproval planes and trust-score transitions require separate footage.",
      ],
    };
    session.report.probes = [];
    let flow, grant, principal;
    if (previous) {
      const prior = previous.report;
      assert.equal(prior.probes?.length, 1, "Only the settled first denial may be resumed");
      assert.equal(prior.probes[0].label, "ungranted");
      for (const created of prior.created.filter(item => item.kind !== "WorkflowRun")) {
        const live = await session.get(PLURALS[created.kind], created.name);
        assert.equal(live.metadata.uid, created.uid);
        const archived = prior.evidence.find(item => item.type === "created" && item.resource.metadata.uid === created.uid)?.resource;
        assert(archived, "The original exact creation evidence is required");
        verifyIntent(live, archived);
      }
      principal = await session.get("agents", desired.actor.metadata.name);
      assert.equal(principal.spec.replicas, 0);
      flow = await session.get("workflows", desired.flow.metadata.name);
      for (const object of [desired.grant, desired.calculatorGrant, desired.publicationGrant, desired.rejection, desired.quorum]) {
        assert.equal(await session.get(PLURALS[object.kind], object.metadata.name, session.namespace, true), null,
          "A later governance action already exists; do not replay it");
      }
      const oldRun = await session.get("workflowruns", prior.probes[0].run.metadata.name);
      assert.equal(oldRun.metadata.uid, prior.probes[0].run.metadata.uid);
      const oldReceipt = await receipt(`${options.runId}-ungranted`);
      verifyGrantOutcome(oldRun, prior.probes[0].events, oldReceipt, "ungranted", oldReceipt.runID);
      session.report.continuation = { reportPath: previous.reportPath, reportSHA256: previous.reportSHA256,
        sourceVideoSHA256: previous.sourceVideoSHA256, actionsReplayed: false,
        priorSetupAndDeniedRunPreserved: true };
      session.report.probes.push({ ...prior.probes[0], recoveredWithoutReplay: true });
      await session.scene("G00", ["S10", "S35"], "Continue from the preserved setup and settled denied run.",
        "The earlier take saved the exact resources and proved the first denial without a target effect, then its read-only viewer failed. We reverify those identities and the denied result. No action or request is replayed.", async () => {
          await session.inspectObserved("workflowruns", oldRun.metadata.name, ["Failed"], "recovered-ungranted-result");
        });
    } else {
    await session.scene("G01", ["S10"], "Create an isolated Tool alias and a one-attempt governed workflow.",
      "A Tool definition identifies an endpoint. It does not authorize an Agent to use it. This isolated alias points to the tutorial receiver, and every probe uses a fresh publication identity.", async () => {
        await session.save(desired.modelGrant);
        principal = await session.save(desired.actor);
        await session.save(desired.role);
        await session.save(desired.binding);
        await session.save(session.resource("TrustProfile", `full-${options.runId}-trust`, {
          agentRef: { name: principal.metadata.name }, agentIncarnationUID: principal.metadata.uid,
          initialScore: 0.9, initialClass: "story-demonstration", maxClass: "story-demonstration",
        }));
        await session.wait("zero-replica principal definition readiness", async () =>
          (await session.get("agents", principal.metadata.name)).status?.phase === "Ready");
        await session.save(desired.tool);
        await session.wait("governance Tool readiness", async () =>
          (await session.get("tools", desired.tool.metadata.name)).status?.phase === "Ready");
        flow = await session.save(desired.flow);
      });
    }
    const probe = async (label, content, expected) => {
      const publicationID = `${options.runId}-${label}`;
      assert.equal((await receipt(publicationID)).publicationCount, 0, "Never reuse a publication identity");
      const created = await session.startWorkflow(flow, "Exercise one exact governance boundary.", { publicationID, content });
      const terminal = await session.wait("governance run terminal state", async () => {
        const run = await session.get("workflowruns", created.metadata.name);
        return ["Ready", "Failed"].includes(run.status?.phase) ? run : false;
      });
      const events = JSON.parse(await session.kube(["get", "events", "-n", session.namespace,
        "--field-selector", `involvedObject.uid=${terminal.metadata.uid}`, "-o", "json"])).items;
      const publication = await receipt(publicationID);
      verifyGrantOutcome(terminal, events, publication, expected, publicationID);
      session.report.probes.push({ label, expected, run: terminal, events, publication });
      session.persist();
      await session.inspectObserved("workflowruns", terminal.metadata.name, [terminal.status.phase], `governance-${label}`);
    };
    if (!previous) await session.scene("G02", ["S10", "S35"], "Observe denial without a matching ToolGrant.",
      "The first run fails at authorization. Independent events identify the missing grant, and the receiver has no publication for this run. Creating the Tool did not create permission.", async () => {
        await probe("ungranted", CONTENT, "ungranted");
      });
    await session.scene("G03", ["S10", "S35"], "Grant only this Agent, Tool and exact approved content.",
      "We create one narrow invoke grant with an argument constraint. A fresh run now completes, and the receiver records exactly one approved fictional publication.", async () => {
        grant = await session.save(desired.grant);
        await probe("allowed", CONTENT, "allowed");
      });
    await session.scene("G04", ["S10", "S35"], "Reject changed arguments without changing the grant.",
      "The next run changes the content. The grant's exact argument constraint rejects it, and independent receiver readback confirms zero publications for this new identity.", async () => {
        await probe("arguments", "UNAPPROVED DEMO CONTENT", "arguments");
      });
    await session.scene("G05", ["S10", "S35"], "Expire the same grant and verify that a fresh valid request is denied.",
      "We preserve the grant identity but set its validity to a past time. Even the previously permitted content is now denied on a new run. Earlier successful and failed outcomes remain unchanged.", async () => {
        const expired = await session.get("toolgrants", grant.metadata.name);
        assert.equal(expired.metadata.uid, grant.metadata.uid);
        expired.spec.conditions = { validUntil: "2000-01-01T00:00:00Z" };
        await session.save(expired, false);
        await probe("expired", CONTENT, "expired");
      });
    const requester = JSON.parse(await session.kube(["auth", "whoami", "-o", "json"])).status.userInfo.username;
    const waiting = async name => session.wait("workflow approval gate", async () => {
      const run = await session.get("workflowruns", name);
      if (run.status?.phase === "Failed") throw new Error("Approval demonstration failed before its gate");
      return run.status?.activeStep === "publish" &&
        run.status.conditions?.some(condition => condition.reason === "WaitingApproval") ? run : false;
    });
    const decide = async (run, decision, reason) => {
      const gate = session.page.locator("#preview-host .run-approval-gate");
      await expect(gate).toBeVisible({ timeout: 30000 });
      await gate.getByPlaceholder("e.g. reviewed by me").fill(reason);
      const endpoint = "/api/resources/approvaldecisions";
      session.arm({ method: "POST", path: endpoint, validateBody(body) {
        assert.equal(body.apiVersion, "kognition.dev/v1alpha1");
        assert.equal(body.kind, "ApprovalDecision");
        assert.equal(body.metadata.namespace, session.namespace);
        assert(new RegExp(`^studio-${decision}-[a-z0-9-]+$`).test(body.metadata.name));
        assert(body.metadata.name.length <= 63);
        assert.deepEqual(body.spec, { workflowRunRef: { name: run.metadata.name }, step: "publish", decision, reason });
      } });
      const [response] = await Promise.all([
        session.page.waitForResponse(item => item.request().method() === "POST" && new URL(item.url()).pathname === endpoint),
        session.click(gate.getByRole("button", { name: decision === "deny" ? "Reject" : "Approve", exact: true })),
      ]);
      const record = await response.json();
      assert(response.ok(), `Authenticated workflow decision failed (HTTP${response.status()})`);
      assert.equal(record.spec.approver, requester);
      assert.equal(record.spec.decision, decision);
      assert.equal(record.spec.workflowRunRef.name, run.metadata.name);
      session.report.evidence.push({ type: "authenticated-scripted-decision", record });
      return record;
    };
    await session.scene("G06", ["S24"], "Reject a separate workflow before publication.",
      "A workflow gate is different from a Tool grant. This authenticated scripted rejection ends a separate run before its publication step. The target still has no publication for that identity.", async () => {
        await session.save(desired.calculatorGrant);
        await session.save(desired.publicationGrant);
        const definition = await session.save(desired.rejection);
        const publicationID = `${options.runId}-rejected`;
        assert.equal((await receipt(publicationID)).publicationCount, 0);
        const run = await session.startWorkflow(definition, "Show an explicit scripted rejection.", { publicationID, content: CONTENT });
        await waiting(run.metadata.name);
        await decide(run, "deny", "Automated tutorial rejection; no independent human review.");
        const rejected = await session.wait("approval rejection settlement", async () => {
          const current = await session.get("workflowruns", run.metadata.name);
          return current.status?.phase === "Failed" ? current : false;
        });
        assert.equal((await receipt(publicationID)).publicationCount, 0);
        session.report.rejectedGate = rejected;
        await session.inspectObserved("workflowruns", rejected.metadata.name, ["Failed"], "rejected-workflow-gate");
      });
    await session.scene("G07", ["S24"], "Show one vote remaining insufficient for a two-approver quorum, then observe expiry.",
      "This gate requires two distinct approvers. One authenticated scripted vote does not release publication. We let the bounded gate expire and verify zero target effects. This is not a demonstration of two independent human reviewers.", async () => {
        const definition = await session.save(desired.quorum);
        const publicationID = `${options.runId}-quorum`;
        assert.equal((await receipt(publicationID)).publicationCount, 0);
        const run = await session.startWorkflow(definition, "Show insufficient quorum and expiry.", { publicationID, content: CONTENT });
        await waiting(run.metadata.name);
        const decision = await decide(run, "approve", "Automated single tutorial vote; not a two-person review.");
        await session.page.waitForTimeout(6000);
        const oneVote = await session.get("workflowruns", run.metadata.name);
        assert.equal(oneVote.status.phase, "Pending");
        assert(oneVote.status.conditions.some(condition => condition.reason === "WaitingApproval"));
        assert.equal((await receipt(publicationID)).publicationCount, 0);
        session.report.insufficientQuorum = { run: oneVote, decision, publicationCount: 0 };
        await session.hold(session.page.locator("#preview-host .run-approval-gate"), "insufficient-quorum");
        const expired = await session.wait("approval timeout settlement", async () => {
          const current = await session.get("workflowruns", run.metadata.name);
          return current.status?.phase === "Failed" ? current : false;
        }, 120000);
        assert(expired.status.conditions.some(condition => condition.reason === "ApprovalRejected"));
        const events = JSON.parse(await session.kube(["get", "events", "-n", session.namespace,
          "--field-selector", `involvedObject.uid=${expired.metadata.uid}`, "-o", "json"])).items;
        assert(events.some(event => event.reason === "ApprovalTimedOut"), "Actual events must distinguish timeout from an explicit rejection");
        assert.equal((await receipt(publicationID)).publicationCount, 0);
        session.report.expiredQuorum = { run: expired, events };
        await session.inspectObserved("workflowruns", expired.metadata.name, ["Failed"], "expired-quorum");
      });
    const after = await session.get("agents", principal.metadata.name);
    assert.equal(after.metadata.uid, principal.metadata.uid);
    assert.equal(after.spec.replicas, 0);
    assert.equal((await session.list("agentinstances", `kognition.dev/agent=${principal.metadata.name}`)).length, 0);
    assert.equal((await session.list("agentjobs")).filter(item => item.spec.agentRef?.name === principal.metadata.name).length, 0);
    session.report.principal = after;
    session.report.zeroNewPrincipalAgentExecutions = true;
  });
}

if (require.main === module) runGovernance(parseContinuationOptions(process.argv.slice(2))).catch(error => {
  console.error(error.message); process.exitCode = 1;
});

module.exports = { CONTENT, POLICY, fixtures, verifyGrantOutcome, runGovernance };
