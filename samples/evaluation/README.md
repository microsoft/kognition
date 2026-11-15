# Evaluation Sample

This sample creates an `EvaluationDataset` named `hello-agent-smoke-dataset`
and an `Evaluation` named `hello-agent-quality` for the `hello-agent` from
`samples/simple-agent`.

## Data Scope

The project owner confirms that the prompts, expected answers, references and
rubrics in this sample are synthetic and provided solely for demonstration and
testing. They are not collected user/customer data, training data or released
model weights. Running the sample sends its synthetic inputs to the model
provider the operator configures; provider requests may incur charges.

Do not replace these fixtures with personal, customer or confidential data in
public contributions. A future real-data, training or model release requires
its own provenance, privacy and approval assessment.

## Evaluation Behavior

The dataset includes a `judge-rubric` case with `reference` and `rubric` fields.
Those fields are visible in the Studio Evaluation Dataset builder and are passed
to the sample `llm_judge` grader. The controller creates a judge `AgentJob` for
each completed target case, waits for the judge output, and records the parsed
`judge_score` metric alongside the built-in grader metrics.

The sample judge is the `hello-agent-judge` Agent. Its `llm_judge` grader uses
`parameters.agentRef` to select that Agent, `parameters.metric` to name the
emitted metric, and `parameters.passingScore` to define the score required when
the judge output does not include an explicit `passed` value. Judge output must
be JSON with a numeric `score` from 0 to 1 and may include `passed`, `reason`, or
`message`.

## Install

Install all samples:

```powershell
./make.ps1 samples-install
```

**Bash (Linux/macOS), with PowerShell 7 installed:**

```bash
pwsh -NoProfile -File ./make.ps1 samples-install
```

Install only this sample after its target agent exists:

```powershell
./make.ps1 samples-install SAMPLE=simple-agent
./make.ps1 samples-install SAMPLE=evaluation
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./make.ps1 samples-install SAMPLE=simple-agent
pwsh -NoProfile -File ./make.ps1 samples-install SAMPLE=evaluation
```

The full sample install path applies `simple-agent` before `evaluation`, so the target
agent and dataset are available when the EvaluationRun creates one target AgentJob per
dataset case.

## Inspect Results

Use Kubernetes resources directly:

The inspection commands work in either shell:

```sh
kubectl get evaluations.kognition.dev
kubectl get evaluationdatasets.kognition.dev
kubectl get evaluationruns.kognition.dev
kubectl get evaluationruns.kognition.dev -o yaml
```

Or open the dashboard:

```powershell
.\bin\kognitionctl.exe dashboard --mode kubernetes
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl dashboard --mode kubernetes
```

Open the `Quality` section, select `Evaluation Runs`, and use the `Results` tab
to inspect the score, metric pass counts, latency, run history, case outputs,
and first target output.
