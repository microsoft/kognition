// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-intent-gateway-client submits a goal through IntentGateway gRPC.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

func main() {
	address := flag.String("address", "127.0.0.1:18081", "gRPC server address")
	namespace := flag.String("namespace", "", "gateway namespace")
	gatewayName := flag.String("gateway", "", "IntentGateway name")
	goal := flag.String("goal", "", "goal text")
	apiKey := flag.String("api-key", "", "optional gateway API key")
	bearerToken := flag.String("bearer-token", "", "optional bearer token")
	flag.Parse()

	if *namespace == "" || *gatewayName == "" || *goal == "" {
		fmt.Fprintln(os.Stderr, "namespace, gateway, and goal are required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, *address, grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials())) //nolint:staticcheck // WithBlock startup semantics require the grpc 1.x dialing API.
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = connection.Close() }()
	if *bearerToken != "" {
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+*bearerToken))
	}

	response, err := intentgatewayv1alpha1.NewIntentGatewayClient(connection).SubmitGoal(ctx, &intentgatewayv1alpha1.SubmitGoalRequest{
		Namespace:   *namespace,
		GatewayName: *gatewayName,
		Goal:        *goal,
		ApiKey:      *apiKey,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"solutionName":      response.GetSolutionName(),
		"solutionNamespace": response.GetSolutionNamespace(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
