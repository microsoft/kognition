// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const fs = require("node:fs");
const path = require("node:path");
const http = require("node:http");
const crypto = require("node:crypto");
const { spawn, execFile } = require("node:child_process");
const { promisify } = require("node:util");
const { chromium, expect } = require("@playwright/test");
const { APPROVED, RANCHER_TARGET, RANCHER_APPROVED, recordingTarget, verifyTarget,
  finalizeRecording, parseOptions: parseAuthoringOptions } = require("./live-authoring");
const { stopChildren } = require("./live-pilot");
const { READING_POLICY, holdReadableText } = require("./reading");

const execute = promisify(execFile);
const VIEWPORT = Object.freeze({ width: 1920, height: 1080 });
const CLI_SHA256 = "da20389ca3cf8ef313281af37b47a8f5957ff0d3764e062d8b2ff3cbcf45db27";
const ENGINEERING_UID = "7b571eaa-2329-42f1-84f5-46a6d93aea70";
const RUNTIME = path.resolve("C:\\Users\\thiagosalles\\Videos\\Kognition\\full-tutorial-20261029-220040.runtime");
const PRODUCTION = path.join(RUNTIME, "full-master-20261103-203444");
const RANCHER_RUNTIME = path.resolve("C:\\Users\\thiagosalles\\Videos\\Kognition\\rancher-tutorial-20261104.runtime");
const RANCHER_PRODUCTION = path.join(RANCHER_RUNTIME, "full-production-20261105");
const COMPONENT_PATTERN = "^(TestDecodeBackendAcceptsTypedConfiguration|TestDecodeBackendRequiresNamespacedLiveObject)$";
const SOURCE_EXCERPTS = Object.freeze({
  probes: ["api\\v1alpha1\\probe.go", 11, 21],
  backends: ["internal\\memory\\serviceapp\\app.go", 109, 140],
  privacy: ["internal\\controller\\memory_rtbf.go", 19, 30],
  sources: ["runtime\\ingester\\ingest.py", 443, 459],
  binding: ["api\\v1alpha1\\memory.go", 546, 563],
  drivers: ["cmd\\kognition-controller-manager\\main.go", 215, 222],
  runtime: ["internal\\runtime\\contract.go", 15, 34],
  imports: ["cmd\\kognitionctl\\cmd\\import.go", 49, 64],
  modes: ["cmd\\kognitionctl\\cmd\\root.go", 28, 35],
});

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

function parseOptions(args) {
  const continuation = args.indexOf("--continue-from");
  if (continuation !== -1 && (args.lastIndexOf("--continue-from") !== continuation ||
      !args[continuation + 1] || args[continuation + 1].startsWith("--"))) {
    throw new Error("--continue-from requires one prior failed operator report");
  }
  const options = parseAuthoringOptions(continuation === -1 ? args :
    [...args.slice(0, continuation), ...args.slice(continuation + 2)]);
  const runtime = options.target === RANCHER_TARGET ? RANCHER_RUNTIME : RUNTIME;
  const production = options.target === RANCHER_TARGET ? RANCHER_PRODUCTION : PRODUCTION;
  if (options.runtime.toLowerCase() !== runtime.toLowerCase()) throw new Error("Only the exact approved runtime directory is allowed");
  if (path.dirname(options.output).toLowerCase() !== production.toLowerCase() ||
      !/^operator-[a-z0-9-]+$/.test(path.basename(options.output))) {
    throw new Error("Output must be a fresh operator-* child of the approved full-production root");
  }
  if (continuation !== -1) options.continuationPath = path.resolve(args[continuation + 1]);
  return options;
}

function sourceRoot(runtime) {
  if (![RUNTIME, RANCHER_RUNTIME].some(approved => path.resolve(runtime).toLowerCase() === approved.toLowerCase())) {
    throw new Error("Only allowlisted installed-source runtime paths are readable");
  }
  return path.join(RUNTIME, "cold-start-repair-v1", "source");
}

function inspectFrozen(runtime, id) {
  if (![RUNTIME, RANCHER_RUNTIME].some(approved => path.resolve(runtime).toLowerCase() === approved.toLowerCase()) ||
      !Object.hasOwn(SOURCE_EXCERPTS, id)) {
    throw new Error("Only allowlisted installed-source excerpts are readable");
  }
  const [relative, first, last] = SOURCE_EXCERPTS[id];
  const file = path.join(sourceRoot(runtime), relative);
  const lines = fs.readFileSync(file, "utf8").split(/\r?\n/);
  if (lines.length < last) throw new Error("Frozen source excerpt is unavailable");
  return [
    `FROZEN SOURCE WALKTHROUGH: ${relative}:${first}-${last}`,
    `file SHA256: ${sha256(file)}`,
    `file modified: ${fs.statSync(file).mtime.toISOString()}`,
    "Selected lines only; configuration/source evidence, not deployed acceptance.",
    ...lines.slice(first - 1, last).map((line, index) => `${first + index}: ${line}`),
  ].join("\n") + "\n";
}

function inspectBuild(runtime) {
  const base = path.dirname(sourceRoot(runtime));
  const buildFile = path.join(base, "controller-build.json");
  const imageFile = path.join(base, "controller-image.json");
  const build = JSON.parse(fs.readFileSync(buildFile, "utf8"));
  const image = JSON.parse(fs.readFileSync(imageFile, "utf8"));
  return JSON.stringify({
    scope: "Historical build artifacts; compare their digest with the independent live Deployment read.",
    buildFile, buildFileSHA256: sha256(buildFile), binarySHA256: build.sha256,
    platform: build.platform, allCommandsBuildPassed: build.allCommandsBuildPassed,
    imageFile, imageFileSHA256: sha256(imageFile), linuxManifestDigest: image.linuxManifestDigest,
    binaryMatchVerified: image.binaryMatchVerified,
    currentActivationNotProvenByBuildArtifact: true,
  }, null, 2) + "\n";
}

function readPriorComponent(file) {
  const absolute = path.resolve(file);
  const production = path.dirname(path.dirname(absolute));
  const approved = production.toLowerCase() === RANCHER_PRODUCTION.toLowerCase() ? RANCHER_APPROVED : APPROVED;
  if (path.basename(absolute) !== "report.json" ||
      ![PRODUCTION, RANCHER_PRODUCTION].some(root => production.toLowerCase() === root.toLowerCase()) ||
      !path.basename(path.dirname(absolute)).startsWith("operator-")) {
    throw new Error("Continuation evidence must be a production operator report");
  }
  const prior = JSON.parse(fs.readFileSync(absolute, "utf8"));
  const command = prior.commands?.find(item => item.id === "pgvector-component");
  const completedPrefix = prior.scenes?.filter(scene => Number(scene.id.slice(1)) < 43);
  if (prior.batch !== "read-only-operator" || prior.passed !== false || !prior.failure ||
      prior.identity?.controllerUID !== approved.controllerUID || prior.identity?.cliSHA256 !== CLI_SHA256 ||
      prior.safety?.liveMutations !== 0 || !prior.recording?.bytes ||
      !command || command.passed !== false || command.exitCode === 0 ||
      completedPrefix?.length !== 9 ||
      !completedPrefix.every((scene, index) => scene.passed && scene.id === `S${index + 34}`) ||
      path.dirname(path.resolve(prior.recording.path)).toLowerCase() !== path.dirname(absolute).toLowerCase()) {
    throw new Error("Prior report is not a preserved, independently targeted component failure");
  }
  if (fs.statSync(prior.recording.path).size !== prior.recording.bytes) throw new Error("Prior raw recording differs");
  return { prior, command, reportPath: absolute };
}

