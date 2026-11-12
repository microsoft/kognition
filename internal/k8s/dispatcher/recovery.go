// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/runtime/autogen"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/store"
)

type runtimeRecoverySnapshot struct {
	Version    int                      `json:"version"`
	Checkpoint []byte                   `json:"checkpoint"`
	Status     *autogen.HTTPAgentStatus `json:"status,omitempty"`
}

func (d *K8sDispatcher) ConfigureRuntimeRecoveryFile(storage store.RuntimeRecoveryStore, path string, retention time.Duration) error {
	if storage == nil || path == "" {
		return recovery.ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return recovery.ErrUnavailable
	}
	defer func() { _ = file.Close() }()
	configuration, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil || len(configuration) > 256<<10 {
		return recovery.ErrUnavailable
	}
	crypto, err := recovery.NewCipher(configuration)
	if err != nil {
		return err
	}
	if retention == 0 {
		retention = 7 * 24 * time.Hour
		if configured := os.Getenv("KOGNITION_RUNTIME_RECOVERY_RETENTION"); configured != "" {
			var err error
			retention, err = time.ParseDuration(configured)
			if err != nil {
				return recovery.ErrUnavailable
			}
		}
	}
	return d.SetRuntimeRecovery(storage, crypto, retention)
}

func (d *K8sDispatcher) currentRecoveryCipher() (*recovery.Cipher, error) {
	if d.config.RuntimeRecoveryKeysFile == "" {
		if d.recoveryCipher == nil {
			return nil, recovery.ErrUnavailable
		}
		return d.recoveryCipher, nil
	}
	file, err := os.Open(d.config.RuntimeRecoveryKeysFile)
	if err != nil {
		return nil, recovery.ErrUnavailable
	}
	defer func() { _ = file.Close() }()
	configuration, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if err != nil || len(configuration) > 256<<10 {
		return nil, recovery.ErrUnavailable
	}
	return recovery.NewCipher(configuration)
}

func (d *K8sDispatcher) SetRuntimeRecovery(storage store.RuntimeRecoveryStore, crypto *recovery.Cipher, retention time.Duration) error {
	if storage == nil || crypto == nil || retention <= 0 || retention > 30*24*time.Hour {
		return recovery.ErrUnavailable
	}
	d.recoveryStore, d.recoveryCipher, d.recoveryRetention = storage, crypto, retention
	return nil
}

func recoveryBinding(instance *v1alpha1.AgentInstance, agent *v1alpha1.Agent, sessionID, configuration string) recovery.Binding {
	return recovery.Binding{Workspace: instance.ObjectMeta.Namespace, AgentUID: agent.ObjectMeta.UID, InstanceUID: instance.ObjectMeta.UID, SessionID: sessionID, Generation: instance.Status.RestartCount, Runtime: "autogen/0.7.5", ConfigurationDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(configuration+"\x00"+instance.Spec.SessionContext["input"])))}
}

