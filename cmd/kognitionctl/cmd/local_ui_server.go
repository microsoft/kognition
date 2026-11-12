// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/pkg/browser"
)

type localUIServerOptions struct {
	name           string
	bind           string
	port           int
	handler        http.Handler
	openBrowser    bool
	onListening    func(string)
	contextLabel   string
	requestTimeout time.Duration
}

func runLocalUIServer(ctx context.Context, opts localUIServerOptions) error {
	addr := fmt.Sprintf("%s:%d", opts.bind, opts.port)
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "localhost"
	}
	url := fmt.Sprintf("http://%s:%s", host, port)
	srv := &http.Server{
		Handler: opts.handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: opts.requestTimeout, WriteTimeout: opts.requestTimeout, IdleTimeout: 2 * opts.requestTimeout,
	}

	_, _ = fmt.Fprintf(os.Stdout, "Kognition %s listening on %s\n", opts.name, url)
	if opts.contextLabel != "" {
		_, _ = fmt.Fprintln(os.Stdout, opts.contextLabel)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "Mode: %s   Namespace: %s\n", currentModeLabel(), namespace)
	}
	_, _ = fmt.Fprintln(os.Stdout, "Press Ctrl+C to stop.")
	if opts.onListening != nil {
		opts.onListening(url)
	}

	if opts.openBrowser {
		if err := browser.OpenURL(url); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not open browser: %v\n", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		_, _ = fmt.Fprintf(os.Stdout, "\nShutting down %s...\n", opts.name)
	case serveErr = <-errCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return serveErr
}