function inspectPriorComponent(file) {
  const { command, reportPath } = readPriorComponent(file);
  return [
    "SAVED COMPONENT FAILURE INSPECTION — not rerun and not a passed test.",
    `source report: ${reportPath}`,
    `report SHA256: ${sha256(reportPath)}`,
    `original command: ${command.command}`,
    `original start: ${command.startedAt}`,
    `original completion: ${command.completedAt}`,
    `original exit code: ${command.exitCode}`,
    `original failure: ${command.failure}`,
    "original stdout (verbatim):", command.stdout,
    "original stderr (verbatim):", command.stderr,
    "No database fixture or provider acceptance is claimed. Integration tests were not selected.",
  ].join("\n") + "\n";
}

function quoted(value) {
  return `'${String(value).replaceAll("'", "''")}'`;
}

function displayCommand(command) {
  const environment = Object.entries(command.environment || {})
    .map(([name, value]) => `$env:${name}=${quoted(value)}; `).join("");
  return `${environment}& ${quoted(command.program)} ${command.args.map(quoted).join(" ")}`;
}

function buildPlan(options) {
  const APPROVED = recordingTarget(options.target);
  const kubeconfig = path.join(options.runtime, "kubeconfig");
  const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
  const common = ["--kubeconfig", kubeconfig, "--context", APPROVED.context, "--request-timeout=20s"];
  const environment = { KUBECONFIG: kubeconfig };
  const kube = (id, args, extra = {}) => ({ id, program: "kubectl", args: [...common, ...args],
    environment, timeout: 30000, expectedExit: 0, ...extra });
  const ctl = (id, args) => ({ id, program: cli, args: [...args, "--mode", "kubernetes",
    "--kube-context", APPROVED.context, "--namespace", APPROVED.namespace,
    "--kube-namespace", APPROVED.systemNamespace], environment, timeout: 30000, expectedExit: 0 });
  const selected = (id, kind, name, namespace, fields) => kube(id, ["get", kind, ...(name ? [name] : []),
    ...(namespace ? ["-n", namespace] : []), "-o", `jsonpath=${fields.map(([label, field]) =>
      `${label}={.${field}}{"\\n"}`).join("")}`]);
  const table = (id, kinds, namespace, columns, names = []) => kube(id,
    ["get", kinds, ...names, ...(namespace ? ["-n", namespace] : []), "-o", `custom-columns=${columns}`]);
  const source = id => ({ id: `source-${id}`, program: process.execPath,
    args: [__filename, "--inspect-frozen", options.runtime, id], timeout: 10000, expectedExit: 0,
    scope: "frozen-source walkthrough; not deployed acceptance" });
  const scenes = [
    {
      id: "S34", title: "Workspace scope and workload identity", behaviors: ["B54", "B69", "B53"],
      subactions: ["1: namespace/Workspace inventory", "2: generated identity and role references", "5: metadata-only identity distinction"],
      narration: "These are the two real tutorial namespaces. The support identity has SPIFFE attribution and separate role bindings. Empty Workspace or cross-namespace-grant inventories mean those resources are not configured here. Attribution metadata is not evidence of verified SPIRE authentication.",
      limits: "No denied/allowed reference mutation, runtime authorization pair, communication test, enrollment, certificate expiry or outage test.",
      commands: [
        table("namespace-identity", "namespace", "", "NAME:.metadata.name,UID:.metadata.uid,PHASE:.status.phase",
          [APPROVED.namespace, "tutorial-engineering"]),
        table("workspace-roles", "workspaces,agentroles,agentrolebindings,crossnamespacegrants", APPROVED.namespace,
          "KIND:.kind,NAME:.metadata.name,ROLE:.spec.roleRef.name"),
        selected("support-identity", "agentidentity", "agent-identity-rehearsal-story-support", APPROVED.namespace,
          [["agent", "spec.agentRef.name"], ["SPIFFE attribution", "spec.spiffeID"], ["phase", "status.phase"],
            ["verified workload identity", "status.workloadIdentity.ready"]]),
      ],
    },
    {
      id: "S35", title: "Admission and quota configuration", behaviors: ["B55", "B56", "B17"],
      subactions: ["1/3/4: configured policy inventory", "5: real quota specification and status projection"],
      narration: "The live quota exposes resource and token limits beside observed usage. Limit ranges, admission policies and runtime policies are separate resources; the inventory shows what is actually configured. Status is a projection, not a demonstration of the durable reservation ledger.",
      limits: "No resource creation/defaulting, policy rejection, exhausted-quota request, settlement/restart proof or rate-cap interval variants.",
      commands: [
        selected("quota-projection", "cognitivequota", "tutorial-budget", APPROVED.namespace,
          [["hard limits", "spec.hard"], ["token budget", "spec.tokenBudget"], ["cost budget", "spec.costBudget"],
            ["reset interval", "spec.resetInterval"], ["used", "status.used"], ["token usage", "status.tokenUsage"],
            ["cost usage", "status.costUsage"]]),
        table("policy-inventory", "cognitivelimitranges,cognitiveadmissionpolicies,agentpolicies", APPROVED.namespace,
          "KIND:.kind,NAME:.metadata.name,FAILURE_POLICY:.spec.failurePolicy,MODE:.spec.enforcementMode"),
      ],
    },
    {
      id: "S36", title: "Read-only operational CLI", behaviors: ["B03", "B02", "B74"],
      subactions: ["5: actual get and immutable revisions", "4: actual missing-resource read", "6: explicit backend selection"],
      narration: "Every command names the Kubernetes context and uses this dedicated kubeconfig. The pinned CLI reads live Agents and immutable revision history. The missing-resource command is intentionally read-only and its nonzero exit is retained. This terminal viewer is recording tooling, not Kognition Studio or Dashboard.",
      limits: "No Dashboard interaction, edit/cancel/save, optimistic write conflict, watch, plugin invocation, workflow log output or standalone invocation.",
      commands: [
        ctl("cli-agents", ["get", "agents"]),
        ctl("cli-revisions", ["revisions", "agent", "rehearsal-story-support"]),
        kube("missing-resource", ["get", "agent", "operator-missing-read-only-" + options.runId,
          "-n", APPROVED.namespace, "-o", "custom-columns=NAME:.metadata.name"], { expectedExit: 1 }),
      ],
    },
    {
      id: "S37", title: "Accounting and retained evidence identities", behaviors: ["B56", "B58", "B59", "B35"],
      subactions: ["4: bounded real cost query with grouping/time/pagination", "1: audit metadata inventory only"],
      narration: "This real cost query is time-bounded, grouped and paginated. Its units are normalized micro-US dollars, not a provider invoice. Zero configured cost does not prove a free operation. The next command reads three immutable audit-object identities, not audit payloads or a correlated trace.",
      limits: "No exported trace spans, scrape measurements, weighted metric calculations, ledger settlement inspection or configured Workspace retention acceptance.",
      commands: [
        ctl("normalized-cost", ["cost", "--start", options.target === RANCHER_TARGET ? "2026-11-05T00:00:00Z" : "2026-11-02T00:00:00Z",
          "--end", options.target === RANCHER_TARGET ? new Date().toISOString() : "2026-11-04T00:00:00Z",
          "--group-by", "workspace,agent,model", "--limit", "3", "--offset", "0"]),
        kube("audit-identities", ["get", "configmaps", "-n", APPROVED.systemNamespace,
          "-l", "kognition.dev/cognitive-audit=true", "-o",
          'jsonpath={range .items[0:3]}name={.metadata.name}{"\\n"}uid={.metadata.uid}{"\\n"}immutable={.immutable}{"\\n"}{end}'],
        { selection: "Only the first three matching metadata records; no ConfigMap data or binaryData is requested." }),
      ],
    },
    {
      id: "S38", title: "Replica bounds versus autoscaling evidence", behaviors: ["B07", "B60", "B61", "B52"],
      subactions: ["1/4: autoscaler configuration inventory", "3: present Agent replica projection, not routed invocation"],
      narration: "The Agent's requested and ready replicas are directly observable. AgentAutoscaler and ModelAutoscaler are different resources. An empty scaler inventory does not demonstrate scale-up, stabilization or model fallback. We leave this installation unchanged rather than manufacturing a scaling result.",
      limits: "No load, replica transition, fallback/recovery, AgentService routing result or throughput certification.",
      commands: [
        selected("agent-replicas", "agent", "rehearsal-story-support", APPROVED.namespace,
          [["agent", "metadata.name"], ["generation", "metadata.generation"], ["runtime class", "spec.runtime.class"],
            ["requested replicas", "spec.replicas"], ["observed replicas", "status.replicas"],
            ["ready replicas", "status.readyReplicas"], ["phase", "status.phase"]]),
        table("scaler-inventory", "agentautoscalers,modelautoscalers,agentservices", APPROVED.namespace,
          "KIND:.kind,NAME:.metadata.name,MIN:.spec.minReplicas,MAX:.spec.maxReplicas,PHASE:.status.phase"),
      ],
    },
    {
      id: "S39", title: "Probe and recovery contracts", behaviors: ["B08", "B18", "B62"],
      subactions: ["1: actual probe/ladder fields and frozen API walkthrough"],
      narration: "The live Agent currently has no configured probe or restart ladder in these selected fields. The installed-source API separates health, readiness and reasoning probes. Reading that contract is not a canary execution and does not prove a restart, rollback, degradation, quarantine or alert.",
      limits: "No inference, dependency failure, threshold crossing, restart-ladder action, rollback or Tool exec-probe acceptance.",
      commands: [
        selected("probe-configuration", "agent", "rehearsal-story-support", APPROVED.namespace,
          [["probes", "spec.probes"], ["restart ladder", "spec.restartLadder"], ["probe status", "status.probeStatus"],
            ["phase", "status.phase"]]),
        source("probes"),
      ],
    },
    {
      id: "S40", title: "Actual memory backend selection", behaviors: ["B26", "B24"],
      subactions: ["1: selected live service backend", "6: embedding and backend limits; frozen selection walkthrough"],
      narration: "The actual Memory Service selects pgvector. Only that nonsecret environment value is requested. The frozen implementation selects in-memory, Mem0 or pgvector; a class name is not another shipped backend. This tutorial's recall uses lexical BM25 without an embedder, not verified vector semantic search.",
      limits: "No backend CRUD, process restart, new persistence test, Mem0 connectivity, embedding setup or Redis acceptance.",
      commands: [
        kube("memory-backend", ["get", "deployment", "kognition-tutorial-memory-service", "-n", APPROVED.systemNamespace,
          "-o", 'jsonpath={range .spec.template.spec.containers[*].env[?(@.name=="MEMORY_BACKEND_TYPE")]}{.name}={.value}{"\\n"}{end}']),
        source("backends"),
      ],
    },
    {
      id: "S41", title: "Memory lifecycle without erasing evidence", behaviors: ["B23", "B25", "B28"],
      subactions: ["4: actual claim/class/volume inventory only", "5/6: supported tombstone scope walkthrough"],
      narration: "The retained claim is bound to the existing memory volume. The class's reclaim setting is configuration, not measured deletion or archival. The source contract follows first-class derived MemoryRecords. Tombstoning those records is not proof of erasure from every backend, backup or external copy.",
      limits: "No RTBF request, derived-record fixture, aggregation, reclaim mutation, backend erasure or backup verification.",
      commands: [
        selected("memory-claim", "memoryclaim", "story-customer-memory", APPROVED.namespace,
          [["UID", "metadata.uid"], ["class", "spec.memoryClassName"], ["type", "spec.type"],
            ["phase", "status.phase"], ["bound volume", "status.boundVolumeName"]]),
        selected("memory-class", "memoryclass", "default", "",
          [["backend type", "spec.backendType"], ["reclaim policy", "spec.reclaimPolicy"]]),
        source("privacy"),
      ],
    },
    {
      id: "S42", title: "Knowledge sources: deployed inline versus source contracts", behaviors: ["B19", "B21", "B76"],
      subactions: ["1: existing inline corpus counts", "6: unsupported-source rejection in frozen ingester"],
      narration: "The existing inline KnowledgeBase reports its real document and chunk counts. The frozen ingester dispatches inline, URL, Git and object-storage handlers and rejects unsupported source types. That code is not new ingestion evidence for HTTP, Git, ConfigMap, Azurite, MinIO or real cloud storage.",
      limits: "No refresh, upload, retrieval call, source variant fixture, filter experiment, external credentials or emulator/cloud acceptance.",
      commands: [
        selected("knowledge-counts", "knowledgebase", "story-orbital-docs", APPROVED.namespace,
          [["UID", "metadata.uid"], ["generation", "metadata.generation"], ["class", "spec.knowledgeBaseClassName"],
            ["source types", "spec.sources[*].type"], ["documents", "status.documentCount"],
            ["indexed chunks", "status.indexedChunks"], ["phase", "status.phase"]]),
        source("sources"),
      ],
    },
    {
      id: "S43", title: "Typed provider binding and isolated component validation", behaviors: ["B77", "B78", "B79"],
      subactions: ["2: frozen immutable binding fields", "6: isolated typed-pgvector configuration contract tests"],
      narration: "The binding contract includes exact resource and operation identities. These two selected pgvector tests exercise configuration validation with simulated dependencies. They do not connect to a database or install a provider. The typed package is not a complete deployed ingestion, query-caller or cleanup integration.",
      limits: "Component contract test / simulated dependencies / not deployed acceptance. No provider Query/Index/Cleanup, mTLS exchange, stale-binding experiment or finalizer mutation. Unselected integration tests are not claimed passed.",
      commands: [
        source("binding"),
        { id: "pgvector-component", program: "go", args: ["test", ".\\providers\\pgvector", "-run", COMPONENT_PATTERN,
          "-count=1", "-v", "-timeout=90s"], cwd: sourceRoot(options.runtime), timeout: 150000, expectedExit: 0,
        scope: "component contract test / simulated dependencies / not deployed acceptance",
        environment: { ...environment, GOTMPDIR: path.join(options.output, "go-work"),
          GOPROXY: "off", GOSUMDB: "off", GOTOOLCHAIN: "local", GOFLAGS: "-mod=readonly" } },
      ],
    },
    {
      id: "S44", title: "Runtime and model-driver extension boundaries", behaviors: ["B65", "B66", "B67", "B70"],
      subactions: ["1: live default runtime selection from S38", "3: frozen runtime API walkthrough", "7: registered-driver source"],
      narration: "The current Agent uses the default runtime. The frozen code registers five model drivers and defines session operations separately from Studio controls. Driver registration is not live acceptance for every provider. Optional AutoGen streaming, cancellation and encrypted recovery remain unqualified in this recording.",
      limits: "No new model call, remote-provider test, AutoGen worker execution, checkpoint read, decryption, cancellation or loss/recovery test.",
      commands: [source("drivers"), source("runtime")],
    },
    {
      id: "S45", title: "Offline import and model candidate review", behaviors: ["B63", "B64", "B68"],
      subactions: ["1/2: offline hash-bound planning contract walkthrough", "5: actual ModelClass candidate projection"],
      narration: "Offline compatibility planning requires explicit source bytes, a matching digest, a namespace, a Model and tool mappings. It is not permission to execute foreign providers. The live class projection shows current candidates and binding fields without changing them. Signature admission and canary promotion need separate acceptance.",
      limits: "No plan/render execution, foreign-module execution, signing keys, detached signature admission, tampering test or candidate canary/promotion.",
      commands: [
        source("imports"),
        selected("model-candidates", "modelclass", "tutorial-foundry", "",
          [["provisioner", "spec.provisioner"], ["version pin", "spec.versionPin"], ["candidate model names", "spec.versionCandidates.*.modelName"],
            ["active version", "status.activeVersion"], ["active model", "status.activeBinding.modelName"], ["phase", "status.phase"]]),
      ],
    },
    {
      id: "S46", title: "Standalone and Kubernetes are distinct modes", behaviors: ["B03", "B54", "B72", "B75"],
      subactions: ["3: mode-specific backend source walkthrough", "1: explicit Kubernetes selection throughout this chapter only"],
      narration: "The frozen CLI documents distinct transports: standalone HTTP versus Kubernetes resource and runtime paths. Every live command here selects Kubernetes explicitly. No separate standalone installation was started. Similar resource names do not prove feature parity, and protected approval authority is not ordinary generic CRUD.",
      limits: "No standalone invocation, HTTP-overlay acceptance, protected-route denial, role-authority mutation or blanket parity claim. The standalone overlay's knowledge-claim restriction remains disclosed, not re-tested.",
      commands: [source("modes")],
    },
    {
      id: "S47", title: "Pinned installation, build provenance and hardening inventory", behaviors: ["B69", "B71", "B72", "B73", "B74"],
      subactions: ["1: independent installed identity/image and historical build artifact", "3: migration Job status only", "5: configured hardening inventory"],
      narration: "The live controller retains its exact approved UID, generation and image digest. Historical build artifacts are compared with that installation, not treated as fresh deployment approval. The hardening inventory shows configured workload security. No activation, recovery, key rotation, installer or Helm rollback is performed.",
      limits: "No new install/activation/drain, database query or vector prerequisite validation, OIDC/TLS exchange, CNI enforcement test, key overlap/rotation or SPIRE acceptance. A completed migration Job is not independent database-schema verification.",
      commands: [
        selected("installed-controller", "deployment", APPROVED.controller, APPROVED.systemNamespace,
          [["UID", "metadata.uid"], ["generation", "metadata.generation"], ["observed generation", "status.observedGeneration"],
            ["ready replicas", "status.readyReplicas"], ["image", "spec.template.spec.containers[*].image"]]),
        table("pod-hardening", "deployments", APPROVED.systemNamespace,
          "NAME:.metadata.name,READY:.status.readyReplicas,NONROOT:.spec.template.spec.securityContext.runAsNonRoot,SA:.spec.template.spec.serviceAccountName",
          [APPROVED.controller, "kognition-tutorial-admission-guard", "kognition-tutorial-webhook"]),
        { id: "controller-build", program: process.execPath, args: [__filename, "--inspect-build", options.runtime],
          timeout: 10000, expectedExit: 0, scope: "historical build artifact walkthrough; not current activation acceptance" },
        table("migration-job-status", "jobs", options.target === RANCHER_TARGET ? APPROVED.systemNamespace : APPROVED.namespace,
          "NAME:.metadata.name,SUCCEEDED:.status.succeeded,FAILED:.status.failed",
          []),
      ],
    },
  ];
  if (!options.continuationPath) return scenes;
  const remaining = scenes.filter(scene => Number(scene.id.slice(1)) >= 43);
  const component = remaining[0];
  component.subactions = ["2: frozen immutable binding fields", "6: preserved failed component attempt; no pass claim"];
  component.narration = "The preceding read-only take reached a deadline while compiling the two selected pgvector component tests. They did not pass. We preserve that failure and continue only the source walkthrough. No database or provider acceptance, skipped-test pass, or production lifecycle integration is implied.";
  component.limits = "Component attempt failed at its deadline; original recording and command evidence retained. No test rerun, provider Query/Index/Cleanup, mTLS exchange, database fixture or deployed acceptance.";
  component.commands = [source("binding"), { id: "preserved-component-failure", program: process.execPath,
    args: [__filename, "--inspect-component-result", options.continuationPath], timeout: 10000, expectedExit: 0,
    scope: "Inspection of the saved failed component attempt — NOT a passed component test" }];
  return remaining;
}

