// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLocalUIServersServeAssetsAndShutdown(t *testing.T) {
	tests := []struct {
		name      string
		bind      string
		pageTitle string
		run       func(context.Context, localUIServerOptions) error
	}{
		{name: "studio", bind: "127.0.0.1", pageTitle: "<title>Kognition Studio</title>", run: runStudioServer},
		{name: "dashboard", bind: "0.0.0.0", pageTitle: "<title>Kognition Dashboard</title>", run: runDashboardServer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			listening := make(chan string, 1)
			done := make(chan error, 1)
			go func() {
				done <- tt.run(ctx, localUIServerOptions{
					bind:        tt.bind,
					onListening: func(url string) { listening <- url },
				})
			}()

			var url string
			select {
			case url = <-listening:
			case <-time.After(5 * time.Second):
				t.Fatal("server did not start")
			}
			if tt.bind == "0.0.0.0" && !strings.HasPrefix(url, "http://localhost:") {
				t.Fatalf("wildcard listener URL = %q, want localhost host", url)
			}

			resp, err := http.Get(url + "/")
			if err != nil {
				t.Fatalf("GET assets: %v", err)
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read assets: %v", readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET assets status = %d, want 200", resp.StatusCode)
			}
			if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
				t.Fatalf("GET assets content type = %q, want text/html", contentType)
			}
			if !strings.Contains(string(body), tt.pageTitle) {
				t.Fatalf("GET assets did not serve %s page", tt.name)
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("server shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not shut down")
			}
		})
	}
}

func TestStudioAssetsIncludeEvaluationDatasetBuilder(t *testing.T) {
	app, err := studioAssets.ReadFile("studio_assets/app.js")
	if err != nil {
		t.Fatalf("read studio app asset: %v", err)
	}
	index, err := studioAssets.ReadFile("studio_assets/index.html")
	if err != nil {
		t.Fatalf("read studio index asset: %v", err)
	}

	checks := map[string]string{
		"plural mapping":        `evaluationdataset: "evaluationdatasets"`,
		"refresh target":        `"evaluationdatasets"`,
		"kind mapping":          `evaluationdatasets: "EvaluationDataset"`,
		"draft factory":         `function newEvaluationDataset()`,
		"editor dispatch":       `renderEvaluationDatasetBuilder(host)`,
		"builder":               `function renderEvaluationDatasetBuilder(host)`,
		"case editor":           `function evaluationDatasetCasesEditor(items, onChange, onStructureChange)`,
		"case metadata editor":  `kvField("Metadata"`,
		"llm judge grader":      `"llm_judge"`,
		"case result renderer":  `function renderEvaluationCaseResults(caseResults, openKeyPrefix)`,
		"button event listener": `document.getElementById("btn-new-evaluationdataset").addEventListener("click", newEvaluationDataset)`,
	}
	for name, want := range checks {
		if !strings.Contains(string(app), want) {
			t.Fatalf("studio app missing %s: %s", name, want)
		}
	}

	indexChecks := map[string]string{
		"new button":   `id="btn-new-evaluationdataset"`,
		"sidebar list": `id="list-evaluationdatasets"`,
		"refresh hook": `data-refresh="evaluationdatasets"`,
	}
	for name, want := range indexChecks {
		if !strings.Contains(string(index), want) {
			t.Fatalf("studio index missing %s: %s", name, want)
		}
	}
}

func TestStudioAssetsIncludeModelGrantBuilder(t *testing.T) {
	app, err := studioAssets.ReadFile("studio_assets/app.js")
	if err != nil {
		t.Fatalf("read studio app asset: %v", err)
	}
	index, err := studioAssets.ReadFile("studio_assets/index.html")
	if err != nil {
		t.Fatalf("read studio index asset: %v", err)
	}

	checks := map[string]string{
		"plural mapping":        `modelgrant: "modelgrants"`,
		"refresh target":        `"modelgrants"`,
		"kind mapping":          `modelgrants: "ModelGrant"`,
		"draft factory":         `function newModelGrant()`,
		"editor dispatch":       `renderModelGrantBuilder(host)`,
		"builder":               `function renderModelGrantBuilder(host)`,
		"button event listener": `document.getElementById("btn-new-modelgrant").addEventListener("click", newModelGrant)`,
	}
	for name, want := range checks {
		if !strings.Contains(string(app), want) {
			t.Fatalf("studio app missing %s: %s", name, want)
		}
	}

	indexChecks := map[string]string{
		"new button":   `id="btn-new-modelgrant"`,
		"sidebar list": `id="list-modelgrants"`,
		"refresh hook": `data-refresh="modelgrants"`,
	}
	for name, want := range indexChecks {
		if !strings.Contains(string(index), want) {
			t.Fatalf("studio index missing %s: %s", name, want)
		}
	}
}

