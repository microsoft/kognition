// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// out and errOut are bound by the caller so a long-lived server never
	// re-reads the process-wide os.Stdout/os.Stderr after the caller has moved on.
	out    io.Writer
	errOut io.Writer
}

func runLocalUIServer(ctx context.Context, opts localUIServerOptions) error {
	out, errOut := opts.out, opts.errOut
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
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

	_, _ = fmt.Fprintf(out, "Kognition %s listening on %s\n", opts.name, url)
	if opts.contextLabel != "" {
		_, _ = fmt.Fprintln(out, opts.contextLabel)
	} else {
		_, _ = fmt.Fprintf(out, "Mode: %s   Namespace: %s\n", currentModeLabel(), namespace)
	}
	_, _ = fmt.Fprintln(out, "Press Ctrl+C to stop.")
	if opts.onListening != nil {
		opts.onListening(url)
	}

	if opts.openBrowser {
		if err := browser.OpenURL(url); err != nil {
			fmt.Fprintf(errOut, "warning: could not open browser: %v\n", err)
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
		_, _ = fmt.Fprintf(out, "\nShutting down %s...\n", opts.name)
	case serveErr = <-errCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return serveErr
}