function createCommandRunner(commands, directory, persist = () => {}) {
  const byID = new Map(commands.map(command => [command.id, command]));
  if (byID.size !== commands.length) throw new Error("Command IDs must be unique");
  const attempted = new Set();
  let busy = false;
  return async id => {
    const command = byID.get(id);
    if (!command || attempted.has(id) || busy) throw new Error("Unknown, repeated or overlapping command refused");
    busy = true;
    attempted.add(id);
    const stdoutPath = path.join(directory, `${id}.stdout.txt`);
    const stderrPath = path.join(directory, `${id}.stderr.txt`);
    fs.writeFileSync(stdoutPath, "", { flag: "wx" });
    fs.writeFileSync(stderrPath, "", { flag: "wx" });
    const record = { id, command: displayCommand(command), program: command.program, args: command.args,
      environmentOverrides: { ...command.environment },
      cwd: command.cwd || process.cwd(), startedAt: new Date().toISOString(), expectedExit: command.expectedExit,
      scope: command.scope || "live read-only selected-field observation", selection: command.selection,
      stdoutPath, stderrPath, passed: false };
    persist(record);
    try {
      return await new Promise((resolve, reject) => {
        const child = spawn(command.program, command.args, {
          cwd: command.cwd, env: { ...process.env, ...command.environment }, windowsHide: true,
          shell: false, stdio: ["ignore", "pipe", "pipe"],
        });
        record.pid = child.pid;
        persist(record);
        let bytes = 0, stopped = false;
        const stop = reason => {
          if (stopped) return;
          stopped = true;
          record.failure = reason;
          stopChildren([child]).catch(error => { record.stopFailure = error.message; });
        };
        const timer = setTimeout(() => stop("Command deadline exceeded; original output retained"), command.timeout || 30000);
        for (const [stream, file] of [[child.stdout, stdoutPath], [child.stderr, stderrPath]]) {
          stream.on("data", chunk => {
            fs.appendFileSync(file, chunk);
            bytes += chunk.length;
            if (bytes > 1024 * 1024) stop("Output exceeded the reviewed bound; original output retained, not silently truncated");
          });
        }
        child.once("error", error => {
          clearTimeout(timer);
          record.failure = error.message;
          record.completedAt = new Date().toISOString();
          persist(record);
          reject(error);
        });
        child.once("close", (code, signal) => {
          clearTimeout(timer);
          record.exitCode = code;
          record.signal = signal;
          record.completedAt = new Date().toISOString();
          record.stdout = fs.readFileSync(stdoutPath, "utf8");
          record.stderr = fs.readFileSync(stderrPath, "utf8");
          record.stdoutSHA256 = sha256(stdoutPath);
          record.stderrSHA256 = sha256(stderrPath);
          record.passed = !record.failure && code === command.expectedExit;
          persist(record);
          resolve(record);
        });
      });
    } finally {
      busy = false;
    }
  };
}

