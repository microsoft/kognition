// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package studiotutorial

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const CurriculumVersion = 1
const maxProgressCounter = uint64(1<<53 - 1)

var StepIDs = []string{"practice", "create", "instructions", "yaml", "save", "preview", "challenge"}
var ErrProgressConflict = errors.New("tutorial progress changed in another session; reload practice")

type Progress struct {
	Version    int      `json:"version"`
	Generation uint64   `json:"generation"`
	Revision   uint64   `json:"revision"`
	Completed  []string `json:"completed"`
	Skipped    []string `json:"skipped"`
	Cursor     int      `json:"cursor"`
	Dismissed  bool     `json:"dismissed"`
}

func emptyProgress() Progress {
	return Progress{Version: CurriculumVersion, Generation: 1, Completed: []string{}, Skipped: []string{}}
}

type ProgressStore struct {
	path              string
	initializationErr error
}

func NewProgressStore(path string) *ProgressStore { return &ProgressStore{path: path} }

func NewDefaultProgressStore() *ProgressStore {
	path, err := DefaultProgressPath()
	return &ProgressStore{path: path, initializationErr: err}
}

func DefaultProgressPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "kognition", "studio", "tutorial-progress.json"), nil
}

func validProgress(p Progress) error {
	if p.Version != CurriculumVersion {
		return errors.New("tutorial curriculum changed; restart this lesson")
	}
	if p.Generation == 0 || p.Generation >= maxProgressCounter || p.Revision >= maxProgressCounter || p.Cursor < 0 || p.Cursor >= len(StepIDs) {
		return errors.New("invalid tutorial progress; restart this lesson")
	}
	for _, list := range [][]string{p.Completed, p.Skipped} {
		if len(list) > len(StepIDs) {
			return errors.New("invalid tutorial progress entries")
		}
		seen := map[string]bool{}
		for _, id := range list {
			if stepIndex(id) < 0 || seen[id] {
				return errors.New("unknown or duplicate tutorial step")
			}
			seen[id] = true
		}
	}
	return nil
}

func stepIndex(id string) int {
	for i, candidate := range StepIDs {
		if candidate == id {
			return i
		}
	}
	return -1
}

func (s *ProgressStore) read() (Progress, error) {
	p := emptyProgress()
	if s.initializationErr != nil {
		return p, s.initializationErr
	}
	if s.path == "" {
		return p, errors.New("local tutorial progress storage is unavailable")
	}
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return emptyProgress(), fmt.Errorf("read tutorial progress: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return emptyProgress(), errors.New("invalid tutorial progress document")
	}
	return p, validProgress(p)
}

func (s *ProgressStore) Load() (Progress, error) { return s.read() }

// Save merges completion from concurrent CLI instances, but never crosses a reset generation.
func (s *ProgressStore) Save(p Progress, reset bool) (Progress, error) {
	if s.initializationErr != nil {
		return p, s.initializationErr
	}
	if s.path == "" {
		return p, errors.New("local tutorial progress storage is unavailable")
	}
	if err := validProgress(p); err != nil {
		return p, err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return p, err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return p, err
	}
	defer lock.Close()
	unlock, err := lockProgress(lock)
	if err != nil {
		return p, fmt.Errorf("tutorial progress is busy: %w", err)
	}
	defer unlock()
	current, readErr := s.read()
	if readErr != nil && !reset {
		return p, readErr
	}
	if readErr != nil {
		current = emptyProgress()
	}
	if readErr == nil && current.Generation != p.Generation {
		return p, ErrProgressConflict
	}
	if reset {
		p.Generation = max(p.Generation, current.Generation) + 1
	} else {
		for _, id := range current.Completed {
			p.Completed = include(p.Completed, id)
		}
		for _, id := range current.Skipped {
			p.Skipped = include(p.Skipped, id)
		}
	}
	skipped := []string{}
	for _, id := range p.Skipped {
		if !contains(p.Completed, id) {
			skipped = append(skipped, id)
		}
	}
	p.Skipped = skipped
	p.Revision = current.Revision + 1
	if reset {
		p.Revision = 1
	}
	if err := validProgress(p); err != nil {
		return p, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tutorial-progress-*")
	if err != nil {
		return p, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return p, err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return p, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return p, err
	}
	if err := tmp.Close(); err != nil {
		return p, err
	}
	if err := os.Rename(name, s.path); err != nil {
		return p, err
	}
	return p, nil
}

func include(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

func contains(ids []string, id string) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}
