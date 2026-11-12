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

var getCmd = &cobra.Command{
	Use:   "get <resource> [name]",
	Short: "Get one or many resources",
	Long:  `Display one or many resources. If a name is specified, show that specific resource. Otherwise, list all resources of that type.`,
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runGet,
}

func init() {
	rootCmd.AddCommand(getCmd)
}

func runGet(_ *cobra.Command, args []string) error {
	resource := args[0]

	if len(args) == 2 {
		// Get specific resource
		name := args[1]
		var body []byte
		var err error
		if k8sMode {
			body, err = k8sGet(namespace, resource, name)
		} else {
			body, err = doGet(apiURLName(namespace, resource, name))
		}
		if err != nil {
			return err
		}
		return printOutput(body)
	}

	// List resources
	var body []byte
	var err error
	if k8sMode {
		body, err = k8sList(namespace, resource)
	} else {
		body, err = doGet(apiURL(namespace, resource))
	}
	if err != nil {
		return err
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(body)
	}

	// Table output (default for list)
	return printTable(resource, body)
}

func printTable(_ string, data []byte) error {
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		// Maybe it's not a list, just print it
		return printOutput(data)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "NAMESPACE\tNAME\tKIND\tAGE\n")

	for _, item := range list.Items {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		ns, _ := meta["namespace"].(string)
		name, _ := meta["name"].(string)
		kind, _ := obj["kind"].(string)
		createdAt, _ := meta["creationTimestamp"].(string)
		if createdAt == "" {
			createdAt, _ = meta["createdAt"].(string)
		}
		if createdAt == "" {
			createdAt, _ = meta["created_at"].(string)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ns, name, kind, shortenAge(createdAt))
	}
	return w.Flush()
}

func shortenAge(ts string) string {
	if ts == "" {
		return "<unknown>"
	}
	// Just show the timestamp for now; could compute relative time
	return strings.Split(ts, "T")[0]
}