const VIEWER_HTML = `<!doctype html><html lang="en"><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>Kognition operator evidence</title>
<style>
*{box-sizing:border-box}body{margin:0;background:#101721;color:#e6edf5;font-family:Segoe UI,Arial,sans-serif}
header{position:sticky;top:0;z-index:5;padding:20px 34px;border-bottom:2px solid #5ec4b6;background:#172333}
header strong{display:block;color:#78e1d1;font-size:26px;margin-bottom:8px}
header span{font-size:19px;color:#c5d0df}main{padding:20px 34px}
h1{font-size:29px;margin:0 0 12px}.scope{font-size:18px;color:#ffcf80;margin:8px 0}
#framing{font-size:21px;line-height:1.5;padding:12px 0}#limits{color:#ffcf80}
#terminal{border:1px solid #56667a;border-radius:6px;overflow:hidden}
#command{white-space:pre-wrap;overflow-wrap:anywhere;font:16px/1.5 Consolas,monospace;margin:0;padding:16px;background:#1a293b}
#output{height:470px;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;tab-size:4;
font:20px/1.5 Consolas,monospace;padding:18px;margin:0;background:#0b111a}
#stamp{padding:12px 16px;font:17px/1.4 Consolas,monospace;color:#bdd1e4;background:#1a293b}
#run{font-size:18px;padding:8px 18px;margin-top:12px;color:#101721;background:#78e1d1;border:0;border-radius:3px}
</style><header><strong>Recording-only terminal viewer, not Kognition Studio</strong>
<span>Real subprocess stdout / stderr · exact commands, timestamps and exit codes · no live resource mutations</span></header>
<main><h1 id="title">Bounded operator chapter</h1><div class="scope" id="scope"></div>
<div id="framing"><div id="narration"></div><div id="limits"></div></div>
<div id="terminal"><pre id="command"></pre><pre id="output">Waiting for the reviewed read-only command.</pre><div id="stamp"></div></div>
<button id="run" disabled>Execute reviewed read-only command</button></main>
<script>
let current;
async function load(id){
 const response=await fetch('/command?id='+encodeURIComponent(id));
 if(!response.ok)throw Error('Command unavailable');
 current=await response.json();
 document.getElementById('title').textContent=current.scene+' — '+current.title;
 document.getElementById('scope').textContent=current.scope;
 document.getElementById('narration').textContent=current.narration;
 document.getElementById('limits').textContent='Limits: '+current.limits;
 document.getElementById('command').textContent='';
 document.getElementById('output').textContent='Command has not executed.';
 document.getElementById('stamp').textContent='';
 document.getElementById('run').disabled=false;
}
document.getElementById('run').onclick=async()=>{
 const button=document.getElementById('run');button.disabled=true;
 document.getElementById('stamp').textContent='Executing actual subprocess; no output has been fabricated.';
 const response=await fetch('/execute',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:current.id})});
 const record=await response.json();
 document.getElementById('output').textContent='STDOUT (verbatim)\\n'+record.stdout+'\\nSTDERR (verbatim)\\n'+record.stderr;
 document.getElementById('stamp').textContent='Started '+record.startedAt+' | Completed '+record.completedAt+' | Exit '+record.exitCode;
 document.getElementById('output').dataset.completed=record.id;
 if(!response.ok)document.getElementById('stamp').textContent+=' | FAILED; original evidence retained';
};
window.loadReviewedCommand=load;
</script></html>`;