func recoveryBindingDigest(binding recovery.Binding) string {
	encoded, _ := json.Marshal(binding)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (d *K8sDispatcher) prepareAutoGenRecovery(ctx context.Context, session *autoGenSession, binding recovery.Binding) error {
	if d.recoveryStore == nil || d.recoveryCipher == nil {
		return recovery.ErrUnavailable
	}
	crypto, err := d.currentRecoveryCipher()
	if err != nil {
		return err
	}
	if _, err := d.recoveryStore.ExpireRuntimeRecovery(ctx, time.Now().UTC(), 100); err != nil {
		return err
	}
	stored, err := d.recoveryStore.GetRuntimeRecovery(ctx, binding.Workspace, binding.SessionID)
	if err == nil {
		if stored.BindingDigest != recoveryBindingDigest(binding) {
			return store.ErrConflict
		}
		if stored.Phase == store.RuntimeRecoveryClaimed || stored.Phase == store.RuntimeRecoveryRetired {
			return intctrl.ErrDispatchAccountingUncertain
		}
		plaintext, err := crypto.Open(binding, stored.Ciphertext, time.Now())
		if err != nil {
			return err
		}
		var snapshot runtimeRecoverySnapshot
		if json.Unmarshal(plaintext, &snapshot) != nil || snapshot.Version != 1 || len(snapshot.Checkpoint) == 0 {
			return recovery.ErrUnavailable
		}
		session.recoveryRecord, session.recoveryBinding = stored, binding
		session.preparedCheckpoint = snapshot.Checkpoint
		if stored.Phase == store.RuntimeRecoveryTerminal {
			if snapshot.Status == nil || snapshot.Status.Phase != "Completed" || snapshot.Status.TokenUsage == nil {
				return intctrl.ErrDispatchAccountingUncertain
			}
			session.terminalStatus = snapshot.Status
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	checkpoint, err := session.runtime.Checkpoint(ctx, binding.SessionID)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(runtimeRecoverySnapshot{Version: 1, Checkpoint: checkpoint.Token})
	if err != nil {
		return err
	}
	expiry := time.Now().UTC().Add(d.recoveryRetention)
	encrypted, err := crypto.Seal(binding, plaintext, expiry, time.Now())
	if err != nil {
		return err
	}
	stored, err = d.recoveryStore.PrepareRuntimeRecovery(ctx, store.RuntimeRecoveryRecord{Workspace: binding.Workspace, SessionID: binding.SessionID, BindingDigest: recoveryBindingDigest(binding), Ciphertext: encrypted, ExpiresAt: expiry})
	if err != nil {
		return err
	}
	if stored.Phase != store.RuntimeRecoveryPrepared {
		return intctrl.ErrDispatchAccountingUncertain
	}
	session.recoveryRecord, session.recoveryBinding = stored, binding
	session.preparedCheckpoint = checkpoint.Token
	return nil
}

func (d *K8sDispatcher) claimAutoGenRecovery(ctx context.Context, session *autoGenSession) error {
	record, err := d.recoveryStore.ClaimRuntimeRecovery(ctx, session.recoveryRecord.Workspace, session.recoveryRecord.SessionID, session.recoveryRecord.Version, uuid.NewString())
	if err != nil {
		return err
	}
	session.recoveryRecord = record
	return nil
}

func (d *K8sDispatcher) restoreAutoGenPrepared(ctx context.Context, session *autoGenSession) error {
	var checkpoint struct {
		State []byte `json:"state"`
	}
	if json.Unmarshal(session.preparedCheckpoint, &checkpoint) != nil || len(checkpoint.State) == 0 {
		return recovery.ErrUnavailable
	}
	_, err := session.client.RestoreAgent(ctx, checkpoint.State)
	return err
}

func (d *K8sDispatcher) completeAutoGenRecovery(ctx context.Context, session *autoGenSession) error {
	crypto, err := d.currentRecoveryCipher()
	if err != nil {
		return err
	}
	status, err := session.client.Status(ctx, session.recoveryBinding.SessionID)
	if err != nil || status.Phase != "Completed" || status.RuntimeApprovalState != "" || status.TokenUsage == nil {
		return intctrl.ErrDispatchAccountingUncertain
	}
	checkpoint, err := session.runtime.Checkpoint(ctx, session.recoveryBinding.SessionID)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(runtimeRecoverySnapshot{Version: 1, Checkpoint: checkpoint.Token, Status: &status})
	if err != nil {
		return err
	}
	record := session.recoveryRecord
	record.ExpiresAt = time.Now().UTC().Add(d.recoveryRetention)
	record.Ciphertext, err = crypto.Seal(session.recoveryBinding, plaintext, record.ExpiresAt, time.Now())
	if err != nil {
		return err
	}
	if err := d.recoveryStore.CompleteRuntimeRecovery(ctx, record); err != nil {
		return err
	}
	session.mu.Lock()
	session.terminalStatus = &status
	session.mu.Unlock()
	_ = session.client.ReleaseAgent(ctx, session.recoveryBinding.SessionID)
	d.autogenSessions.CompareAndDelete(session.recoveryBinding.SessionID, session)
	return nil
}

func (d *K8sDispatcher) recoverAutoGenTerminal(ctx context.Context, instance *v1alpha1.AgentInstance, agent *v1alpha1.Agent) (intctrl.RuntimeStatus, error) {
	uncertain := func() (intctrl.RuntimeStatus, error) {
		return intctrl.RuntimeStatus{}, intctrl.ErrDispatchAccountingUncertain
	}
	if d.recoveryStore == nil || d.recoveryCipher == nil {
		return uncertain()
	}
	crypto, err := d.currentRecoveryCipher()
	if err != nil {
		return uncertain()
	}
	model, err := getResource[v1alpha1.Model](ctx, d.store, "Model", instance.ObjectMeta.Namespace, agent.Spec.ModelRef.Name)
	if err != nil || model == nil {
		return uncertain()
	}
	configuration, err := d.externalConfiguration(ctx, "", instance.ObjectMeta.Namespace, agent, model)
	if err != nil {
		return uncertain()
	}
	binding := recoveryBinding(instance, agent, instance.Status.SessionID, configuration)
	record, err := d.recoveryStore.GetRuntimeRecovery(ctx, binding.Workspace, binding.SessionID)
	if err != nil || record.BindingDigest != recoveryBindingDigest(binding) || record.Phase != store.RuntimeRecoveryTerminal {
		return uncertain()
	}
	plaintext, err := crypto.Open(binding, record.Ciphertext, time.Now())
	if err != nil {
		return uncertain()
	}
	var snapshot runtimeRecoverySnapshot
	if json.Unmarshal(plaintext, &snapshot) != nil || snapshot.Version != 1 || snapshot.Status == nil || len(snapshot.Checkpoint) == 0 {
		return uncertain()
	}
	status := snapshot.Status
	if status.Phase != "Completed" || status.RuntimeApprovalState != "" || status.SessionID != binding.SessionID || status.InstanceUID != binding.InstanceUID || status.AgentName != agent.ObjectMeta.Name || status.TokenUsage == nil {
		return uncertain()
	}
	result := intctrl.RuntimeStatus{Alive: true, Completed: true, Output: status.Output, ToolCalls: status.ToolCalls, TokenUsage: tokenUsageFromAutoGen(*status)}
	var details []map[string]any
	encoded, err := json.Marshal(status.ToolCallsDetail)
	if err != nil || json.Unmarshal(encoded, &details) != nil {
		return uncertain()
	}
	result.ToolCallsDetail = toolCallDetailsFromRuntime(details)
	return result, nil
}
