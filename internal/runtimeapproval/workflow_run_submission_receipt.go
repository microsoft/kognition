// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	authenticationv1 "k8s.io/api/authentication/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

const NativeWorkflowSubmissionDomain = "kognition.dev/workflow-submission/native/v1"
const NativeWorkflowSubmissionFormat = "kognition-native-workflow-submission-v1"
const NativeWorkflowSubmissionTTL = time.Minute
const NativeWorkflowSubmissionAnnotation = "kognition.dev/workflow-submission-receipt"
const NativeWorkflowExecutionBindingAnnotation = "kognition.dev/workflow-execution-binding"

type NativeWorkflowSubmissionReceiptStore interface {
	CreateNativeWorkflowSubmissionReceipt(context.Context, NativeWorkflowSubmissionReceipt) error
	GetNativeWorkflowSubmissionReceipt(context.Context, string, string) (NativeWorkflowSubmissionReceipt, error)
}

func NativeWorkflowSubmissionObjectName(workspace, runUID string) string {
	digest := sha256.Sum256([]byte(workspace + "\x00" + runUID))
	return "kognition-workflow-run-submission-" + fmt.Sprintf("%x", digest)
}

func WorkflowRunAcceptanceObjectName(workspace, runUID string) string {
	digest := sha256.Sum256([]byte(workspace + "\x00" + runUID))
	return "kognition-workflow-run-acceptance-" + fmt.Sprintf("%x", digest)
}

type NativeWorkflowSubmissionReceipt struct {
	AdmissionUID           string                       `json:"admissionUID"`
	Run                    api.RuntimeApprovalReference `json:"run"`
	Workflow               api.RuntimeApprovalReference `json:"workflow"`
	ExecutionBinding       api.RuntimeApprovalReference `json:"executionBinding"`
	WorkflowRevisionDigest string                       `json:"workflowRevisionDigest"`
	RequestDigest          string                       `json:"requestDigest"`
	Executor               api.ControlPlaneRoleSubject  `json:"executor"`
	Submitter              authenticationv1.UserInfo    `json:"submitter"`
	IssuedAt               time.Time                    `json:"issuedAt"`
	ExpiresAt              time.Time                    `json:"expiresAt"`
	Binding                api.RuntimeActionBinding     `json:"binding"`
}

func ValidateNativeWorkflowSubmissionReceipt(receipt NativeWorkflowSubmissionReceipt, now time.Time) error {
	run := receipt.Run
	run.ResourceVersion = 1
	validText := func(value string) bool {
		return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
	}
	if now.IsZero() || !validText(receipt.AdmissionUID) || receipt.Run.ResourceVersion != 0 ||
		executorRecordReference(run, "WorkflowRun", run.Namespace) != nil ||
		executorRecordReference(receipt.Workflow, "Workflow", run.Namespace) != nil ||
		executorRecordReference(receipt.ExecutionBinding, "WorkflowExecutionBinding", run.Namespace) != nil ||
		!validInstanceConfigurationDigest(receipt.WorkflowRevisionDigest) || !validInstanceConfigurationDigest(receipt.RequestDigest) ||
		executorRecordPrincipal(receipt.Executor) != nil || !validText(receipt.Submitter.Username) || receipt.Submitter.Username == "system:anonymous" ||
		receipt.Submitter.Username == receipt.Executor.Subject || receipt.Submitter.UID != "" && !validText(receipt.Submitter.UID) ||
		len(receipt.Submitter.Groups) > 64 || len(receipt.Submitter.Extra) > 16 || receipt.IssuedAt.IsZero() || receipt.IssuedAt.After(now) ||
		!receipt.ExpiresAt.After(now) || !receipt.ExpiresAt.After(receipt.IssuedAt) || receipt.ExpiresAt.Sub(receipt.IssuedAt) > NativeWorkflowSubmissionTTL {
		return fmt.Errorf("native submission receipt requires exact resource identities, authenticated submitter evidence, and bounded lifetime")
	}
	for _, group := range receipt.Submitter.Groups {
		if !validText(group) || group == "system:unauthenticated" {
			return fmt.Errorf("native submission receipt has invalid submitter groups")
		}
	}
	for key, values := range receipt.Submitter.Extra {
		if !validText(key) || len(values) > 16 {
			return fmt.Errorf("native submission receipt has invalid submitter extras")
		}
		for _, value := range values {
			if !validText(value) {
				return fmt.Errorf("native submission receipt has invalid submitter extras")
			}
		}
	}
	return nil
}

