// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kognition-ai/kognition/internal/studiotutorial"
)

type studioPractice struct {
	mu     sync.Mutex
	ctx    context.Context
	assets fs.FS
	store  *studiotutorial.ProgressStore
	url    string
	done   chan error
}

func newStudioPractice(ctx context.Context, assets fs.FS) *studioPractice {
	return &studioPractice{ctx: ctx, assets: assets, store: studiotutorial.NewDefaultProgressStore()}
}

func localTutorialRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		return err == nil && parsed.Scheme == "http" && parsed.Host == r.Host
	}
	return true
}

func (p *studioPractice) start() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	if p.url != "" {
		select {
		case err := <-p.done:
			p.url = ""
			if err != nil {
				return "", err
			}
		default:
			return p.url, nil
		}
	}
	handler, err := studiotutorial.New(p.assets, p.store)
	if err != nil {
		return "", err
	}
	ready := make(chan string, 1)
	done := make(chan error, 1)
	p.done = done
	childCtx, cancel := context.WithCancel(p.ctx)
	go func() {
		defer cancel()
		done <- runLocalUIServer(childCtx, localUIServerOptions{
			name: "Studio Practice", bind: "127.0.0.1", handler: handler,
			contextLabel:   "Practice only: synthetic resources; no live backend or provider calls.",
			requestTimeout: 30 * time.Second,
			onListening:    func(address string) { ready <- address },
		})
	}()
	select {
	case address := <-ready:
		p.url = address
		return address, nil
	case err := <-done:
		cancel()
		if err == nil {
			err = errors.New("practice stopped before becoming available")
		}
		return "", err
	case <-p.ctx.Done():
		cancel()
		return "", p.ctx.Err()
	case <-time.After(5 * time.Second):
		cancel()
		return "", errors.New("practice did not become available")
	}
}

func (p *studioPractice) launch(w http.ResponseWriter, r *http.Request) {
	if !localTutorialRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Practice is available only from local Studio."})
		return
	}
	address, err := p.start()
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, address, http.StatusSeeOther)
}

func (p *studioPractice) metadata(w http.ResponseWriter, r *http.Request) {
	progress, err := p.store.Load()
	warning := ""
	if err != nil {
		warning = "Tutorial progress is unavailable. Practice can continue without saved progress."
	}
	writeJSON(w, http.StatusOK, struct {
		dashboardMeta
		TutorialAvailable bool   `json:"tutorialAvailable"`
		TutorialDismissed bool   `json:"tutorialDismissed"`
		TutorialWarning   string `json:"tutorialWarning,omitempty"`
	}{dashboardMetadata(), localTutorialRequest(r), progress.Dismissed, warning})
}

func (p *studioPractice) dismiss(w http.ResponseWriter, r *http.Request) {
	if !localTutorialRequest(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "A same-origin Studio request is required."})
		return
	}
	progress, err := p.store.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	progress.Dismissed = true
	if _, err := p.store.Save(progress, false); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"dismissed": true})
}
