// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package studiotutorial serves synthetic Studio practice without live backend adapters.
package studiotutorial

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"sigs.k8s.io/yaml"
)

const PracticeNamespace = "tutorial"
const bodyLimit = 256 << 10

var objectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type Server struct {
	mu       sync.Mutex
	store    *ProgressStore
	progress Progress
	warning  string
	token    string
	objects  map[string]map[string]json.RawMessage
	known    map[string]string
	lastSave string
	preview  bool
	yamlSeen bool
	paused   bool
	sequence int
	mux      *http.ServeMux
}

func New(assets fs.FS, progress *ProgressStore) (*Server, error) {
	if progress == nil {
		return nil, errors.New("tutorial progress dependency is required")
	}
	raw, err := fs.ReadFile(assets, "resource-catalog.json")
	if err != nil {
		return nil, err
	}
	var catalog []struct{ Kind, Plural, Treatment string }
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	s := &Server{store: progress, token: hex.EncodeToString(token[:]), known: map[string]string{}, mux: http.NewServeMux()}
	for _, entry := range catalog {
		if entry.Treatment != "unavailable" {
			s.known[entry.Plural] = entry.Kind
		}
	}
	s.progress, err = progress.Load()
	if err != nil {
		generation := s.progress.Generation
		s.progress = emptyProgress()
		if generation > 0 && generation < maxProgressCounter {
			s.progress.Generation = generation
		}
		s.warning = "Saved progress is unavailable or incompatible. Restart the lesson; progress may remain in memory only."
	}
	s.seed()
	s.mux.HandleFunc("GET /api/meta", s.meta)
	s.mux.HandleFunc("GET /api/namespaces", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, []string{PracticeNamespace})
	})
	s.mux.HandleFunc("GET /api/tutorial", s.session)
	s.mux.HandleFunc("POST /api/tutorial", s.lesson)
	s.mux.HandleFunc("/api/resources/{plural}", s.resources)
	s.mux.HandleFunc("/api/resources/{plural}/{name}", s.resources)
	s.mux.HandleFunc("POST /api/invoke/agent/{name}", s.invoke)
	s.mux.HandleFunc("POST /api/parse-yaml", s.parseYAML)
	s.mux.HandleFunc("POST /api/studio/profile-digest", s.profileDigest)
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusNotImplemented, "This operation is not available in practice. No live request was made.")
	})
	files := http.FileServer(http.FS(assets))
	s.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			problem(w, http.StatusMethodNotAllowed, "Practice assets are read-only.")
			return
		}
		files.ServeHTTP(w, r)
	}))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		problem(w, http.StatusForbidden, "Practice requires its owned loopback origin.")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "http" || parsed.Host != r.Host {
			problem(w, http.StatusForbidden, "Cross-origin practice requests are not allowed.")
			return
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); (site == "cross-site" || site == "same-site") &&
		!(r.Method == http.MethodGet && r.URL.Path == "/" && r.Header.Get("Sec-Fetch-Mode") == "navigate") {
		problem(w, http.StatusForbidden, "Use the practice origin directly.")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-Kognition-Practice") != s.token {
			problem(w, http.StatusForbidden, "Practice session ownership is required.")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	s.mux.ServeHTTP(w, r)
}

func (s *Server) seed() {
	s.objects = map[string]map[string]json.RawMessage{}
	s.objects["models"] = map[string]json.RawMessage{
		"tutorial-model": json.RawMessage(`{"apiVersion":"kognition.dev/v1alpha1","kind":"Model","metadata":{"name":"tutorial-model","namespace":"tutorial","uid":"practice-model","resourceVersion":1},"spec":{"provider":"openai-compatible","modelID":"tutorial-only","endpoint":"https://example.invalid/v1","capabilities":["chat"]},"status":{"phase":"Ready","message":"Simulated model; no provider connection"}}`),
	}
	s.objects["agents"] = map[string]json.RawMessage{}
	s.objects["prompts"] = map[string]json.RawMessage{}
	s.lastSave, s.preview, s.yamlSeen = "", false, contains(s.progress.Completed, "yaml")
	if contains(s.progress.Completed, "save") {
		agent := checkpointAgent()
		raw, _ := json.Marshal(agent)
		s.objects["agents"]["practice-agent"] = raw
		s.lastSave = "practice-agent"
		s.preview = contains(s.progress.Completed, "preview")
	}
}

