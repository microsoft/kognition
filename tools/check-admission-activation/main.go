// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) > 0 && (arguments[0] == "prepare" || arguments[0] == "activate" || arguments[0] == "recover") {
		return runTransaction(arguments, stdout, stderr)
	}
	flags := flag.NewFlagSet("check-admission-activation", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "Trusted rendered bootstrap manifest")
	kubeconfig := flags.String("kubeconfig", "", "Explicit kubeconfig for the target cluster")
	drainOnly := flags.Bool("drain-only", false, "Read-only pre-bootstrap drain check; no admission resources required")
	policyBehavior := flags.Bool("policy-behavior", false, "Verify six live policies using temporary exact-object grants; emit JSON evidence, not activation approval")
	writersFrozen := flags.Bool("writers-frozen", false, "Acknowledge externally frozen writers for the duration of policy probes and cleanup")
	namespace := flags.String("namespace", "", "Installation namespace (drain-only)")
	controller := flags.String("controller-service-account", "", "Dedicated controller service account (drain-only)")
	deployment := flags.String("controller-deployment", "", "Controller Deployment name (drain-only)")
	workloads := flags.String("workload-namespaces", "", "Comma-separated dedicated workload namespaces (drain-only)")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *kubeconfig == "" || *policyBehavior != *writersFrozen || *drainOnly && (*policyBehavior || *manifestPath != "" || *namespace == "" || *controller == "" || *deployment == "" || *workloads == "") || !*drainOnly && (*manifestPath == "" || *namespace != "" || *controller != "" || *deployment != "" || *workloads != "") {
		_, _ = fmt.Fprintln(stderr, "explicit --kubeconfig is required; use --manifest for bootstrap verification OR --drain-only with --namespace, --controller-service-account, --controller-deployment and --workload-namespaces; --policy-behavior requires --manifest and --writers-frozen and cannot be used with --drain-only")
		return 2
	}
	report := policyReport{Started: time.Now().UTC(), SeparateGates: []string{"deployed-guard", "CNI", "stdio", "activation"}}
	err := func() error {
		var content []byte
		var manifest activation.Manifest
		var err error
		if *drainOnly {
			manifest = activation.Manifest{Namespace: *namespace, Controller: *controller, Deployment: *deployment, Namespaces: strings.Split(*workloads, ",")}
		} else {
			content, err = os.ReadFile(*manifestPath)
			if err != nil {
				return err
			}
			report.ManifestSHA256 = fmt.Sprintf("%x", sha256.Sum256(content))
			manifest, err = activation.Parse(bytes.NewReader(content))
			if err != nil {
				return err
			}
		}
		report.Controller = "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller
		config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			return err
		}
		report.Endpoint = config.Host
		config.Timeout = 15 * time.Second
		admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if *policyBehavior {
			cluster, err := kubernetes.NewForConfig(config)
			if err != nil {
				return err
			}
			version, err := cluster.Discovery().ServerVersion()
			if err != nil {
				return err
			}
			report.Version = version.GitVersion
			report.Probes, err = activation.VerifyPolicyBehavior(ctx, config, manifest)
			return err
		}
		if *drainOnly {
			if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Drain verified for %s/%s. Read-only snapshot; keep external writers frozen and recheck immediately before activation. Admission and activation are NOT verified.\n", manifest.Namespace, manifest.Controller)
			return err
		}
		if err := activation.Verify(ctx, admin, manifest); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Bootstrap state verified for %s/%s; manifest SHA256 %x. Controller remains inactive. CNI and stdio acceptance are separate required gates.\n", manifest.Namespace, manifest.Controller, sha256.Sum256(content))
		return err
	}()
	if *policyBehavior {
		err = writePolicyReport(stdout, report, err)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func writePolicyReport(output io.Writer, report policyReport, verificationErr error) error {
	report.Finished = time.Now().UTC()
	report.Passed = verificationErr == nil
	if verificationErr != nil {
		report.Error = verificationErr.Error()
	}
	return errors.Join(verificationErr, json.NewEncoder(output).Encode(report))
}

type policyReport struct {
	Started        time.Time                      `json:"started"`
	Finished       time.Time                      `json:"finished"`
	Endpoint       string                         `json:"endpoint"`
	Version        string                         `json:"version"`
	Controller     string                         `json:"controller"`
	ManifestSHA256 string                         `json:"manifestSHA256"`
	Passed         bool                           `json:"passed"`
	Probes         []activation.PolicyProbeResult `json:"probes"`
	Error          string                         `json:"error,omitempty"`
	SeparateGates  []string                       `json:"separateGates"`
}
