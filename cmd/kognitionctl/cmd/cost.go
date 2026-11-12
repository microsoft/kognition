// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/observability"
)

var (
	costStart            string
	costEnd              string
	costAgentIdentity    string
	costSolution         string
	costSolutionRevision string
	costGroupBy          string
	costLimit            int
	costOffset           int
	costNamespace        string
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Query normalized cost aggregations",
	Long:  "Query normalized micro-USD cost records for the selected Workspace.",
	Args:  cobra.NoArgs,
	RunE:  runCost,
}

func init() {
	rootCmd.AddCommand(costCmd)
	costCmd.Flags().StringVar(&costStart, "start", "", "Inclusive RFC3339 start time")
	costCmd.Flags().StringVar(&costEnd, "end", "", "Inclusive RFC3339 end time")
	costCmd.Flags().StringVar(&costAgentIdentity, "agent-identity", "", "Filter by AgentIdentity")
	costCmd.Flags().StringVar(&costSolution, "solution", "", "Filter by Solution")
	costCmd.Flags().StringVar(&costSolutionRevision, "solution-revision", "", "Filter by Solution revision")
	costCmd.Flags().StringVar(&costGroupBy, "group-by", "", "Comma-separated grouping: workspace,agent,solution,model,operation_type")
	costCmd.Flags().IntVar(&costLimit, "limit", 0, "Maximum aggregation rows")
	costCmd.Flags().IntVar(&costOffset, "offset", 0, "Aggregation row offset")
	costCmd.Flags().StringVar(&costNamespace, "cost-namespace", "", "Kubernetes namespace containing immutable cost ConfigMaps (defaults to --kube-namespace)")
}

func runCost(_ *cobra.Command, _ []string) error {
	query, err := cliCostQuery()
	if err != nil {
		return err
	}
	var result *observability.CostQueryResult
	if k8sMode {
		result, err = queryKubernetesCosts(query)
	} else {
		result, err = queryStandaloneCosts(query)
	}
	if err != nil {
		return err
	}
	return printCostResult(result)
}

func cliCostQuery() (observability.CostQuery, error) {
	query := observability.CostQuery{Workspace: namespace, AgentIdentity: costAgentIdentity, Solution: costSolution, Limit: costLimit, Offset: costOffset}
	if query.Workspace == "" {
		return observability.CostQuery{}, fmt.Errorf("namespace is required")
	}
	if query.Limit < 0 || query.Offset < 0 {
		return observability.CostQuery{}, fmt.Errorf("limit and offset must be non-negative")
	}
	var err error
	if query.Start, err = parseCLICostTime(costStart); err != nil {
		return observability.CostQuery{}, fmt.Errorf("invalid start: %w", err)
	}
	if query.End, err = parseCLICostTime(costEnd); err != nil {
		return observability.CostQuery{}, fmt.Errorf("invalid end: %w", err)
	}
	if !query.Start.IsZero() && !query.End.IsZero() && query.End.Before(query.Start) {
		return observability.CostQuery{}, fmt.Errorf("end must not be before start")
	}
	if costSolutionRevision != "" {
		revision, parseErr := strconv.ParseInt(costSolutionRevision, 10, 64)
		if parseErr != nil || revision < 0 {
			return observability.CostQuery{}, fmt.Errorf("solution-revision must be a non-negative integer")
		}
		query.SolutionRev = &revision
	}
	if costGroupBy != "" {
		for _, value := range strings.Split(costGroupBy, ",") {
			group := observability.CostGroupBy(strings.TrimSpace(value))
			switch group {
			case observability.GroupByWorkspace, observability.GroupByAgent, observability.GroupBySolution, observability.GroupByModel, observability.GroupByOperationType:
				query.GroupBy = append(query.GroupBy, group)
			default:
				return observability.CostQuery{}, fmt.Errorf("unsupported group-by value %q", value)
			}
		}
	}
	return query, nil
}

func parseCLICostTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func queryStandaloneCosts(query observability.CostQuery) (*observability.CostQueryResult, error) {
	values := url.Values{}
	if !query.Start.IsZero() {
		values.Set("start", query.Start.UTC().Format(time.RFC3339))
	}
	if !query.End.IsZero() {
		values.Set("end", query.End.UTC().Format(time.RFC3339))
	}
	if query.AgentIdentity != "" {
		values.Set("agentIdentity", query.AgentIdentity)
	}
	if query.Solution != "" {
		values.Set("solution", query.Solution)
	}
	if query.SolutionRev != nil {
		values.Set("solutionRevision", strconv.FormatInt(*query.SolutionRev, 10))
	}
	if len(query.GroupBy) > 0 {
		groups := make([]string, len(query.GroupBy))
		for index := range query.GroupBy {
			groups[index] = string(query.GroupBy[index])
		}
		values.Set("groupBy", strings.Join(groups, ","))
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Offset > 0 {
		values.Set("offset", strconv.Itoa(query.Offset))
	}
	body, err := doGet(fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/costrecords?%s", serverURL, query.Workspace, values.Encode()))
	if err != nil {
		return nil, err
	}
	var result observability.CostQueryResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode cost aggregation response: %w", err)
	}
	return &result, nil
}

func queryKubernetesCosts(query observability.CostQuery) (*observability.CostQueryResult, error) {
	if k8sRESTConfig == nil {
		return nil, fmt.Errorf("Kubernetes client is not configured")
	}
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core Kubernetes scheme: %w", err)
	}
	kubernetesClient, err := ctrlclient.New(k8sRESTConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes cost client: %w", err)
	}
	controlNamespace := costNamespace
	if controlNamespace == "" {
		controlNamespace = kubeNamespace
	}
	store, err := k8scost.NewConfigMapStore(kubernetesClient, controlNamespace)
	if err != nil {
		return nil, err
	}
	return store.QueryCosts(context.Background(), query)
}

func printCostResult(result *observability.CostQueryResult) error {
	if result == nil {
		return fmt.Errorf("cost aggregation returned no result")
	}
	if outputFormat == "json" || outputFormat == "yaml" {
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return printOutput(body)
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "WORKSPACE\tAGENT\tSOLUTION\tREVISION\tMODEL\tOPERATION\tCOST (micro-USD)\tOPERATIONS\tPROMPT TOKENS\tCOMPLETION TOKENS")
	for _, aggregation := range result.Aggregations {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t%d\t%d\t%d\n", aggregation.Workspace, aggregation.AgentIdentity, aggregation.Solution, aggregation.SolutionRev, aggregation.ModelName, aggregation.OperationType, aggregation.TotalCostUnits, aggregation.TotalOperations, aggregation.TotalPromptTokens, aggregation.TotalCompletionTokens)
	}
	return writer.Flush()
}
