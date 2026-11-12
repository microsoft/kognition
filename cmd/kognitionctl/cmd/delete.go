// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:   "delete <resource> <name>",
	Short: "Delete a resource",
	Args:  cobra.ExactArgs(2),
	RunE:  runDelete,
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}

func runDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	resource := args[0]
	name := args[1]

	if k8sMode {
		if err := k8sDelete(namespace, resource, name); err != nil {
			return err
		}
		fmt.Printf("%s \"%s\" deleted\n", singularize(resource), name)
		return nil
	}

	url := apiURLName(namespace, resource, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server error (%d): %s", resp.StatusCode, string(body))
	}

	fmt.Printf("%s \"%s\" deleted\n", singularize(resource), name)
	return nil
}
