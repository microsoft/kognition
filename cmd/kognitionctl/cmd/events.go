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

var eventsCmd = &cobra.Command{
	Use:   "events <resource> <name>",
	Short: "Show events for a resource",
	Long:  `Display the event history for a specific resource.`,
	Args:  cobra.ExactArgs(2),
	RunE:  runEvents,
}

func init() {
	rootCmd.AddCommand(eventsCmd)
}

func runEvents(_ *cobra.Command, args []string) error {
	resource := args[0]
	name := args[1]

	url := fmt.Sprintf("%s/events", apiURLName(namespace, resource, name))
	body, err := doGet(url)
	if err != nil {
		return err
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(body)
	}

	// Table output
	var events struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		// Maybe raw array
		var arr []json.RawMessage
		if err2 := json.Unmarshal(body, &arr); err2 != nil {
			return printOutput(body)
		}
		events.Items = arr
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "TYPE\tREASON\tMESSAGE\tTIMESTAMP\n")

	for _, item := range events.Items {
		var ev map[string]any
		if err := json.Unmarshal(item, &ev); err != nil {
			continue
		}
		evType, _ := ev["type"].(string)
		reason, _ := ev["reason"].(string)
		message, _ := ev["message"].(string)
		ts, _ := ev["timestamp"].(string)
		if ts == "" {
			ts, _ = ev["createdAt"].(string)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", evType, reason, message, ts)
	}
	return w.Flush()
}