func nativeWorkflowSubmissionTag(key BindingKey, receipt NativeWorkflowSubmissionReceipt) (string, error) {
	receipt.Binding.Tag = ""
	raw, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	canonical, err := CanonicalizeJSON(raw)
	if err != nil || len(canonical) > WorkflowExecutorRecordMaxBytes {
		return "", fmt.Errorf("native submission receipt exceeds its content bound")
	}
	mac := hmac.New(sha256.New, key.Material)
	_, _ = mac.Write([]byte(NativeWorkflowSubmissionDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(canonical)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func SealNativeWorkflowSubmissionReceipt(ctx context.Context, keys BindingKeyStore, receipt NativeWorkflowSubmissionReceipt, now time.Time) (NativeWorkflowSubmissionReceipt, error) {
	if keys == nil {
		return NativeWorkflowSubmissionReceipt{}, ErrBindingKeyUnavailable
	}
	if err := ValidateNativeWorkflowSubmissionReceipt(receipt, now); err != nil {
		return NativeWorkflowSubmissionReceipt{}, err
	}
	key, _, err := ValidatedBindingKeys(ctx, keys)
	if err != nil {
		return NativeWorkflowSubmissionReceipt{}, err
	}
	receipt.Submitter = *receipt.Submitter.DeepCopy()
	receipt.Binding = api.RuntimeActionBinding{Format: NativeWorkflowSubmissionFormat, Domain: NativeWorkflowSubmissionDomain,
		Algorithm: BindingAlgorithmV1, Epoch: key.Epoch, KeyVersion: key.Version}
	receipt.Binding.Tag, err = nativeWorkflowSubmissionTag(key, receipt)
	if err != nil {
		return NativeWorkflowSubmissionReceipt{}, err
	}
	if err := VerifyNativeWorkflowSubmissionReceipt(ctx, keys, receipt, now); err != nil {
		return NativeWorkflowSubmissionReceipt{}, err
	}
	return receipt, nil
}

func VerifyNativeWorkflowSubmissionReceipt(ctx context.Context, keys BindingKeyStore, receipt NativeWorkflowSubmissionReceipt, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if keys == nil {
		return ErrBindingKeyUnavailable
	}
	if err := ValidateNativeWorkflowSubmissionReceipt(receipt, now); err != nil {
		return err
	}
	if receipt.Binding.Domain != NativeWorkflowSubmissionDomain || receipt.Binding.Format != NativeWorkflowSubmissionFormat ||
		receipt.Binding.Algorithm != BindingAlgorithmV1 || receipt.Binding.Epoch < 1 || receipt.Binding.KeyVersion < 1 {
		return fmt.Errorf("native submission receipt has an invalid authentication domain")
	}
	key, err := keys.ByVersion(ctx, receipt.Binding.KeyVersion)
	if err != nil {
		return err
	}
	if key.Version != receipt.Binding.KeyVersion || key.Epoch != receipt.Binding.Epoch || key.Domain != BindingDomainV1 || key.Algorithm != BindingAlgorithmV1 ||
		len(key.Material) < sha256.Size || key.Status != BindingKeyActive && key.Status != BindingKeyVerifyOnly {
		return ErrBindingKeyUnavailable
	}
	if err := validateBindingTag(receipt.Binding.Tag); err != nil {
		return err
	}
	tag, err := nativeWorkflowSubmissionTag(key, receipt)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(tag), []byte(receipt.Binding.Tag)) {
		return fmt.Errorf("native submission receipt signature conflict")
	}
	return ctx.Err()
}

func DecodeNativeWorkflowSubmissionReceipt(raw []byte) (NativeWorkflowSubmissionReceipt, error) {
	var receipt NativeWorkflowSubmissionReceipt
	if err := decodeWorkflowExecutorRecord(raw, &receipt); err != nil {
		return NativeWorkflowSubmissionReceipt{}, err
	}
	return receipt, nil
}
