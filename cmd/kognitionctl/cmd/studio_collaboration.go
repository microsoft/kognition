// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
)

// collabTextLimit bounds the goal and result text sent to the browser per task.
const collabTextLimit = 4000

// registerStudioCollaborationAPI wires the read-only collaboration trace of a
// WorkflowRun. The trace explains who took part in a collaboration step, which
// the step status alone does not.
func registerStudioCollaborationAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workflowruns/{name}/collaboration", handleStudioRunCollaboration)
}

type collabTrace struct {
	Steps []collabStep `json:"steps"`
}

type collabStep struct {
	Step      string `json:"step"`
	Attempt   int32  `json:"attempt"`
	Available bool   `json:"available"`
	// Message explains why the ledger is not available.
	Message  string `json:"message,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// FinalBy is the member alias whose task produced the step result.
	FinalBy    string           `json:"finalBy,omitempty"`
	Counters   collabCounters   `json:"counters"`
	Limits     collabLimits     `json:"limits"`
	Members    []collabMember   `json:"members"`
	Tasks      []collabTask     `json:"tasks"`
	Dispatches []collabDispatch `json:"dispatches"`
}

type collabCounters struct {
	Turns       int32 `json:"turns"`
	Messages    int32 `json:"messages"`
	Delegations int32 `json:"delegations"`
	TokensUsed  int64 `json:"tokensUsed"`
}

type collabLimits struct {
	MaxTurns        int32 `json:"maxTurns"`
	MaxDelegations  int32 `json:"maxDelegations"`
	MaxTokens       int64 `json:"maxTokens"`
	MaxContentBytes int64 `json:"maxContentBytes"`
}

type collabMember struct {
	Alias string `json:"alias"`
	Agent string `json:"agent,omitempty"`
	// Role is the member's part in the strategy, for example coordinator.
	Role string `json:"role"`
	// Involved reports whether the member received any task.
	Involved    bool  `json:"involved"`
	Tasks       int   `json:"tasks"`
	Turns       int32 `json:"turns"`
	Tokens      int64 `json:"tokens"`
	Contributed bool  `json:"contributed"`
	Final       bool  `json:"final"`
}

type collabText struct {
	Text      string `json:"text"`
	Chars     int    `json:"chars"`
	Truncated bool   `json:"truncated,omitempty"`
}

type collabTask struct {
	ID        string      `json:"id"`
	ParentID  string      `json:"parentId,omitempty"`
	Requester string      `json:"requester,omitempty"`
	Recipient string      `json:"recipient"`
	State     string      `json:"state"`
	Turns     int32       `json:"turns"`
	Final     bool        `json:"final,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Goal      collabText  `json:"goal"`
	Result    *collabText `json:"result,omitempty"`
}

