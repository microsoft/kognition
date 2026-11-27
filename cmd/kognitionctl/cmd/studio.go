// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/kognition-ai/kognition/internal/studiotutorial"
	"github.com/spf13/cobra"
)

//go:embed studio_assets
var studioAssets embed.FS

var (
	studioPort     int
	studioBind     string
	studioOpen     bool
	studioTutorial bool
)

var studioCmd = &cobra.Command{
	Use:   "studio",
	Short: "Launch Kognition Studio for visual resource authoring",
	Long: `Start a local web app for visually building, editing, and deploying
supported Kognition resources.

Studio gives you:
  • Guided builders and schema-backed All API fields controls for supported
    configuration, including autonomy classes, memory claims, secret references,
    models, prompts, policies, storage classes, workloads and routing.
  • A click-to-place DAG canvas for workflows with a property inspector
    covering steps, edges, approval gates, budgets and emergent settings.
  • Live YAML preview and round-trip editing, with one-click Deploy to the
    cluster (or the standalone apiserver — see --mode).
  • Explicit execution confirmation, read-only generated state, and clear
    explanations for protected or deferred capabilities.

Normal Studio runs in-process and reuses the CLI backend. Use --tutorial
for an isolated practice workspace with no backend or kubeconfig dependency.
Press Ctrl+C to stop.`,
	RunE: runStudio,
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		if studioTutorial {
			return nil
		}
		return resolveMode(cmd)
	},
}

func init() {
	studioCmd.Flags().IntVar(&studioPort, "port", 0, "Port to bind (0 = pick a free port)")
	studioCmd.Flags().StringVar(&studioBind, "bind", "127.0.0.1", "Address to bind the studio server")
	studioCmd.Flags().BoolVar(&studioOpen, "open", false, "Open the studio in the default browser after start")
	studioCmd.Flags().BoolVar(&studioTutorial, "tutorial", false, "Start isolated practice without a live backend or kubeconfig")
	rootCmd.AddCommand(studioCmd)
}

func runStudio(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runStudioServer(ctx, localUIServerOptions{})
}

func runStudioServer(ctx context.Context, opts localUIServerOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	assets, err := fs.Sub(studioAssets, "studio_assets")
	if err != nil {
		return fmt.Errorf("load studio assets: %w", err)
	}
	if studioTutorial {
		if studioBind != "127.0.0.1" || (opts.bind != "" && opts.bind != "127.0.0.1") {
			return fmt.Errorf("tutorial practice must bind to 127.0.0.1")
		}
		handler, err := studiotutorial.New(assets, studiotutorial.NewDefaultProgressStore())
		if err != nil {
			return err
		}
		opts.name, opts.bind, opts.handler = "Studio Practice", "127.0.0.1", handler
		opts.contextLabel = "Practice only: synthetic resources; no live backend or provider calls."
		opts.requestTimeout = 30 * time.Second
		if opts.port == 0 {
			opts.port = studioPort
		}
		opts.openBrowser = opts.openBrowser || studioOpen
		return runLocalUIServer(ctx, opts)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	practice := newStudioPractice(ctx, assets)
	registerResourceAPIWithMeta(mux, practice.metadata)
	mux.HandleFunc("GET /tutorial/start", practice.launch)
	mux.HandleFunc("POST /api/studio/tutorial-dismiss", practice.dismiss)
	registerStudioInvokeAPI(mux)
	registerStudioCollaborationAPI(mux)
	mux.HandleFunc("POST /api/studio/profile-digest", handleStudioProfileDigest)

	if opts.name == "" {
		opts.name = "Studio"
	}
	if opts.bind == "" {
		opts.bind = studioBind
	}
	if opts.port == 0 {
		opts.port = studioPort
	}
	if !opts.openBrowser {
		opts.openBrowser = studioOpen
	}
	opts.handler = mux
	return runLocalUIServer(ctx, opts)
}

func handleStudioProfileDigest(w http.ResponseWriter, r *http.Request) {
	digest, err := readInvocationProfileDigest(http.MaxBytesReader(w, r.Body, 4<<20), "")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"digest": digest})
}
