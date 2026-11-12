// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type tutorialBackendTrap struct{ calls atomic.Int32 }

func (t *tutorialBackendTrap) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("live backend must not be used by practice")
}

func tutorialTestProfile(t *testing.T) {
	t.Helper()
	profile := t.TempDir()
	t.Setenv("APPDATA", profile)
	t.Setenv("XDG_CONFIG_HOME", profile)
	t.Setenv("KUBECONFIG", filepath.Join(profile, "nonexistent-kubeconfig"))
}

func TestStudioTutorialBootSkipsBackendInitialization(t *testing.T) {
	// TC-0340: tutorial-only startup must not resolve live backend access.
	tutorialTestProfile(t)
	previousTutorial, previousMode, previousHTTP := studioTutorial, cliMode, httpClient
	studioTutorial, cliMode = true, "kubernetes"
	trap := &tutorialBackendTrap{}
	httpClient = &http.Client{Transport: trap}
	t.Cleanup(func() { studioTutorial, cliMode, httpClient = previousTutorial, previousMode, previousHTTP })
	if err := studioCmd.PersistentPreRunE(studioCmd, nil); err != nil {
		t.Fatalf("practice depended on unavailable kubeconfig: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening, done := make(chan string, 1), make(chan error, 1)
	go func() {
		done <- runStudioServer(ctx, localUIServerOptions{onListening: func(address string) { listening <- address }})
	}()
	var address string
	select {
	case address = <-listening:
	case err := <-done:
		t.Fatalf("practice startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("practice did not start")
	}
	client := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get(address + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	err = json.NewDecoder(response.Body).Decode(&meta)
	response.Body.Close()
	if err != nil || meta["environment"] != "practice" || meta["namespace"] != "tutorial" {
		t.Fatalf("practice metadata: %#v %v", meta, err)
	}
	if trap.calls.Load() != 0 {
		t.Fatal("tutorial-only boot contacted a live backend")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStudioPracticeLaunchUsesSeparateOwnedOrigin(t *testing.T) {
	tutorialTestProfile(t)
	assets, err := fs.Sub(studioAssets, "studio_assets")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	practice := newStudioPractice(ctx, assets)
	address, err := practice.start()
	if err != nil {
		t.Fatal(err)
	}
	reused, err := practice.start()
	if err != nil || reused != address {
		t.Fatalf("launch created unbounded services: %s %s %v", address, reused, err)
	}
	client := &http.Client{Transport: &http.Transport{}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get(address + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	cancel()
	select {
	case err := <-practice.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("practice outlived its owning Studio context")
	}
	if _, err := client.Get(address + "/api/meta"); err == nil {
		t.Fatal("practice listener survived shutdown")
	}
}

func TestStudioTutorialLaunchEndpointUsesSeparateOriginWithoutLiveCalls(t *testing.T) {
	tutorialTestProfile(t)
	previousTutorial, previousMode, previousHTTP := studioTutorial, k8sMode, httpClient
	studioTutorial, k8sMode = false, false
	trap := &tutorialBackendTrap{}
	httpClient = &http.Client{Transport: trap}
	t.Cleanup(func() { studioTutorial, k8sMode, httpClient = previousTutorial, previousMode, previousHTTP })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() {
		done <- runStudioServer(ctx, localUIServerOptions{onListening: func(address string) { ready <- address }})
	}()
	var mainURL string
	select {
	case mainURL = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("Studio did not start")
	}
	client := &http.Client{
		Transport: &http.Transport{}, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(mainURL + "/tutorial/start")
	if err != nil {
		t.Fatal(err)
	}
	practiceURL := response.Header.Get("Location")
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || practiceURL == "" || practiceURL == mainURL {
		t.Fatalf("practice did not get its own origin: %d %q", response.StatusCode, practiceURL)
	}
	response, err = client.Get(practiceURL + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	err = json.NewDecoder(response.Body).Decode(&meta)
	response.Body.Close()
	if err != nil || meta["environment"] != "practice" || trap.calls.Load() != 0 {
		t.Fatalf("launch reached live state: %#v %v, calls=%d", meta, err, trap.calls.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Studio did not stop")
	}
}

func TestStudioTutorialDoesNotCreateProgressUntilAnExplicitAction(t *testing.T) {
	tutorialTestProfile(t)
	assets, err := fs.Sub(studioAssets, "studio_assets")
	if err != nil {
		t.Fatal(err)
	}
	newStudioPractice(context.Background(), assets)
	entries, err := os.ReadDir(os.Getenv("APPDATA"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("constructing tutorial support wrote user state: %v %v", entries, err)
	}
}
