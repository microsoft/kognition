// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var logsFollow bool
var logsInstance string

var logsCmd = &cobra.Command{
	Use:   "logs <agent-name>",
	Short: "Show output logs from agent instances",
	Long: `Display the output and status of agent instances belonging to a specific agent.

Shows session output from each running or completed instance. Use --instance to
target a specific instance by name.`,
	Args: cobra.ExactArgs(1),
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow output (poll for updates)")
	logsCmd.Flags().StringVarP(&logsInstance, "instance", "i", "", "Show logs for a specific instance name")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(_ *cobra.Command, args []string) error {
	agentName := args[0]

	if logsInstance != "" {
		return showInstanceLogs(logsInstance)
	}

	// List all instances for this agent
	var body []byte
	var err error
	if k8sMode {
		body, err = k8sList(namespace, "agentinstances")
	} else {
		body, err = doGet(apiURL(namespace, "agentinstances"))
	}
	if err != nil {
		return err
	}

	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("parse instance list: %w", err)
	}

	// Filter instances belonging to this agent
	type instanceInfo struct {
		Metadata struct {
			Name              string `json:"name"`
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			AgentRef struct {
				Name string `json:"name"`
			} `json:"agentRef"`
			Revision int64 `json:"revision"`
		} `json:"spec"`
		Status struct {
			Phase     string `json:"phase"`
			SessionID string `json:"sessionID"`
			Output    string `json:"output"`
			StartTime string `json:"startTime"`
		} `json:"status"`
	}

	var instances []instanceInfo
	for _, item := range list.Items {
		var inst instanceInfo
		if err := json.Unmarshal(item, &inst); err != nil {
			continue
		}
		if strings.EqualFold(inst.Spec.AgentRef.Name, agentName) {
			instances = append(instances, inst)
		}
	}

	if len(instances) == 0 {
		fmt.Fprintf(os.Stderr, "No instances found for agent %q in namespace %q\n", agentName, namespace)
		return nil
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		out, _ := json.Marshal(instances)
		return printOutput(out)
	}

	// Print summary table + output
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "INSTANCE\tPHASE\tREVISION\tSESSION\tSTART\n")
	for _, inst := range instances {
		session := inst.Status.SessionID
		if len(session) > 8 {
			session = session[:8]
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			inst.Metadata.Name,
			inst.Status.Phase,
			inst.Spec.Revision,
			session,
			inst.Status.StartTime,
		)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Print output from each instance that has it
	for _, inst := range instances {
		if inst.Status.Output != "" {
			fmt.Printf("\n--- %s ---\n%s\n", inst.Metadata.Name, inst.Status.Output)
		}
	}

	// Also fetch and show events for the agent
	eventsURL := fmt.Sprintf("%s/events", apiURLName(namespace, "agents", agentName))
	evBody, err := doGet(eventsURL)
	if err == nil && len(evBody) > 2 { // not empty "[]" or "{}"
		fmt.Printf("\n--- Events ---\n")
		var events []struct {
			Type      string `json:"type"`
			Reason    string `json:"reason"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(evBody, &events) == nil && len(events) > 0 {
			for _, ev := range events {
				fmt.Printf("[%s] %s: %s (%s)\n", ev.Timestamp, ev.Reason, ev.Message, ev.Type)
			}
		}
	}

	return nil
}

func showInstanceLogs(name string) error {
	var body []byte
	var err error
	if k8sMode {
		body, err = k8sGet(namespace, "agentinstances", name)
	} else {
		body, err = doGet(apiURLName(namespace, "agentinstances", name))
	}
	if err != nil {
		return err
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(body)
	}

	var inst struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			AgentRef struct {
				Name string `json:"name"`
			} `json:"agentRef"`
			Revision int64 `json:"revision"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			SessionID  string `json:"sessionID"`
			Output     string `json:"output"`
			StartTime  string `json:"startTime"`
			TokenUsage *struct {
				PromptTokens     int64 `json:"promptTokens"`
				CompletionTokens int64 `json:"completionTokens"`
				TotalTokens      int64 `json:"totalTokens"`
			} `json:"tokenUsage"`
			ToolCalls int32 `json:"toolCalls"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &inst); err != nil {
		return printOutput(body)
	}

	fmt.Printf("Instance:  %s\n", inst.Metadata.Name)
	fmt.Printf("Agent:     %s\n", inst.Spec.AgentRef.Name)
	fmt.Printf("Revision:  %d\n", inst.Spec.Revision)
	fmt.Printf("Phase:     %s\n", inst.Status.Phase)
	fmt.Printf("Session:   %s\n", inst.Status.SessionID)
	fmt.Printf("Started:   %s\n", inst.Status.StartTime)
	if inst.Status.TokenUsage != nil {
		fmt.Printf("Tokens:    %d prompt / %d completion / %d total\n",
			inst.Status.TokenUsage.PromptTokens,
			inst.Status.TokenUsage.CompletionTokens,
			inst.Status.TokenUsage.TotalTokens)
	}
	if inst.Status.ToolCalls > 0 {
		fmt.Printf("ToolCalls: %d\n", inst.Status.ToolCalls)
	}
	if inst.Status.Output != "" {
		fmt.Printf("\n--- Output ---\n%s\n", inst.Status.Output)
	} else {
		fmt.Printf("\n(no output yet)\n")
	}

	return nil
}