func checkpointAgent() map[string]any {
	return map[string]any{
		"apiVersion": api.APIVersion, "kind": "Agent",
		"metadata": map[string]any{"name": "practice-agent", "namespace": PracticeNamespace, "uid": "practice-agent", "resourceVersion": 1},
		"spec":     map[string]any{"modelRef": map[string]any{"name": "tutorial-model"}, "instructions": map[string]any{"inline": "Answer briefly and explain your next step."}},
	}
}

func (s *Server) meta(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	respond(w, http.StatusOK, map[string]any{
		"mode": "standalone", "namespace": PracticeNamespace, "environment": "practice",
		"tutorial": map[string]any{"token": s.token, "progress": s.progress, "warning": s.warning},
	})
}

func (s *Server) session(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.result()
	if raw, found := s.objects["agents"][s.lastSave]; found {
		result["draft"] = raw
		result["existsInPractice"] = true
	} else if contains(s.progress.Completed, "create") {
		result["draft"] = checkpointAgent()
		result["existsInPractice"] = false
	}
	respond(w, http.StatusOK, result)
}

func (s *Server) result() map[string]any {
	return map[string]any{"progress": s.progress, "warning": s.warning, "paused": s.paused, "simulated": true}
}

func (s *Server) persist(reset bool) error {
	progress, err := s.store.Save(s.progress, reset)
	if errors.Is(err, ErrProgressConflict) {
		current, readErr := s.store.Load()
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		s.progress = current
		s.seed()
		return err
	}
	if err != nil {
		s.warning = "Progress is in memory only: local progress could not be saved. You can continue, but resume after restart is unavailable."
		s.progress.Revision++
		if reset {
			s.progress.Generation++
		}
		return nil
	}
	s.progress, s.warning = progress, ""
	return nil
}