type collabDispatch struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	TaskID    string     `json:"taskId"`
	Recipient string     `json:"recipient"`
	Turn      int32      `json:"turn,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	Job       *collabJob `json:"job,omitempty"`
}

type collabJob struct {
	Name        string `json:"name"`
	Agent       string `json:"agent,omitempty"`
	Phase       string `json:"phase,omitempty"`
	StartTime   string `json:"startTime,omitempty"`
	Completion  string `json:"completionTime,omitempty"`
	TotalTokens int64  `json:"totalTokens,omitempty"`
	Message     string `json:"message,omitempty"`
}

// collabRunStep is the part of a WorkflowRun the trace needs for one step.
type collabRunStep struct {
	name          string
	attempt       int32
	collaboration *v1alpha1.WorkflowCollaboration
}

func handleStudioRunCollaboration(w http.ResponseWriter, r *http.Request) {
	trace := collabTrace{Steps: []collabStep{}}
	// Collaboration steps run only in Kubernetes mode, so other modes have no trace.
	if !k8sMode || k8sCoreClient == nil {
		writeJSON(w, http.StatusOK, trace)
		return
	}
	ns := dashboardNamespace(r)
	name := r.PathValue("name")
	if ns == "" || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespace and run name are required"})
		return
	}
	raw, err := dashGet(ns, "workflowruns", name)
	if err != nil {
		status := http.StatusBadGateway
		if apierrors.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	uid, steps, err := collabRunSteps(raw)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	for _, step := range steps {
		trace.Steps = append(trace.Steps, loadCollabStep(r.Context(), ns, name, uid, step))
	}
	writeJSON(w, http.StatusOK, trace)
}

// collabRunSteps returns the run UID and the collaboration steps that have started.
func collabRunSteps(raw []byte) (string, []collabRunStep, error) {
	var run struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Status struct {
			StepStatuses []struct {
				Name     string `json:"name"`
				Attempts int32  `json:"attempts"`
			} `json:"stepStatuses"`
			SnapshotSpec struct {
				Steps []struct {
					Name          string                          `json:"name"`
					Type          string                          `json:"type"`
					Collaboration *v1alpha1.WorkflowCollaboration `json:"collaboration"`
				} `json:"steps"`
			} `json:"snapshotSpec"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return "", nil, fmt.Errorf("decode workflow run: %w", err)
	}
	attempts := map[string]int32{}
	for _, s := range run.Status.StepStatuses {
		attempts[s.Name] = s.Attempts
	}
	var out []collabRunStep
	for _, s := range run.Status.SnapshotSpec.Steps {
		if s.Collaboration == nil {
			continue
		}
		// A step without a status entry has not started, so it has no ledger.
		if attempt := attempts[s.Name]; attempt > 0 {
			out = append(out, collabRunStep{name: s.Name, attempt: attempt, collaboration: s.Collaboration})
		}
	}
	return run.Metadata.UID, out, nil
}

func loadCollabStep(ctx context.Context, ns, runName, runUID string, step collabRunStep) collabStep {
	out := collabStep{Step: step.name, Attempt: step.attempt}
	cm, err := k8sCoreClient.CoreV1().ConfigMaps(ns).Get(ctx, collaboration.LedgerName(ns, runUID, runName, step.name, step.attempt), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out.Message = "No collaboration record exists for this step attempt yet."
		return out
	case err != nil:
		out.Message = "Collaboration record could not be read: " + err.Error()
		return out
	}
	st, err := collaboration.Decode([]byte(cm.Data[collaboration.LedgerStateKey]))
	if err != nil {
		out.Message = "Collaboration record is unreadable: " + err.Error()
		return out
	}
	jobs := map[string]*collabJob{}
	for _, d := range st.Dispatches {
		if job := loadCollabJob(ns, collaboration.JobName(ns, runUID, step.name, step.attempt, d.ID)); job != nil {
			jobs[d.ID] = job
		}
	}
	return buildCollabStep(step, st, jobs)
}