async function startViewer(scenes, run, onRecord = () => {}) {
  const catalog = new Map();
  for (const scene of scenes) for (const command of scene.commands) catalog.set(command.id, {
    id: command.id, scene: scene.id, title: scene.title, narration: scene.narration, limits: scene.limits,
    scope: command.scope || "LIVE READ-ONLY / configuration walkthrough — not mutation acceptance",
    command: displayCommand(command),
  });
  const ids = [...catalog.keys()];
  let next = 0, origin, active = false;
  const server = http.createServer(async (request, response) => {
    response.setHeader("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'");
    response.setHeader("Cache-Control", "no-store");
    response.setHeader("X-Content-Type-Options", "nosniff");
    const url = new URL(request.url, origin);
    if (request.headers.host !== new URL(origin).host) { response.writeHead(403).end(); return; }
    if (request.method === "GET" && url.pathname === "/") {
      response.setHeader("Content-Type", "text/html; charset=utf-8"); response.end(VIEWER_HTML); return;
    }
    response.setHeader("Content-Type", "application/json; charset=utf-8");
    if (request.method === "GET" && url.pathname === "/health") {
      response.end(JSON.stringify({ recordingOnly: true, ready: true })); return;
    }
    if (request.method === "GET" && url.pathname === "/command" && catalog.has(url.searchParams.get("id"))) {
      response.end(JSON.stringify(catalog.get(url.searchParams.get("id")))); return;
    }
    if (request.method !== "POST" || url.pathname !== "/execute" || request.headers.origin !== origin || active) {
      response.writeHead(403).end('{"error":"Only the next reviewed command is executable"}'); return;
    }
    active = true;
    try {
      let body = "";
      for await (const chunk of request) {
        body += chunk;
        if (body.length > 1024) throw new Error("Command request exceeds bound");
      }
      const input = JSON.parse(body);
      if (Object.keys(input).length !== 1 || input.id !== ids[next]) throw new Error("Unknown, repeated or out-of-order command");
      next++;
      const record = await run(input.id);
      onRecord(record);
      response.writeHead(record.passed ? 200 : 422).end(JSON.stringify(record));
    } catch (error) {
      response.writeHead(400).end(JSON.stringify({ failure: error.message, stdout: "", stderr: "",
        exitCode: null, startedAt: new Date().toISOString(), completedAt: new Date().toISOString() }));
    } finally { active = false; }
  });
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  origin = `http://127.0.0.1:${server.address().port}`;
  const health = await fetch(`${origin}/health`, { signal: AbortSignal.timeout(5000) });
  if (!health.ok || !(await health.json()).recordingOnly) throw new Error("Recording viewer failed its independent health read");
  return { origin, server, close: () => new Promise(resolve => server.close(resolve)) };
}

