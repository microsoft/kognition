// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status [agent-name]",
	Short: "Show status overview of agents",
	Long:  `Display a tabular overview of all agents (or a specific agent) showing phase, replicas, model, and last-active time.`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(_ *cobra.Command, args []string) error {
	if len(args) == 1 {
		return showAgentStatus(args[0])
	}
	return showAllAgentStatus()
}

func showAgentStatus(name string) error {
	var body []byte
	var err error
	if k8sMode {
		body, err = k8sGet(namespace, "agents", name)
	} else {
		body, err = doGet(apiURLName(namespace, "agents", name))
	}
	if err != nil {
		return err
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(body)
	}

	var agent agentStatusInfo
	if err := json.Unmarshal(body, &agent); err != nil {
		return printOutput(body)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "NAME\tPHASE\tREPLICAS\tMODEL\tLAST-ACTIVE\n")
	printAgentRow(w, &agent)
	if err := w.Flush(); err != nil {
		return err
	}
	return nil
}

func showAllAgentStatus() error {
	var body []byte
	var err error
	if k8sMode {
		body, err = k8sList(namespace, "agents")
	} else {
		body, err = doGet(apiURL(namespace, "agents"))
	}
	if err != nil {
		return err
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(body)
	}

	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return printOutput(body)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "NAME\tPHASE\tREPLICAS\tMODEL\tLAST-ACTIVE\n")

	for _, item := range list.Items {
		var agent agentStatusInfo
		if err := json.Unmarshal(item, &agent); err != nil {
			continue
		}
		printAgentRow(w, &agent)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return nil
}

// agentStatusInfo holds the fields needed for the status table.
type agentStatusInfo struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Model struct {
			Name string `json:"name"`
		} `json:"model"`
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Replicas   int    `json:"replicas"`
		LastActive string `json:"lastActive"`
	} `json:"status"`
}

func printAgentRow(w *tabwriter.Writer, a *agentStatusInfo) {
	phase := a.Status.Phase
	if phase == "" {
		phase = "Unknown"
	}

	replicas := fmt.Sprintf("%d", a.Status.Replicas)
	if a.Spec.Replicas != nil {
		replicas = fmt.Sprintf("%d/%d", a.Status.Replicas, *a.Spec.Replicas)
	}

	model := a.Spec.Model.Name
	if model == "" {
		model = "<none>"
	}

	lastActive := a.Status.LastActive
	if lastActive == "" {
		lastActive = "<never>"
	}

	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		a.Metadata.Name, phase, replicas, model, lastActive)
}
