// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Inspect workflow runs",
	Long:  `Commands for inspecting WorkflowRun resources and their step execution timeline.`,
}

var workflowLogsCmd = &cobra.Command{
	Use:   "logs <run-name>",
	Short: "Show timeline of step executions for a workflow run",
	Long: `Display the per-step timeline of a WorkflowRun, including phase, attempts,
start/finish timestamps, and duration. Step rows are derived from the child
AgentJobs whose ownerReferences point at the WorkflowRun.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowLogs,
}

func init() {
	workflowCmd.AddCommand(workflowLogsCmd)
	rootCmd.AddCommand(workflowCmd)
}

type workflowRunDoc struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Status struct {
		Phase        string `json:"phase"`
		StepStatuses []struct {
			Name        string     `json:"name"`
			Phase       string     `json:"phase"`
			Attempts    int        `json:"attempts"`
			StartedAt   *time.Time `json:"startedAt,omitempty"`
			NextRetryAt *time.Time `json:"nextRetryAt,omitempty"`
		} `json:"stepStatuses"`
	} `json:"status"`
}

type agentJobDoc struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		Annotations     map[string]string `json:"annotations"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		Phase          string     `json:"phase"`
		CompletionTime *time.Time `json:"completionTime,omitempty"`
	} `json:"status"`
}

func runWorkflowLogs(_ *cobra.Command, args []string) error {
	runName := args[0]

	// Fetch the WorkflowRun.
	var runBody []byte
	var err error
	if k8sMode {
		runBody, err = k8sGet(namespace, "workflowruns", runName)
	} else {
		runBody, err = doGet(apiURLName(namespace, "workflowruns", runName))
	}
	if err != nil {
		return fmt.Errorf("get workflowrun %q: %w", runName, err)
	}

	var run workflowRunDoc
	if err := json.Unmarshal(runBody, &run); err != nil {
		return fmt.Errorf("parse workflowrun: %w", err)
	}

	// List AgentJobs and filter by ownerRef to this WorkflowRun.
	var jobsBody []byte
	if k8sMode {
		jobsBody, err = k8sList(namespace, "agentjobs")
	} else {
		jobsBody, err = doGet(apiURL(namespace, "agentjobs"))
	}
	if err != nil {
		return fmt.Errorf("list agentjobs: %w", err)
	}

	var jobList struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(jobsBody, &jobList); err != nil {
		return fmt.Errorf("parse agentjob list: %w", err)
	}

	jobsByStep := map[string]agentJobDoc{}
	for _, raw := range jobList.Items {
		var job agentJobDoc
		if err := json.Unmarshal(raw, &job); err != nil {
			continue
		}
		owned := false
		for _, owner := range job.Metadata.OwnerReferences {
			if strings.EqualFold(owner.Kind, "WorkflowRun") && owner.Name == runName {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		stepName := job.Metadata.Annotations["kognition.dev/step-name"]
		if stepName == "" {
			continue
		}
		// Key by step name + iteration so multi-iteration steps render distinct rows.
		iter := job.Metadata.Annotations["kognition.dev/step-iteration"]
		key := stepName
		if iter != "" {
			key = stepName + "#" + iter
		}
		// Keep the latest completion (or any) per key.
		if existing, ok := jobsByStep[key]; ok {
			if existing.Status.CompletionTime != nil && job.Status.CompletionTime != nil &&
				job.Status.CompletionTime.Before(*existing.Status.CompletionTime) {
				continue
			}
		}
		jobsByStep[key] = job
	}

	if outputFormat == "json" {
		out, _ := json.MarshalIndent(map[string]any{
			"run":        run,
			"jobsByStep": jobsByStep,
		}, "", "  ")
		return printOutput(out)
	}

	// Sort step rows by start time, then by name.
	rows := make([]struct {
		Name     string
		Phase    string
		Attempts int
		Started  *time.Time
		Finished *time.Time
	}, 0, len(run.Status.StepStatuses))
	for _, ss := range run.Status.StepStatuses {
		row := struct {
			Name     string
			Phase    string
			Attempts int
			Started  *time.Time
			Finished *time.Time
		}{Name: ss.Name, Phase: ss.Phase, Attempts: ss.Attempts, Started: ss.StartedAt}
		if job, ok := jobsByStep[ss.Name]; ok {
			row.Finished = job.Status.CompletionTime
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		switch {
		case rows[i].Started == nil && rows[j].Started == nil:
			return rows[i].Name < rows[j].Name
		case rows[i].Started == nil:
			return false
		case rows[j].Started == nil:
			return true
		default:
			return rows[i].Started.Before(*rows[j].Started)
		}
	})

	_, _ = fmt.Fprintf(os.Stdout, "WorkflowRun: %s/%s   Phase: %s\n\n", run.Metadata.Namespace, run.Metadata.Name, run.Status.Phase)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "STEP\tPHASE\tATTEMPTS\tSTARTED\tFINISHED\tDURATION")
	for _, r := range rows {
		started := "-"
		finished := "-"
		duration := "-"
		if r.Started != nil {
			started = r.Started.UTC().Format(time.RFC3339)
		}
		if r.Finished != nil {
			finished = r.Finished.UTC().Format(time.RFC3339)
		}
		if r.Started != nil && r.Finished != nil {
			d := r.Finished.Sub(*r.Started)
			if d >= 0 {
				duration = d.Truncate(time.Millisecond).String()
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", r.Name, r.Phase, r.Attempts, started, finished, duration)
	}
	return w.Flush()
}
