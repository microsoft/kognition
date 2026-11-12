// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"github.com/spf13/cobra"
)

var describeCmd = &cobra.Command{
	Use:   "describe <resource> <name>",
	Short: "Show detailed information about a resource",
	Args:  cobra.ExactArgs(2),
	RunE:  runDescribe,
}

func init() {
	rootCmd.AddCommand(describeCmd)
}

func runDescribe(_ *cobra.Command, args []string) error {
	resource := args[0]
	name := args[1]

	// Get the resource
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

	// For describe, always output in YAML for readability
	origFormat := outputFormat
	if outputFormat == "" {
		outputFormat = "yaml"
	}
	err = printOutput(body)
	outputFormat = origFormat
	return err
}
