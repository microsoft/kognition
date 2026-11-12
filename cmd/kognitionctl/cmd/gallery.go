// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed gallery_assets
var galleryAssets embed.FS

// galleryItem is the user-facing description of a gallery entry.
type galleryItem struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Icon        string            `json:"icon,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Primary     galleryPrimary    `json:"primary"`
	File        string            `json:"file"`
	Resources   []galleryResource `json:"resources,omitempty"`
}

type galleryPrimary struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type galleryResource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type galleryManifest struct {
	Items []galleryItem `json:"items"`
}

var (
	galleryOnce sync.Once
	galleryData []galleryItem
	galleryErr  error
)

func loadGallery() ([]galleryItem, error) {
	galleryOnce.Do(func() {
		raw, err := galleryAssets.ReadFile("gallery_assets/manifest.json")
		if err != nil {
			galleryErr = fmt.Errorf("read gallery manifest: %w", err)
			return
		}
		var m galleryManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			galleryErr = fmt.Errorf("parse gallery manifest: %w", err)
			return
		}
		for i := range m.Items {
			it := &m.Items[i]
			docs, err := readGalleryDocs(it.File)
			if err != nil {
				galleryErr = fmt.Errorf("gallery item %q: %w", it.ID, err)
				return
			}
			it.Resources = make([]galleryResource, 0, len(docs))
			for _, d := range docs {
				kind, _ := d["kind"].(string)
				meta, _ := d["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				if kind != "" && name != "" {
					it.Resources = append(it.Resources, galleryResource{Kind: kind, Name: name})
				}
			}
		}
		// Stable ordering for the UI.
		sort.SliceStable(m.Items, func(i, j int) bool { return m.Items[i].ID < m.Items[j].ID })
		galleryData = m.Items
	})
	return galleryData, galleryErr
}

// readGalleryDocs reads a multi-document YAML file from the embed.FS and
// returns each document as a normalized map.
func readGalleryDocs(file string) ([]map[string]any, error) {
	raw, err := galleryAssets.ReadFile("gallery_assets/" + file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out []map[string]any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		norm := normalizeYAMLDecoded(v)
		m, ok := norm.(map[string]any)
		if !ok || len(m) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func registerGalleryAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/gallery", handleGalleryList)
	mux.HandleFunc("GET /api/gallery/{id}/yaml", handleGalleryYAML)
	mux.HandleFunc("POST /api/gallery/{id}/import", handleGalleryImport)
}

func handleGalleryList(w http.ResponseWriter, _ *http.Request) {
	items, err := loadGallery()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func handleGalleryYAML(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	it, ok := findGalleryItem(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("gallery item %q not found", id)})
		return
	}
	raw, err := galleryAssets.ReadFile("gallery_assets/" + it.File)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(raw)
}

type galleryImportResult struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"` // "created" | "exists" | "error"
	Message   string `json:"message,omitempty"`
}

func handleGalleryImport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	it, ok := findGalleryItem(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("gallery item %q not found", id)})
		return
	}

	ns := dashboardNamespace(r)
	if ns == "" {
		ns = "default"
	}

	docs, err := readGalleryDocs(it.File)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	results := make([]galleryImportResult, 0, len(docs))
	hadHardError := false
	for _, obj := range docs {
		kind, _ := obj["kind"].(string)
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if kind == "" || name == "" {
			continue
		}
		// Force the requested namespace onto every doc so the bundle lands
		// where the user picked, regardless of what the YAML embeds.
		if meta == nil {
			meta = map[string]any{}
			obj["metadata"] = meta
		}
		meta["namespace"] = ns

		res := galleryImportResult{Kind: kind, Name: name, Namespace: ns}
		urlKind := strings.ToLower(pluralize(kind))
		if _, err := dashCreate(ns, urlKind, obj); err != nil {
			msg := err.Error()
			if isAlreadyExistsError(msg) {
				res.Status = "exists"
				res.Message = "already present in this namespace; left unchanged"
			} else {
				res.Status = "error"
				res.Message = msg
				hadHardError = true
			}
		} else {
			res.Status = "created"
		}
		results = append(results, res)
	}

	status := http.StatusOK
	if hadHardError {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{
		"id":      it.ID,
		"primary": it.Primary,
		"results": results,
	})
}

func findGalleryItem(id string) (galleryItem, bool) {
	items, err := loadGallery()
	if err != nil {
		return galleryItem{}, false
	}
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return galleryItem{}, false
}

func isAlreadyExistsError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "already exists") || strings.Contains(low, "alreadyexists")
}
