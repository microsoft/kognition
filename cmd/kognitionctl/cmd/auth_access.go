// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kognition-ai/kognition/internal/accesscheck"
)

// ErrAccessDenied is returned by `auth can-i` when the answer is no. The caller
// exits non-zero without printing an error message, like `kubectl auth can-i`.
var ErrAccessDenied = errors.New("access denied")

var authSubresource string

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Check what you may do with Kognition resources (Kubernetes mode)",
	Long: `Ask the cluster what the current Kubernetes identity may do with Kognition
resources. The answers come from the cluster's own authorizers and are advisory:
every real request is authorized again by the server.`,
}

var authCanICmd = &cobra.Command{
	Use:   "can-i <verb> <kind>[/<name>]",
	Short: "Check whether you may perform an action",
	Long: `Check whether the current identity may perform a verb on a Kognition kind,
optionally on one named object. Custom Kognition verbs are supported:
use (workflows), invoke (agents), approve and reject (runtimeapprovals,
--subresource decision) and reconcile (runtimeapprovals, --subresource reconcile).

Prints "yes" or "no" and exits 1 for no.`,
	Example: `  kognitionctl auth can-i create workflowruns -n team-a
  kognitionctl auth can-i use workflow/nightly-report -n team-a
  kognitionctl auth can-i approve runtimeapproval/ra-1 --subresource decision -n team-a`,
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runAuthCanI,
}

var authListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your Kognition permissions in a namespace",
	Long: `List what the current identity may do with Kognition resources in a namespace.
The list is advisory and limited to what the cluster can report: cluster-scoped kinds
(the classes) are not included, and some authorizers cannot enumerate their rules.
Use "auth can-i" to check a specific action.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runAuthList,
}

func init() {
	authCanICmd.Flags().StringVar(&authSubresource, "subresource", "", "Subresource to check, for example decision")
	authCmd.AddCommand(authCanICmd, authListCmd)
	rootCmd.AddCommand(authCmd)
}

func requireKubernetesAccessChecks() error {
	if !k8sMode || k8sCoreClient == nil {
		return errors.New("access checks use the cluster's authorization and are only available in Kubernetes mode (standalone mode has its own role model); run with --mode kubernetes")
	}
	return nil
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// writeStructured prints a value as JSON or YAML to the command's output.
func writeStructured(out io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if outputFormat == "json" {
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, indented.String())
		return err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return err
	}
	encoded, err := yaml.Marshal(generic)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(out, string(encoded))
	return err
}

func runAuthCanI(cmd *cobra.Command, args []string) error {
	if err := requireKubernetesAccessChecks(); err != nil {
		return err
	}
	kind, name, _ := strings.Cut(args[1], "/")
	if strings.Contains(name, "/") {
		return fmt.Errorf("%q: expected <kind> or <kind>/<name>", args[1])
	}
	decision, err := accesscheck.CanI(commandContext(cmd), k8sCoreClient.AuthorizationV1(), accesscheck.Request{
		Verb: args[0], Kind: kind, Name: name, Subresource: authSubresource, Namespace: namespace,
	})
	if err != nil {
		return err
	}
	if outputFormat == "json" || outputFormat == "yaml" {
		if err := writeStructured(cmd.OutOrStdout(), map[string]any{"allowed": decision.Allowed, "reason": decision.Reason}); err != nil {
			return err
		}
	} else if decision.Allowed {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "yes"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(cmd.OutOrStdout(), "no"); err != nil {
		return err
	}
	if !decision.Allowed {
		return ErrAccessDenied
	}
	return nil
}

// textWriter keeps the first write error so a run of prints needs one check.
type textWriter struct {
	w   io.Writer
	err error
}

func (t *textWriter) printf(format string, args ...any) {
	if t.err == nil {
		_, t.err = fmt.Fprintf(t.w, format, args...)
	}
}

func runAuthList(cmd *cobra.Command, _ []string) error {
	if err := requireKubernetesAccessChecks(); err != nil {
		return err
	}
	set, err := accesscheck.Rules(commandContext(cmd), k8sCoreClient.AuthorizationV1(), namespace)
	if err != nil {
		return err
	}
	if outputFormat == "json" || outputFormat == "yaml" {
		return writeStructured(cmd.OutOrStdout(), set)
	}

	out := cmd.OutOrStdout()
	table := &textWriter{w: tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)}
	table.printf("RESOURCE\tVERBS\tRESOURCE NAMES\n")
	for _, row := range set.Rows {
		names := "*"
		if len(row.ResourceNames) > 0 {
			names = strings.Join(row.ResourceNames, ",")
		}
		resource := row.Resource
		if row.Wildcard {
			resource += " (wildcard)"
		}
		table.printf("%s\t%s\t%s\n", resource, strings.Join(row.Verbs, ","), names)
	}
	if table.err != nil {
		return table.err
	}
	if err := table.w.(*tabwriter.Writer).Flush(); err != nil {
		return err
	}

	notes := &textWriter{w: out}
	if len(set.Rows) == 0 {
		notes.printf("No Kognition permissions found in namespace %q.\n", set.Namespace)
	}
	notes.printf("\nNamespace %q. This list is advisory; the server authorizes every request.\n", set.Namespace)
	notes.printf("Cluster-scoped kinds (the classes) are not included; use \"auth can-i\" for those.\n")
	if set.Incomplete || set.EvaluationError != "" {
		reason := set.EvaluationError
		if reason == "" {
			reason = "an authorizer could not enumerate its rules"
		}
		notes.printf("Warning: the list is incomplete (%s). Absence from the list does not mean denial.\n", reason)
	}
	return notes.err
}