func loadCollabJob(ns, name string) *collabJob {
	raw, err := k8sGet(ns, "agentjobs", name)
	if err != nil {
		return nil
	}
	var job struct {
		Spec struct {
			AgentRef struct {
				Name string `json:"name"`
			} `json:"agentRef"`
		} `json:"spec"`
		Status struct {
			Phase          string `json:"phase"`
			StartTime      string `json:"startTime"`
			CompletionTime string `json:"completionTime"`
			TokenUsage     struct {
				TotalTokens int64 `json:"totalTokens"`
			} `json:"tokenUsage"`
			Conditions []struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &job) != nil {
		return nil
	}
	out := &collabJob{
		Name: name, Agent: job.Spec.AgentRef.Name, Phase: job.Status.Phase,
		StartTime: job.Status.StartTime, Completion: job.Status.CompletionTime, TotalTokens: job.Status.TokenUsage.TotalTokens,
	}
	if out.Phase != "Ready" && out.Phase != "Succeeded" {
		for _, c := range job.Status.Conditions {
			if c.Status == "False" && c.Message != "" {
				out.Message = c.Message
				break
			}
		}
	}
	return out
}

// buildCollabStep summarizes a ledger. jobs maps a dispatch ID to its AgentJob.
func buildCollabStep(step collabRunStep, st collaboration.State, jobs map[string]*collabJob) collabStep {
	spec := step.collaboration
	out := collabStep{
		Step: step.name, Attempt: step.attempt, Available: true,
		Strategy: string(st.Strategy), Phase: string(st.Phase),
		Counters: collabCounters{Turns: st.Counters.Turns, Messages: st.Counters.Messages, Delegations: st.Counters.Delegations, TokensUsed: st.Counters.TokensUsed},
		Limits:   collabLimits{MaxTurns: spec.Limits.MaxTurns, MaxDelegations: spec.Limits.MaxDelegations, MaxTokens: spec.Limits.MaxTokens, MaxContentBytes: spec.Limits.MaxContentBytes},
		Members:  []collabMember{}, Tasks: []collabTask{}, Dispatches: []collabDispatch{},
	}
	if st.Outcome != nil {
		out.Reason = st.Outcome.Reason
	}

	contributed := map[string]bool{}
	if res := st.Result(); res != nil {
		for _, origin := range res.Origins {
			contributed[origin] = true
		}
	}
	for _, t := range st.Tasks {
		if t.Final && t.State == collaboration.TaskSucceeded {
			out.FinalBy = t.Recipient
		}
		task := collabTask{
			ID: t.ID, ParentID: t.ParentID, Requester: t.Requester, Recipient: t.Recipient,
			State: string(t.State), Turns: t.Turns, Final: t.Final, Reason: t.Reason, Goal: collabExcerpt(t.Goal.Text),
		}
		if t.Result != nil {
			result := collabExcerpt(t.Result.Text)
			task.Result = &result
		}
		out.Tasks = append(out.Tasks, task)
	}
	for _, d := range st.Dispatches {
		out.Dispatches = append(out.Dispatches, collabDispatch{
			ID: d.ID, Kind: string(d.Kind), TaskID: d.TaskID, Recipient: d.Recipient, Turn: d.Turn,
			Status: string(d.Status), Reason: d.Reason, Job: jobs[d.ID],
		})
	}
	for _, m := range spec.Members {
		member := collabMember{Alias: m.Name, Agent: m.AgentRef.Name, Role: collabRole(spec, m.Name)}
		if m.ExternalAgentRef != nil {
			member.Agent = m.ExternalAgentRef.Name
		}
		for _, t := range st.Tasks {
			if t.Recipient == m.Name {
				member.Tasks++
				member.Turns += t.Turns
			}
		}
		for _, d := range st.Dispatches {
			if d.Recipient == m.Name && jobs[d.ID] != nil {
				member.Tokens += jobs[d.ID].TotalTokens
			}
		}
		member.Involved = member.Tasks > 0
		member.Contributed = contributed[m.Name]
		member.Final = out.FinalBy == m.Name
		out.Members = append(out.Members, member)
	}
	return out
}

// collabRole names the part a member plays in the step's strategy.
func collabRole(spec *v1alpha1.WorkflowCollaboration, alias string) string {
	switch spec.Strategy {
	case v1alpha1.CollaborationStrategySupervisor:
		if spec.Supervisor != nil && spec.Supervisor.Coordinator == alias {
			return "coordinator"
		}
		return "specialist"
	case v1alpha1.CollaborationStrategyParallel:
		if spec.Parallel != nil {
			if spec.Parallel.Synthesizer == alias {
				return "synthesizer"
			}
			for _, b := range spec.Parallel.Branches {
				if b.Member == alias {
					return "branch"
				}
			}
		}
	case v1alpha1.CollaborationStrategyHandoff:
		if spec.Handoff != nil && spec.Handoff.EntryMember == alias {
			return "entry"
		}
	case v1alpha1.CollaborationStrategyPeer:
		if spec.Peer != nil && spec.Peer.Finalizer == alias {
			return "finalizer"
		}
	}
	return "member"
}

func collabExcerpt(s string) collabText {
	out := collabText{Text: s, Chars: utf8.RuneCountInString(s)}
	if out.Chars > collabTextLimit {
		out.Text = string([]rune(s)[:collabTextLimit])
		out.Truncated = true
	}
	return out
}