async function preflight(options, report, persist) {
  const APPROVED = recordingTarget(options.target);
  const cli = path.join(options.runtime, "windows", "kognitionctl.exe");
  if (sha256(cli) !== CLI_SHA256) throw new Error("Pinned CLI SHA256 differs");
  const cluster = JSON.parse(fs.readFileSync(path.join(options.runtime, "cluster.json"), "utf8"));
  if (path.resolve(options.runtime, cluster.kubeconfig).toLowerCase() !== path.join(options.runtime, "kubeconfig").toLowerCase()) {
    throw new Error("Runtime kubeconfig path differs");
  }
  const common = ["--kubeconfig", path.join(options.runtime, "kubeconfig"), "--context", APPROVED.context, "--request-timeout=20s"];
  const read = async (id, args) => {
    const result = await execute("kubectl", [...common, ...args], { timeout: 30000, windowsHide: true,
      maxBuffer: 1024 * 1024, encoding: "utf8", env: { ...process.env, KUBECONFIG: path.join(options.runtime, "kubeconfig") } });
    report.preflightReads.push({ id, args: [...common, ...args], stdout: result.stdout, stderr: result.stderr,
      completedAt: new Date().toISOString() }); persist();
    return result.stdout.trimEnd().split(/\r?\n/);
  };
  const configFields = await read("selected-context-no-users", ["config", "view", "--minify", "-o",
    'jsonpath={.contexts[0].name}{"\\n"}{.contexts[0].context.cluster}{"\\n"}{.clusters[0].name}{"\\n"}{.clusters[0].cluster.server}{"\\n"}{.clusters[0].cluster.insecure-skip-tls-verify}{"\\n"}']);
  const config = { contexts: [{ name: configFields[0], context: { cluster: configFields[1] } }],
    clusters: [{ name: configFields[2], cluster: { server: configFields[3],
      "insecure-skip-tls-verify": configFields[4] === "true" } }] };
  const namespace = async name => {
    const fields = await read(`namespace-${name}`, ["get", "namespace", name, "-o",
      'jsonpath={.metadata.name}{"\\n"}{.metadata.uid}{"\\n"}{.status.phase}{"\\n"}{.metadata.deletionTimestamp}{"\\n"}']);
    return { kind: "Namespace", metadata: { name: fields[0], uid: fields[1], deletionTimestamp: fields[3] || undefined },
      status: { phase: fields[2] } };
  };
  const root = await namespace("kube-system"), support = await namespace(APPROVED.namespace);
  const engineering = await namespace("tutorial-engineering");
  const engineeringUID = options.target === RANCHER_TARGET ? "b224d60f-d101-4100-89bc-d2c8a286b1e0" : ENGINEERING_UID;
  if (engineering.metadata.uid !== engineeringUID || engineering.status.phase !== "Active" || engineering.metadata.deletionTimestamp) {
    throw new Error("Engineering namespace identity differs");
  }
  const fields = await read("controller-selected-fields", ["get", "deployment", APPROVED.controller, "-n", APPROVED.systemNamespace,
    "-o", 'jsonpath={.metadata.name}{"\\n"}{.metadata.namespace}{"\\n"}{.metadata.uid}{"\\n"}{.metadata.generation}{"\\n"}{.status.observedGeneration}{"\\n"}{.spec.replicas}{"\\n"}{.status.replicas}{"\\n"}{.status.readyReplicas}{"\\n"}{.status.availableReplicas}{"\\n"}{.status.updatedReplicas}{"\\n"}{.status.unavailableReplicas}{"\\n"}{.spec.template.spec.containers[*].image}{"\\n"}{.metadata.deletionTimestamp}{"\\n"}']);
  const controller = { kind: "Deployment",
    metadata: { name: fields[0], namespace: fields[1], uid: fields[2], generation: Number(fields[3]), deletionTimestamp: fields[12] || undefined },
    spec: { replicas: Number(fields[5]), template: { spec: { containers: (fields[11] || "").split(" ").map(image => ({ image })) } } },
    status: { observedGeneration: Number(fields[4]), replicas: Number(fields[6]), readyReplicas: Number(fields[7]),
      availableReplicas: Number(fields[8]), updatedReplicas: Number(fields[9]), unavailableReplicas: Number(fields[10] || 0) } };
  verifyTarget(cluster, config, root, support, controller, APPROVED);
  const overridesFile = path.join(path.dirname(sourceRoot(options.runtime)), "source-overrides.json");
  const overrides = JSON.parse(fs.readFileSync(overridesFile, "utf8"));
  for (const override of overrides.overrides) {
    if (sha256(path.join(sourceRoot(options.runtime), override.path)) !== override.overrideSHA256) {
      throw new Error("Installed-source override bytes differ from the build inventory");
    }
  }
  report.identity = { cliSHA256: CLI_SHA256, context: APPROVED.context, server: APPROVED.server,
    clusterUID: root.metadata.uid, namespaceUID: support.metadata.uid, engineeringUID: engineering.metadata.uid,
    controllerUID: controller.metadata.uid, controllerGeneration: controller.metadata.generation,
    controllerImage: controller.spec.template.spec.containers[0].image, sourceRoot: sourceRoot(options.runtime),
    sourceOverridesSHA256: sha256(overridesFile), sourceOverrideFilesVerified: overrides.overrides.length };
  report.approvedTarget = APPROVED;
  persist();
}