func TestStudioAssetsIncludeMemoryGrantBuilder(t *testing.T) {
	app, err := studioAssets.ReadFile("studio_assets/app.js")
	if err != nil {
		t.Fatalf("read studio app asset: %v", err)
	}
	index, err := studioAssets.ReadFile("studio_assets/index.html")
	if err != nil {
		t.Fatalf("read studio index asset: %v", err)
	}

	checks := map[string]string{
		"plural mapping":        `memorygrant: "memorygrants"`,
		"refresh target":        `"memorygrants"`,
		"kind mapping":          `memorygrants: "MemoryGrant"`,
		"draft factory":         `function newMemoryGrant()`,
		"editor dispatch":       `renderMemoryGrantBuilder(host)`,
		"builder":               `function renderMemoryGrantBuilder(host)`,
		"memory claim selector": `refSelect("Memory claim"`,
		"access mode selector":  `selectField("Access mode"`,
		"button event listener": `document.getElementById("btn-new-memorygrant").addEventListener("click", newMemoryGrant)`,
	}
	for name, want := range checks {
		if !strings.Contains(string(app), want) {
			t.Fatalf("studio app missing %s: %s", name, want)
		}
	}

	indexChecks := map[string]string{
		"new button":   `id="btn-new-memorygrant"`,
		"sidebar list": `id="list-memorygrants"`,
		"refresh hook": `data-refresh="memorygrants"`,
	}
	for name, want := range indexChecks {
		if !strings.Contains(string(index), want) {
			t.Fatalf("studio index missing %s: %s", name, want)
		}
	}
}

func TestStudioAssetsIncludeCrossNamespaceGrantBuilder(t *testing.T) {
	app, err := studioAssets.ReadFile("studio_assets/app.js")
	if err != nil {
		t.Fatalf("read studio app asset: %v", err)
	}
	index, err := studioAssets.ReadFile("studio_assets/index.html")
	if err != nil {
		t.Fatalf("read studio index asset: %v", err)
	}

	checks := map[string]string{
		"plural mapping":        `crossnamespacegrant: "crossnamespacegrants"`,
		"refresh target":        `"crossnamespacegrants"`,
		"kind mapping":          `crossnamespacegrants: "CrossNamespaceGrant"`,
		"draft factory":         `function newCrossNamespaceGrant()`,
		"editor dispatch":       `renderCrossNamespaceGrantBuilder(host)`,
		"builder":               `function renderCrossNamespaceGrantBuilder(host)`,
		"resources editor":      `function grantResourcesField(resources, onChange)`,
		"button event listener": `document.getElementById("btn-new-crossnamespacegrant").addEventListener("click", newCrossNamespaceGrant)`,
	}
	for name, want := range checks {
		if !strings.Contains(string(app), want) {
			t.Fatalf("studio app missing %s: %s", name, want)
		}
	}

	indexChecks := map[string]string{
		"new button":   `id="btn-new-crossnamespacegrant"`,
		"sidebar list": `id="list-crossnamespacegrants"`,
		"refresh hook": `data-refresh="crossnamespacegrants"`,
	}
	for name, want := range indexChecks {
		if !strings.Contains(string(index), want) {
			t.Fatalf("studio index missing %s: %s", name, want)
		}
	}
}

func TestStudioAssetsIncludeToolGrantBuilder(t *testing.T) {
	app, err := studioAssets.ReadFile("studio_assets/app.js")
	if err != nil {
		t.Fatalf("read studio app asset: %v", err)
	}
	index, err := studioAssets.ReadFile("studio_assets/index.html")
	if err != nil {
		t.Fatalf("read studio index asset: %v", err)
	}

	checks := map[string]string{
		"plural mapping":        `toolgrant: "toolgrants"`,
		"refresh target":        `"toolgrants"`,
		"kind mapping":          `toolgrants: "ToolGrant"`,
		"draft factory":         `function newToolGrant()`,
		"editor dispatch":       `renderToolGrantBuilder(host)`,
		"builder":               `function renderToolGrantBuilder(host)`,
		"subject mode":          `selectField("Agent targeting"`,
		"agent selector":        `kvField("Agent selector"`,
		"tool selector":         `refSelect("Tool"`,
		"verb editor":           `function toolGrantVerbsField(verbs, onChange)`,
		"argument constraints":  `jsonMapField("Argument constraints"`,
		"rate limits":           `numberField("Calls per minute"`,
		"conditions":            `kvField("Required agent labels"`,
		"button event listener": `document.getElementById("btn-new-toolgrant").addEventListener("click", newToolGrant)`,
	}
	for name, want := range checks {
		if !strings.Contains(string(app), want) {
			t.Fatalf("studio app missing %s: %s", name, want)
		}
	}

	indexChecks := map[string]string{
		"new button":   `id="btn-new-toolgrant"`,
		"sidebar list": `id="list-toolgrants"`,
		"refresh hook": `data-refresh="toolgrants"`,
	}
	for name, want := range indexChecks {
		if !strings.Contains(string(index), want) {
			t.Fatalf("studio index missing %s: %s", name, want)
		}
	}
}

func TestStudioAssetsIncludeBrandMark(t *testing.T) {
	index, err := studioAssets.ReadFile("studio_assets/index.html")
	if err != nil {
		t.Fatalf("read studio index asset: %v", err)
	}
	if !strings.Contains(string(index), `src="/kognition-mark.svg"`) || !strings.Contains(string(index), `rel="icon"`) {
		t.Fatal("studio index missing Kognition header mark or favicon")
	}
	if _, err := studioAssets.ReadFile("studio_assets/kognition-mark.svg"); err != nil {
		t.Fatalf("read studio brand mark: %v", err)
	}
}

func TestRunLocalUIServerRejectsInvalidBindAddress(t *testing.T) {
	err := runLocalUIServer(context.Background(), localUIServerOptions{
		name:    "test UI",
		bind:    "not a valid host name",
		handler: http.NotFoundHandler(),
	})
	if err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("runLocalUIServer error = %v, want listen error", err)
	}
}
