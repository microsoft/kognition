// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func helmEnvironment(environment []string, path, digest string) []string {
	result := make([]string, 0, len(environment)+3)
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		name = strings.ToUpper(name)
		if strings.HasPrefix(name, "HELM_KUBE") || strings.HasPrefix(name, "KOGNITION_INSTALL_PLAN") || name == "HELM_DRIVER" {
			continue
		}
		result = append(result, variable)
	}
	return append(result, "HELM_DRIVER=secret", "KOGNITION_INSTALL_PLAN="+path, "KOGNITION_INSTALL_PLAN_SHA256="+digest)
}

func run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		_, _ = fmt.Fprintln(stderr, "use fresh --help for a new installation; use bootstrap or recover --plan <reviewed.json> --writers-frozen for an existing installation")
		return 2
	}
	if arguments[0] == "fresh" {
		return runFresh(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "fresh-recover" {
		return runFreshRecovery(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "post-render" || arguments[0] == "fresh-post-render" {
		if err := postRenderMode(stdin, stdout, arguments[0] == "fresh-post-render"); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	flags := flag.NewFlagSet(arguments[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	planPath := flags.String("plan", "", "Reviewed version 1 installation plan")
	frozen := flags.Bool("writers-frozen", false, "Confirm all release, admission and workload writers remain frozen")
	settled := flags.Bool("requests-settled", false, "Confirm all previous executors and their outstanding API requests have completed")
	stopped := flags.Bool("executors-stopped", false, "Confirm every previous installation executor has terminated")
	claimUID := flags.String("claim-uid", "", "Exact administrator-reviewed installation claim UID for recovery")
	helmUID := flags.String("helm-record-uid", "", "Exact reviewed pending Helm Secret UID to settle during recovery")
	helmDigest := flags.String("helm-record-sha256", "", "Exact reviewed pending Helm Secret ObjectDigest to settle during recovery")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *planPath == "" || !*frozen || (arguments[0] != "bootstrap" && arguments[0] != "recover") || (arguments[0] == "recover" && (!*settled || !*stopped || *claimUID == "")) || (arguments[0] == "bootstrap" && (*settled || *stopped || *claimUID != "")) {
		_, _ = fmt.Fprintln(stderr, "bootstrap/recover require --plan and --writers-frozen; recover also requires --executors-stopped, --requests-settled and --claim-uid")
		return 2
	}
	decoded, digestErr := hex.DecodeString(*helmDigest)
	if (*helmUID == "") != (*helmDigest == "") || (*helmDigest != "" && (arguments[0] != "recover" || digestErr != nil || len(decoded) != sha256.Size || strings.ToLower(*helmDigest) != *helmDigest)) {
		_, _ = fmt.Fprintln(stderr, "Helm settlement requires recover with paired --helm-record-uid and lowercase SHA256 --helm-record-sha256")
		return 2
	}
	err := execute(arguments[0], *planPath, *claimUID, *helmUID, *helmDigest, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func postRender(stdin io.Reader, stdout io.Writer) error {
	return postRenderMode(stdin, stdout, false)
}

func postRenderMode(stdin io.Reader, stdout io.Writer, fresh bool) error {
	path := os.Getenv("KOGNITION_INSTALL_PLAN")
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(content)) != os.Getenv("KOGNITION_INSTALL_PLAN_SHA256") {
		return errors.New("installation plan changed during Helm execution")
	}
	plan, err := installation.ParsePlan(content, path)
	if err != nil {
		return err
	}
	if fresh {
		if err := installation.ValidateFreshPlan(plan); err != nil {
			return err
		}
	}
	expected, err := installation.ReadArtifact(plan.Manifest)
	if err != nil {
		return err
	}
	actual, err := io.ReadAll(io.LimitReader(stdin, 32*1024*1024+1))
	if err != nil {
		return err
	}
	if len(actual) > 32*1024*1024 {
		return errors.New("render exceeds 32 MiB")
	}
	if fresh {
		actual, err = installation.FreshRender(expected, actual, plan.Release)
	} else {
		_, err = installation.ValidateRender(expected, actual, plan.Release)
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(actual)
	return err
}

func execute(mode, path, claimUID, helmUID, helmDigest string, stdout, stderr io.Writer) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	planContent, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	planDigest := fmt.Sprintf("%x", sha256.Sum256(planContent))
	plan, err := installation.ParsePlan(planContent, path)
	if err != nil {
		return err
	}
	for _, artifact := range []installation.Artifact{plan.Chart, plan.Values} {
		if _, err := installation.ReadArtifact(artifact); err != nil {
			return err
		}
	}
	expected, err := installation.ReadArtifact(plan.Manifest)
	if err != nil {
		return err
	}
	manifest, err := installation.ValidateRender(expected, expected, plan.Release)
	if err != nil {
		return err
	}
	freshSnapshot := mode == "fresh" || mode == "fresh-recover"
	if mode == "recover" && installation.ValidateFreshPlan(plan) == nil {
		freshSnapshot = true
	}
	snapshot := installation.Snapshot
	if freshSnapshot {
		snapshot = installation.SnapshotFresh
	}
	plan, snapshotPath, cleanup, err := snapshot(plan)
	if err != nil {
		return err
	}
	defer cleanup()
	snapshotContent, err := os.ReadFile(snapshotPath)
	if err != nil {
		return err
	}
	snapshotDigest := fmt.Sprintf("%x", sha256.Sum256(snapshotContent))
	config, err := clientcmd.BuildConfigFromFlags("", plan.Kubeconfig)
	if err != nil {
		return err
	}
	if config.Host != plan.Endpoint || config.TLSClientConfig.Insecure || config.Impersonate.UserName != "" || config.Impersonate.UID != "" || len(config.Impersonate.Groups) != 0 || len(config.Impersonate.Extra) != 0 {
		return errors.New("kubeconfig endpoint mismatch, insecure TLS or impersonation is unsupported")
	}
	config.Timeout = 15 * time.Second
	if freshSnapshot {
		config.QPS, config.Burst = 50, 100
	}
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return err
	}
	discover, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return err
	}
	timeout := 8 * time.Minute
	if mode == "fresh" {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	operations := &installation.Kubernetes{Plan: plan, Manifest: manifest, Admin: admin, Discovery: discover}
	if mode == "fresh-recover" {
		return recoverFresh(ctx, operations, config, path, planDigest, claimUID, stdout, stderr)
	}
	if mode == "recover" {
		if err := operations.VerifyTarget(ctx); err != nil {
			return err
		}
		record, err := installation.OpenJournal(ctx, admin, plan, planDigest, claimUID)
		if apierrors.IsNotFound(err) {
			if helmUID != "" {
				return errors.New("Helm settlement requires the active installation claim; retired or absent claims cannot authorize it")
			}
			if err := installation.ConfirmRetired(ctx, admin, plan, planDigest, claimUID); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "Claim absence and matching immutable retirement intent confirmed. No mutation or activation was attempted; keep writers frozen and review current state before any new plan.")
			return err
		}
		if err != nil {
			return err
		}
		operations.Journal = record
		if helmUID != "" && !record.AdmissionStarted() {
			return errors.New("Helm settlement requires an admission-stage installation claim")
		}
		outcome := "aborted-before-mutation"
		if !record.BeforeMutation() {
			if !record.AdmissionStarted() {
				if err := operations.RecoverCleanup(ctx, record); err != nil {
					return err
				}
			}
			if err := operations.VerifyDrained(ctx); err != nil {
				return fmt.Errorf("recovery cannot release claim: keep writers frozen, reconcile exact reviewed resources, and never roll back privileges: %w", err)
			}
			if helmUID != "" {
				if err := operations.SettleHelm(ctx, helmUID, helmDigest); err != nil {
					return err
				}
			}
			outcome = "drained-retry"
			if err := operations.VerifyBootstrapNow(ctx); err == nil {
				outcome = "verified-inactive"
			}
		}
		if err := record.Retire(outcome); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Recovery receipt retained: %s. Claim retired. Pre-admission cleanup may have resumed using exact durable intents; requested Helm storage settlement: %t. No rollback or activation was attempted. Keep writers frozen and review current state before any new plan.\n", outcome, helmUID != "")
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		return err
	}
	command := func(ctx context.Context, arguments ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, helmPath, arguments...)
		cmd.Env = helmEnvironment(os.Environ(), snapshotPath, snapshotDigest)
		cmd.Stderr = stderr
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("helm %s: %w", arguments[0], err)
		}
		return output, nil
	}
	common := []string{"--kubeconfig", plan.Kubeconfig, "--namespace", manifest.Namespace}
	checkRevision := func(ctx context.Context) error {
		output, err := command(ctx, append([]string{"list", "--deployed", "--failed", "--pending", "--superseded", "--uninstalled", "--uninstalling", "--filter", "^" + regexp.QuoteMeta(plan.Release) + "$", "--output", "json"}, common...)...)
		if err != nil {
			return err
		}
		var releases []struct {
			Name     string `json:"name"`
			Revision string `json:"revision"`
			Status   string `json:"status"`
		}
		if err := json.Unmarshal(output, &releases); err != nil {
			return err
		}
		for _, release := range releases {
			if release.Name != plan.Release {
				continue
			}
			status := "deployed"
			if plan.FailedRelease != nil {
				status = "failed"
			}
			if plan.ReleaseRevision == 0 || release.Revision != fmt.Sprint(plan.ReleaseRevision) || release.Status != status {
				return errors.New("Helm release revision/status differs from reviewed plan; pending operations require settled-request recovery")
			}
			if plan.FailedRelease != nil {
				history, err := command(ctx, append([]string{"history", plan.Release, "--max", "2147483647", "--output", "json"}, common...)...)
				if err != nil {
					return err
				}
				if err := validateFailedHistory(history, plan); err != nil {
					return err
				}
			}
			for _, entry := range []struct {
				artifact *installation.Artifact
				revision int
			}{{plan.PreviousManifest, plan.ReleaseRevision}, {plan.PreviousDeployedManifest, plan.PreviousDeployedRevision}} {
				if entry.artifact == nil {
					continue
				}
				previous, err := installation.ReadArtifact(*entry.artifact)
				if err != nil {
					return err
				}
				live, err := command(ctx, append([]string{"get", "manifest", plan.Release, "--revision", fmt.Sprint(entry.revision)}, common...)...)
				if err != nil {
					return err
				}
				left, err := installation.CanonicalManifest(previous)
				if err != nil {
					return err
				}
				right, err := installation.CanonicalManifest(live)
				if err != nil {
					return err
				}
				if string(left) != string(right) {
					return errors.New("previous Helm manifest differs from reviewed artifact")
				}
			}
			return nil
		}
		if plan.ReleaseRevision != 0 {
			return errors.New("reviewed Helm release is missing")
		}
		return nil
	}
	operations.Render = func(ctx context.Context) error {
		if err := checkRevision(ctx); err != nil {
			return err
		}
		version, err := discover.ServerVersion()
		if err != nil {
			return err
		}
		arguments := []string{"template", plan.Release, plan.Chart.Path, "--values", plan.Values.Path, "--set", "k8sController.activated=false", "--no-hooks", "--kube-version", version.GitVersion}
		if plan.ReleaseRevision > 0 {
			arguments = append(arguments, "--is-upgrade")
		}
		output, err := command(ctx, append(arguments, common...)...)
		if err != nil {
			return err
		}
		if mode == "fresh" {
			_, err = installation.FreshRender(expected, output, plan.Release)
		} else {
			_, err = installation.ValidateRender(expected, output, plan.Release)
		}
		return err
	}
	operations.Apply = func(ctx context.Context) error {
		if err := operations.VerifyTarget(ctx); err != nil {
			return err
		}
		if err := operations.VerifyDrained(ctx); err != nil {
			return err
		}
		if err := checkRevision(ctx); err != nil {
			return err
		}
		for _, artifact := range []installation.Artifact{plan.Chart, plan.Values, plan.Manifest} {
			if _, err := installation.ReadArtifact(artifact); err != nil {
				return err
			}
		}
		if err := operations.VerifyReleaseObjects(ctx); err != nil {
			return err
		}
		renderMode := "post-render"
		if mode == "fresh" {
			renderMode = "fresh-post-render"
		}
		output, err := command(ctx, append([]string{"upgrade", "--install", plan.Release, plan.Chart.Path, "--reset-values", "--values", plan.Values.Path, "--set", "k8sController.activated=false", "--no-hooks", "--skip-crds", "--post-renderer", executable, "--post-renderer-args", renderMode, "--timeout", "5m"}, common...)...)
		if err != nil {
			return err
		}
		_, err = stdout.Write(output)
		return err
	}
	if err := operations.VerifyTarget(ctx); err != nil {
		return err
	}
	record, err := installation.Claim(ctx, admin, plan, planDigest)
	if err != nil {
		return err
	}
	operations.Journal = record
	if _, err := fmt.Fprintf(stdout, "Installation claim %s/%s UID %s; retained until explicit settled-request recovery.\n", installation.JournalKey(plan).Namespace, installation.JournalKey(plan).Name, record.UID()); err != nil {
		return err
	}
	if err := installation.Bootstrap(ctx, operations, record, true); err != nil {
		return err
	}
	if mode == "fresh" {
		if err := installation.WaitForFreshServices(ctx, admin, expected); err != nil {
			return err
		}
		candidate, err := activation.ParseCandidate(expected, plan.Release)
		if err != nil {
			return err
		}
		binary, err := os.ReadFile(executable)
		if err != nil {
			return err
		}
		serverVersion, err := discover.ServerVersion()
		if err != nil {
			return err
		}
		replicas, err := freshReplicaCount(plan.Values)
		if err != nil {
			return err
		}
		target, err := candidate.Snapshot(ctx, admin, config.Host, serverVersion.GitVersion, plan.Chart.SHA256,
			fmt.Sprintf("sha256:%x", sha256.Sum256(binary)), plan.AuthorityNamespace, replicas)
		if err != nil {
			return err
		}
		authorization, err := record.AuthorizeFresh(ctx, plan, target)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "Admission and migrations verified; activation record %s/%s. Starting the controller after behavioral checks.\n", authorization.Namespace, authorization.Name); err != nil {
			return err
		}
		audit, err := candidate.ActivateFresh(ctx, admin, config, activation.ActivationOptions{
			Approval: client.ObjectKeyFromObject(authorization), Endpoint: config.Host,
			ServerVersion: serverVersion.GitVersion, ChartSHA256: target.ChartSHA256, ToolVersion: target.ToolVersion,
			Replicas: target.Replicas, WritersFrozen: true,
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(audit)
	}
	_, err = fmt.Fprintln(stdout, "Verified inactive bootstrap. Producers remain frozen. Use the separately approved SG-04 activation transaction only after all acceptance gates pass.")
	return err
}