async function verifyMedia(input, output, report, persist) {
  const base = path.resolve(".hackaton2026\\video-production\\node_modules");
  const ffmpeg = path.join(base, "ffmpeg-static", "ffmpeg.exe");
  const ffprobe = path.join(base, "ffprobe-static", "bin", "win32", "x64", "ffprobe.exe");
  const converted = path.join(output, report.failure ? "operator.failed.25fps.mp4" : "operator.recording.25fps.mp4");
  await execute(ffmpeg, ["-hide_banner", "-loglevel", "error", "-n", "-i", input, "-an", "-vf", "fps=25",
    "-c:v", "libx264", "-preset", "fast", "-crf", "18", "-pix_fmt", "yuv420p", "-movflags", "+faststart", converted],
  { windowsHide: true, timeout: 600000, maxBuffer: 1024 * 1024 });
  const probe = JSON.parse((await execute(ffprobe, ["-v", "error", "-select_streams", "v:0", "-count_frames",
    "-show_entries", "stream=codec_name,width,height,r_frame_rate,avg_frame_rate,nb_read_frames:format=duration",
    "-of", "json", converted], { windowsHide: true, timeout: 600000, maxBuffer: 1024 * 1024 })).stdout);
  const stream = probe.streams?.[0];
  const duration = Number(probe.format?.duration);
  if (stream?.width !== VIEWPORT.width || stream?.height !== VIEWPORT.height || stream.avg_frame_rate !== "25/1" ||
      Math.abs(Number(stream.nb_read_frames) - duration * 25) > 2 || duration < 60) throw new Error("Operator video geometry/frame/duration verification failed");
  const decoded = await execute(ffmpeg, ["-hide_banner", "-v", "error", "-xerror", "-i", converted, "-f", "null", "-"],
    { windowsHide: true, timeout: 600000, maxBuffer: 1024 * 1024 });
  if (decoded.stderr.trim()) throw new Error("Full video decode reported errors");
  const frames = [];
  for (const index of [0, Math.floor(report.scenes.length / 2), report.scenes.length - 1]) {
    const scene = report.scenes[index];
    const reading = report.readingHolds.find(hold => hold.scene === scene.id && hold.command);
    const at = Math.min(duration - 1, reading ? reading.startSeconds + 2 : scene.startSeconds + 8);
    const file = path.join(output, `frame-${scene.id}.png`);
    await execute(ffmpeg, ["-hide_banner", "-loglevel", "error", "-n", "-ss", String(at), "-i", converted,
      "-frames:v", "1", file], { windowsHide: true, timeout: 30000, maxBuffer: 1024 * 1024 });
    frames.push({ scene: scene.id, seconds: at, path: file });
  }
  report.media = { path: converted, sha256: sha256(converted), bytes: fs.statSync(converted).size,
    probe, fullDecodePassed: true, decodedFrames: frames, narrationIncluded: false,
    role: "Continuous operator source chapter for the parent's narrated full master; not a full-tutorial completion claim" };
  persist();
}