func (s *Server) resources(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method != http.MethodGet && !s.currentGeneration(w, r) {
		return
	}
	plural, name := r.PathValue("plural"), r.PathValue("name")
	kind, known := s.known[plural]
	if !known {
		problem(w, http.StatusForbidden, "This resource has no practice API.")
		return
	}
	if ns := r.URL.Query().Get("namespace"); ns != "" && ns != PracticeNamespace && ns != "default" {
		problem(w, http.StatusBadRequest, "Only the virtual tutorial namespace is available.")
		return
	}
	if r.Method == http.MethodGet {
		if name != "" {
			if raw, ok := s.objects[plural][name]; ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			} else {
				problem(w, http.StatusNotFound, "Practice resource not found.")
			}
			return
		}
		items := []json.RawMessage{}
		for _, raw := range s.objects[plural] {
			items = append(items, raw)
		}
		respond(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	if plural != "agents" && plural != "prompts" {
		problem(w, http.StatusNotImplemented, "This lesson supports Agent and Prompt writes only. Other operations remain unavailable in practice.")
		return
	}
	if r.Method == http.MethodDelete {
		if _, ok := s.objects[plural][name]; !ok {
			problem(w, http.StatusNotFound, "Practice resource not found.")
			return
		}
		delete(s.objects[plural], name)
		if name == s.lastSave {
			s.lastSave, s.preview = "", false
		}
		respond(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		problem(w, http.StatusMethodNotAllowed, "Unsupported practice method.")
		return
	}
	var object map[string]any
	if err := decode(r, &object); err != nil {
		problem(w, http.StatusBadRequest, "Invalid or oversized practice manifest.")
		return
	}
	meta, _ := object["metadata"].(map[string]any)
	objectKind, _ := object["kind"].(string)
	objectVersion, _ := object["apiVersion"].(string)
	objectNameValue, _ := meta["name"].(string)
	if objectKind != kind || objectVersion != api.APIVersion || !validName(objectNameValue) || (name != "" && name != objectNameValue) {
		problem(w, http.StatusUnprocessableEntity, "Use the canonical kind/version and a valid matching resource name.")
		return
	}
	if ns, _ := meta["namespace"].(string); ns != "" && ns != PracticeNamespace {
		problem(w, http.StatusUnprocessableEntity, "Practice resources must use the tutorial namespace.")
		return
	}
	raw, err := json.Marshal(object)
	if err != nil {
		problem(w, http.StatusBadRequest, "Invalid practice manifest.")
		return
	}
	if kind == "Agent" {
		if _, err := s.agent(raw, true); err != nil {
			problem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	} else {
		var prompt api.Prompt
		if json.Unmarshal(raw, &prompt) != nil || strings.TrimSpace(prompt.Spec.ResolvedContent()) == "" {
			problem(w, http.StatusUnprocessableEntity, "Prompt content must not be empty.")
			return
		}
	}
	old, exists := s.objects[plural][objectNameValue]
	version := float64(1)
	if r.Method == http.MethodPut {
		var previous map[string]any
		if !exists {
			problem(w, http.StatusNotFound, "Practice resource not found.")
			return
		}
		if err := json.Unmarshal(old, &previous); err != nil {
			problem(w, http.StatusInternalServerError, "Practice state is invalid; restart the lesson.")
			return
		}
		previousMeta := previous["metadata"].(map[string]any)
		if meta["resourceVersion"] != previousMeta["resourceVersion"] {
			problem(w, http.StatusConflict, "Practice resource changed; reload it before saving.")
			return
		}
		version = previousMeta["resourceVersion"].(float64) + 1
		meta["uid"] = previousMeta["uid"]
	} else {
		if exists {
			problem(w, http.StatusConflict, "A practice resource with this name already exists.")
			return
		}
		if len(s.objects[plural]) >= 16 {
			problem(w, http.StatusTooManyRequests, "Practice resource limit reached; reset the lesson.")
			return
		}
		s.sequence++
		meta["uid"] = fmt.Sprintf("practice-%d", s.sequence)
	}
	meta["namespace"], meta["resourceVersion"] = PracticeNamespace, version
	object["metadata"] = meta
	delete(object, "status")
	raw, _ = json.Marshal(object)
	s.objects[plural][objectNameValue] = raw
	if kind == "Agent" {
		s.lastSave, s.preview = objectNameValue, false
		s.progress.Completed = include(s.progress.Completed, "save")
		if err := s.persist(false); err != nil {
			problem(w, http.StatusConflict, err.Error())
			return
		}
	}
	respond(w, http.StatusOK, object)
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || !objectName.MatchString(label) {
			return false
		}
	}
	return true
}

func (s *Server) agent(raw []byte, requireInstructions bool) (*api.Agent, error) {
	var agent api.Agent
	if json.Unmarshal(raw, &agent) != nil || agent.Kind != "Agent" || agent.APIVersion != api.APIVersion ||
		!validName(agent.Name) || agent.Spec.ModelRef.Name != "tutorial-model" {
		return nil, errors.New("Name the Agent and select tutorial-model.")
	}
	if agent.Namespace != "" && agent.Namespace != PracticeNamespace {
		return nil, errors.New("Use the virtual tutorial namespace.")
	}
	instructions := agent.Spec.Instructions.Inline
	if agent.Spec.Instructions.Ref != nil {
		raw, ok := s.objects["prompts"][agent.Spec.Instructions.Ref.Name]
		if !ok {
			return nil, errors.New("The referenced practice Prompt does not exist.")
		}
		var prompt api.Prompt
		if err := json.Unmarshal(raw, &prompt); err != nil {
			return nil, err
		}
		instructions = prompt.Spec.ResolvedContent()
	}
	if requireInstructions && strings.TrimSpace(instructions) == "" {
		return nil, errors.New("Add nonempty instructions before saving in practice.")
	}
	return &agent, nil
}

func (s *Server) invoke(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentGeneration(w, r) {
		return
	}
	raw, found := s.objects["agents"][r.PathValue("name")]
	if !found {
		problem(w, http.StatusNotFound, "Save this Agent in practice before previewing it.")
		return
	}
	if _, err := s.agent(raw, true); err != nil {
		problem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var request struct {
		Message string `json:"message"`
	}
	if decode(r, &request) != nil || strings.TrimSpace(request.Message) == "" {
		problem(w, http.StatusBadRequest, "Enter a practice message.")
		return
	}
	s.preview = true
	s.progress.Completed = include(s.progress.Completed, "preview")
	if err := s.persist(false); err != nil {
		problem(w, http.StatusConflict, err.Error())
		return
	}
	respond(w, http.StatusOK, map[string]any{
		"sessionID": "practice-session", "response": "Simulated response: I received your practice message. This demonstrates Studio's preview interface, not real model reasoning. No provider was contacted.",
		"simulated": true, "usage": map[string]int{"totalTokens": 0},
	})
}

func (s *Server) lesson(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Action     string          `json:"action"`
		Generation uint64          `json:"generation"`
		Draft      json.RawMessage `json:"draft"`
		View       string          `json:"view"`
		Answer     string          `json:"answer"`
	}
	if decode(r, &request) != nil {
		problem(w, http.StatusBadRequest, "Invalid practice action.")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Generation != s.progress.Generation {
		problem(w, http.StatusConflict, ErrProgressConflict.Error())
		return
	}
	switch request.Action {
	case "start", "reset":
		old := s.progress
		s.progress = emptyProgress()
		s.progress.Generation, s.progress.Dismissed = old.Generation, old.Dismissed
		if request.Action == "start" {
			s.progress.Completed = []string{"practice"}
		}
		if err := s.persist(true); err != nil {
			problem(w, http.StatusConflict, err.Error())
			return
		}
		s.seed()
		s.paused = false
	case "pause":
		s.paused = true
	case "exit":
		s.seed()
		s.paused = true
	case "resume":
		s.paused = false
	case "next":
		id := StepIDs[s.progress.Cursor]
		if !contains(s.progress.Completed, id) && !contains(s.progress.Skipped, id) {
			problem(w, http.StatusUnprocessableEntity, "Finish the current task or explicitly skip it.")
			return
		}
		if s.progress.Cursor >= 1 && s.progress.Cursor <= 3 {
			if _, err := s.agent(request.Draft, s.progress.Cursor >= 2); err != nil {
				problem(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
		}
		if (id == "save" && s.lastSave == "") || (id == "preview" && !s.preview) {
			problem(w, http.StatusUnprocessableEntity, "Revisit this practice action before continuing.")
			return
		}
		s.progress.Cursor = min(s.progress.Cursor+1, len(StepIDs)-1)
	case "back":
		s.progress.Cursor = max(0, s.progress.Cursor-1)
	case "skip":
		id := StepIDs[s.progress.Cursor]
		if !contains(s.progress.Completed, id) {
			s.progress.Skipped = include(s.progress.Skipped, id)
		}
		s.progress.Cursor = min(s.progress.Cursor+1, len(StepIDs)-1)
	case "check":
		if len(request.Draft) > 0 {
			agent, err := s.agent(request.Draft, false)
			if err == nil {
				s.progress.Completed = include(s.progress.Completed, "create")
				if _, err := s.agent(request.Draft, true); err == nil {
					s.progress.Completed = include(s.progress.Completed, "instructions")
					if request.View == "yaml" {
						s.yamlSeen = true
					}
					if s.yamlSeen && request.View == "builder" {
						s.progress.Completed = include(s.progress.Completed, "yaml")
					}
					if s.preview && request.Answer == "practice" && s.lastSave != "" {
						var saved api.Agent
						if json.Unmarshal(s.objects["agents"][s.lastSave], &saved) == nil &&
							(agent.Name != saved.Name || agent.Spec.Instructions.Inline != saved.Spec.Instructions.Inline) {
							s.progress.Completed = include(s.progress.Completed, "challenge")
						}
					}
				}
			}
		}
	default:
		problem(w, http.StatusBadRequest, "Unknown practice action.")
		return
	}
	if request.Action != "start" && request.Action != "reset" {
		if err := s.persist(false); err != nil {
			problem(w, http.StatusConflict, err.Error())
			return
		}
	}
	respond(w, http.StatusOK, s.result())
}

func (s *Server) currentGeneration(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Kognition-Practice-Generation") != strconv.FormatUint(s.progress.Generation, 10) {
		problem(w, http.StatusConflict, ErrProgressConflict.Error())
		return false
	}
	return true
}

func (s *Server) parseYAML(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		problem(w, http.StatusBadRequest, "Practice manifest is too large.")
		return
	}
	data, err := yaml.YAMLToJSON(raw)
	if err != nil {
		problem(w, http.StatusBadRequest, "Invalid YAML.")
		return
	}
	var object map[string]any
	if json.Unmarshal(data, &object) != nil || object == nil {
		problem(w, http.StatusBadRequest, "Use a single resource mapping.")
		return
	}
	respond(w, http.StatusOK, object)
}

func (s *Server) profileDigest(w http.ResponseWriter, r *http.Request) {
	var profile struct {
		Kind       string          `json:"kind"`
		APIVersion string          `json:"apiVersion"`
		Spec       json.RawMessage `json:"spec"`
	}
	if decode(r, &profile) != nil || profile.Kind != "ModelInvocationProfile" || profile.APIVersion != api.APIVersion {
		problem(w, http.StatusBadRequest, "Use a canonical invocation profile.")
		return
	}
	var spec api.ModelInvocationProfileSpec
	decoder := json.NewDecoder(bytes.NewReader(profile.Spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		problem(w, http.StatusBadRequest, "Unknown or invalid profile fields.")
		return
	}
	if err := spec.Validate(); err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	digest, err := modelinvocation.Digest(spec)
	if err != nil {
		problem(w, http.StatusBadRequest, "Profile digest is unavailable.")
		return
	}
	respond(w, http.StatusOK, map[string]string{"digest": digest})
}

func decode(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
