// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runTransaction(arguments []string, stdout, stderr io.Writer) int {
	mode := arguments[0]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "Exact reviewed rendered bootstrap manifest")
	chartPath := flags.String("chart", "", "Exact reviewed chart archive")
	kubeconfig := flags.String("kubeconfig", "", "Explicit target kubeconfig")
	release := flags.String("release", "", "Reviewed Helm release name")
	authority := flags.String("authority-namespace", "", "Dedicated administrator-owned approval namespace")
	approval := flags.String("approval", "", "Immutable approval ConfigMap name")
	replicas := flags.Int64("replicas", 0, "Explicit authorized controller replica count")
	frozen := flags.Bool("writers-frozen", false, "Confirm external writers remain frozen throughout this operation")
	stopped := flags.Bool("executors-stopped", false, "Confirm every previous activation and recovery executor has terminated")
	dryRun := flags.Bool("dry-run", false, "Validate activation without consuming approval or mutating resources")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *manifestPath == "" || *chartPath == "" || *kubeconfig == "" || *release == "" || *authority == "" || *replicas < 1 || *replicas > math.MaxInt32 || !*frozen || (mode == "prepare" && (*approval != "" || *dryRun || *stopped)) || (mode != "prepare" && *approval == "") || (mode == "recover" && (!*stopped || *dryRun)) || (mode != "recover" && *stopped) {
		_, _ = fmt.Fprintln(stderr, "require --manifest, --chart, --kubeconfig, --release, --authority-namespace, --replicas and --writers-frozen; activate/recover require --approval; recover additionally requires --executors-stopped and forbids --dry-run")
		return 2
	}
	var result interface{}
	err := func() error {
		content, err := os.ReadFile(*manifestPath)
		if err != nil {
			return err
		}
		candidate, err := activation.ParseCandidate(content, *release)
		if err != nil {
			return err
		}
		chartDigest, err := fileDigest(*chartPath)
		if err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		toolDigest, err := fileDigest(executable)
		if err != nil {
			return err
		}
		config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			return err
		}
		config.Timeout = 15 * time.Second
		config.QPS = 50
		config.Burst = 100
		cluster, err := kubernetes.NewForConfig(config)
		if err != nil {
			return err
		}
		version, err := cluster.Discovery().ServerVersion()
		if err != nil {
			return err
		}
		admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), candidate.TransactionTimeout()+time.Minute)
		defer cancel()
		options := activation.ActivationOptions{Approval: client.ObjectKey{Namespace: *authority, Name: *approval}, Endpoint: config.Host, ServerVersion: version.GitVersion, ChartSHA256: chartDigest, ToolVersion: "sha256:" + toolDigest, Replicas: int32(*replicas), WritersFrozen: *frozen, DryRun: *dryRun}
		switch mode {
		case "prepare":
			result, err = candidate.Snapshot(ctx, admin, options.Endpoint, options.ServerVersion, options.ChartSHA256, options.ToolVersion, *authority, options.Replicas)
		case "activate":
			result, err = candidate.Activate(ctx, admin, config, options)
		case "recover":
			result, err = candidate.Recover(ctx, admin, options, *stopped)
		}
		return err
	}()
	report := struct {
		Mode   string      `json:"mode"`
		Passed bool        `json:"passed"`
		Result interface{} `json:"result"`
		Error  string      `json:"error,omitempty"`
	}{Mode: mode, Passed: err == nil, Result: result}
	if err != nil {
		report.Error = err.Error()
	}
	err = errors.Join(err, json.NewEncoder(stdout).Encode(report))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func fileDigest(path string) (digest string, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