async function runOperator(input) {
  const options = parseOptions(["--runtime", input.runtime, "--output", input.output, "--run-id", input.runId,
    ...(input.target === undefined ? [] : ["--target", input.target]),
    ...(input.continuationPath ? ["--continue-from", input.continuationPath] : []),
    ...(input.authorizedLiveWrites === true ? ["--authorize-live-writes"] : [])]);
  fs.mkdirSync(path.dirname(options.output), { recursive: true });
  fs.mkdirSync(options.output);
  const reportPath = path.join(options.output, "report.json");
  const report = { schemaVersion: 1, batch: "read-only-operator", runId: options.runId, startedAt: new Date().toISOString(),
    passed: false, completeTutorial: false, viewport: VIEWPORT, outputFrameRate: 25, readingPolicy: READING_POLICY,
    preflightReads: [], commands: [], scenes: [], readingHolds: [], browserErrors: [],
    safety: { liveMutations: 0, shellExecution: false, rawSecretsRead: false, rawKubeconfigRead: false,
      checkpointPayloadsRead: false, mockedLiveResponses: false, globalContextChanged: false,
      componentTests: "two explicitly selected frozen-source configuration tests; no database or provider calls" },
    coverage: { entireScenesCompleted: [], scope: "Selected observed subactions plus explicitly bounded walkthroughs only" },
    process: { recorderPID: process.pid, viewerPID: process.pid, ownedChildPIDs: [] } };
  const persist = () => fs.writeFileSync(reportPath, JSON.stringify(report, null, 2));
  persist();
  let browser, context, page, video, viewer, failure, epoch;
  try {
    await preflight(options, report, persist);
    if (options.continuationPath) {
      const { prior, reportPath: previous } = readPriorComponent(options.continuationPath);
      report.continuation = { reportPath: previous, priorRawVideo: prior.recording.path,
        firstTakeFailed: true, reason: prior.failure, selectedComponentTestsPassed: false,
        scope: "Separate continuous take S43-S47; preceding S34-S42 remain in the original source take" };
      report.safety.componentTests = "No rerun; inspect the retained failed component attempt with its original exit and output";
      persist();
    }
    const scenes = buildPlan(options);
    const sourceHashes = Object.fromEntries(Object.keys(SOURCE_EXCERPTS).map(id => {
      const [file] = SOURCE_EXCERPTS[id]; return [file, sha256(path.join(sourceRoot(options.runtime), file))];
    }));
    report.sourceHashes = sourceHashes;
    fs.mkdirSync(path.join(options.output, "commands"));
    fs.mkdirSync(path.join(options.output, "go-work"));
    const runner = createCommandRunner(scenes.flatMap(scene => scene.commands), path.join(options.output, "commands"), record => {
      if (!report.commands.includes(record)) report.commands.push(record);
      if (record.pid && !report.process.ownedChildPIDs.includes(record.pid)) report.process.ownedChildPIDs.push(record.pid);
      persist();
    });
    viewer = await startViewer(scenes, runner);
    report.process.viewerOrigin = viewer.origin;
    report.process.viewerResponsiveAt = new Date().toISOString(); persist();
    browser = await chromium.launch({ headless: true });
    context = await browser.newContext({ viewport: VIEWPORT, colorScheme: "dark", serviceWorkers: "block",
      recordVideo: { dir: options.output, size: VIEWPORT } });
    epoch = performance.now();
    report.videoStartedAt = new Date().toISOString();
    page = await context.newPage();
    video = page.video();
    report.recording = { rawPath: await video.path(), viewport: VIEWPORT }; persist();
    page.on("pageerror", error => { report.browserErrors.push(error.message); persist(); });
    await page.goto(viewer.origin, { waitUntil: "networkidle", timeout: 30000 });
    for (const scene of scenes) {
      const entry = { id: scene.id, title: scene.title, behaviors: scene.behaviors, subactions: scene.subactions,
        scope: "read-only observation / configuration or frozen-source walkthrough; not whole-scene acceptance",
        narrationDraft: scene.narration, limitations: scene.limits, startedAt: new Date().toISOString(),
        startSeconds: (performance.now() - epoch) / 1000, passed: false, commandIDs: [] };
      report.scenes.push(entry); persist();
      console.log(`${scene.id}: ${scene.title}`);
      let first = true;
      for (const command of scene.commands) {
        await page.evaluate(id => window.loadReviewedCommand(id), command.id);
        await expect(page.locator("#title")).toHaveText(`${scene.id} — ${scene.title}`);
        if (first) {
          const hold = await holdReadableText(page, page.locator("#framing"));
          report.readingHolds.push({ scene: scene.id, part: "scope/narration/limits", ...hold,
            endSeconds: (performance.now() - epoch) / 1000 }); first = false; persist();
        }
        const commandText = displayCommand(command);
        await page.locator("#command").evaluate((element, text) => { element.textContent = text; }, "");
        for (let offset = 0; offset < commandText.length; offset += 24) {
          await page.locator("#command").evaluate((element, text) => { element.textContent += text; },
            commandText.slice(offset, offset + 24));
          await page.waitForTimeout(12);
        }
        const [response] = await Promise.all([
          page.waitForResponse(item => item.url() === `${viewer.origin}/execute`, { timeout: command.timeout + 15000 }),
          page.locator("#run").click(),
        ]);
        await expect(page.locator("#output")).toHaveAttribute("data-completed", command.id, { timeout: 10000 });
        const record = report.commands.find(item => item.id === command.id);
        if (!record) throw new Error("Viewer omitted actual command evidence");
        entry.commandIDs.push(command.id);
        record.startSeconds = (performance.now() - epoch) / 1000;
        await expect(page.locator("#output")).toHaveText(`STDOUT (verbatim)\n${record.stdout}\nSTDERR (verbatim)\n${record.stderr}`);
        const hold = await holdReadableText(page, page.locator("#output"));
        report.readingHolds.push({ scene: scene.id, command: command.id, part: "verbatim stdout/stderr", ...hold,
          startSeconds: record.startSeconds, endSeconds: (performance.now() - epoch) / 1000 });
        await expect(page.locator("#stamp")).toContainText(`Exit ${record.exitCode}`);
        persist();
        if (!response.ok() || !record.passed) throw new Error(`Recorded command ${command.id} failed; preserve its original exit/output`);
        if (command.id === "missing-resource" &&
            (!record.stderr.includes('NotFound') || !record.stderr.includes(`operator-missing-read-only-${options.runId}`))) {
          throw new Error("The nonzero read did not prove the exact missing resource");
        }
      }
      entry.endSeconds = (performance.now() - epoch) / 1000;
      entry.completedAt = new Date().toISOString(); entry.passed = true; persist();
    }
    for (const [file, expectedHash] of Object.entries(sourceHashes)) {
      if (sha256(path.join(sourceRoot(options.runtime), file)) !== expectedHash) throw new Error("Frozen source changed during capture");
    }
    await preflight(options, report, persist);
    if (report.browserErrors.length) throw new Error("Recording-only viewer reported browser errors");
  } catch (error) {
    failure = error; report.failure = error.message; persist();
    if (page && !page.isClosed()) await page.screenshot({ path: path.join(options.output, "failure.png") }).catch(() => {});
  } finally {
    const errors = [];
    if (context && browser && video) {
      const destination = path.join(options.output, failure ? "operator.failed.webm" : "operator.recording.webm");
      try { Object.assign(report.recording, await finalizeRecording(context, browser, video, destination)); }
      catch (error) { errors.push(error.message); }
    } else {
      if (context) await context.close().catch(error => errors.push(error.message));
      if (browser) await browser.close().catch(error => errors.push(error.message));
    }
    if (viewer) await viewer.close().catch(error => errors.push(error.message));
    report.process.viewerClosedAt = new Date().toISOString();
    if (errors.length) { report.cleanupFailures = errors; failure ||= new Error(errors.join("; ")); }
    report.captureCompletedAt = new Date().toISOString(); persist();
  }
  if (!failure && report.recording?.bytes) {
    try { await verifyMedia(report.recording.path, options.output, report, persist); }
    catch (error) { failure = error; report.failure = error.message; }
  }
  report.passed = !failure;
  report.completedAt = new Date().toISOString(); persist();
  if (failure) throw new Error(`Operator batch failed; inspect retained ${reportPath}: ${failure.message}`);
  console.log(`Verified operator chapter: ${report.media.path}\nReport: ${reportPath}`);
  return report;
}

if (require.main === module) {
  Promise.resolve().then(() => {
    const args = process.argv.slice(2);
    if (args[0] === "--inspect-frozen" && args.length === 3) process.stdout.write(inspectFrozen(args[1], args[2]));
    else if (args[0] === "--inspect-build" && args.length === 2) process.stdout.write(inspectBuild(args[1]));
    else if (args[0] === "--inspect-component-result" && args.length === 2) process.stdout.write(inspectPriorComponent(args[1]));
    else return runOperator(parseOptions(args));
  }).catch(error => { console.error(error.message); process.exitCode = 1; });
}

module.exports = { VIEWPORT, CLI_SHA256, RUNTIME, PRODUCTION, RANCHER_RUNTIME, RANCHER_PRODUCTION, COMPONENT_PATTERN, SOURCE_EXCERPTS,
  parseOptions, inspectFrozen, inspectBuild, readPriorComponent, inspectPriorComponent,
  displayCommand, buildPlan, createCommandRunner, startViewer, verifyMedia, runOperator };
