// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// ErrNotFound is returned when a resource does not exist.
var ErrNotFound = errors.New("resource not found")

// ErrConflict is returned on optimistic concurrency violation.
var ErrConflict = errors.New("resource version conflict")

// ErrSelfApproval is returned when the principal that initiated an action tries
// to decide that same action's RuntimeApproval.
var ErrSelfApproval = errors.New("initiator cannot decide its own RuntimeApproval")

// ErrAlreadyExists is returned when creating a resource that already exists.
var ErrAlreadyExists = errors.New("resource already exists")

// ErrQuotaExceeded is returned when an atomic reservation would exceed quota.
var ErrQuotaExceeded = errors.New("quota exceeded")

// WatchEvent represents a change to a stored resource.
type WatchEvent struct {
	Type            EventType
	Kind            string
	Namespace       string
	Name            string
	ResourceVersion int64
	Object          []byte // JSON-encoded resource
}

// EventType classifies a watch event.
type EventType string

const (
	EventCreated EventType = "ADDED"
	EventUpdated EventType = "MODIFIED"
	EventDeleted EventType = "DELETED"
	// EventResyncRequired terminates a lagging watch. Consumers must relist
	// current state before opening a replacement watch.
	EventResyncRequired EventType = "ERROR"
)

// ListOptions controls list/watch queries.
type ListOptions struct {
	Namespace       string
	LabelSelector   map[string]string
	ResourceVersion int64

	// Limit caps the number of results returned. Zero means no limit.
	Limit int

	// Continue is an opaque pagination token returned from a previous List call.
	// When set, the List starts after the position encoded in the token.
	Continue string
}

// ListResult wraps paginated list output.
type ListResult struct {
	Items    [][]byte
	Continue string // empty when there are no more results
}

// Store is the resource persistence interface.
type Store interface {
	// Ping verifies the store is reachable.
	Ping(ctx context.Context) error

	// Get retrieves a resource by kind, namespace, and name.
	// Returns ErrNotFound if the resource does not exist.
	Get(ctx context.Context, kind, namespace, name string) ([]byte, error)

	// List returns all resources matching the query.
	List(ctx context.Context, kind string, opts ListOptions) ([][]byte, error)

	// Create persists a new resource. Returns ErrAlreadyExists if it exists.
	Create(ctx context.Context, kind, namespace, name string, data []byte) error

	// Update modifies an existing resource with optimistic concurrency.
	// The resourceVersion must match the current version or ErrConflict is returned.
	Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error

	// Delete removes a resource. Returns ErrNotFound if it does not exist.
	Delete(ctx context.Context, kind, namespace, name string) error

	// Watch returns a channel of events for a resource kind starting after the given resourceVersion.
	Watch(ctx context.Context, kind string, opts ListOptions) (<-chan WatchEvent, error)

	// CreateRevision stores an immutable revision snapshot for a resource.
	CreateRevision(ctx context.Context, kind, namespace, name string, revision int64, spec, data []byte) error

	// ListRevisions returns all revisions for a resource, ordered by revision number.
	ListRevisions(ctx context.Context, kind, namespace, name string) ([]Revision, error)

	// GetRevision retrieves a specific revision of a resource.
	// Returns ErrNotFound if the revision does not exist.
	GetRevision(ctx context.Context, kind, namespace, name string, revision int64) (*Revision, error)

	// AppendEvent appends an execution event for a resource instance.
	AppendEvent(ctx context.Context, kind, namespace, name string, event InstanceEvent) error

	// ListEvents returns all events for a resource instance, ordered by timestamp ascending.
	ListEvents(ctx context.Context, kind, namespace, name string) ([]InstanceEvent, error)
}

// UpdateResultStore updates a resource and returns the representation produced
// by the final successful backend write. Backends that split spec and status
// updates may still partially persist changes before returning an error.
type UpdateResultStore interface {
	UpdateAndGet(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) ([]byte, error)
}

// CreateResultStore creates a generic resource and returns the representation
// containing the backend-assigned immutable UID and resource version.
type CreateResultStore interface {
	CreateAndGet(ctx context.Context, kind, namespace, name string, data []byte) ([]byte, error)
}

// ControlPlanePolicyStore performs policy mutations whose referential checks
// must be atomic with persistence.
type ControlPlanePolicyStore interface {
	CreateControlPlaneRoleBinding(ctx context.Context, namespace, name, roleName string, data []byte) ([]byte, error)
	UpdateControlPlaneRoleBinding(ctx context.Context, namespace, name, roleName string, data []byte, resourceVersion int64) ([]byte, error)
	DeleteControlPlaneRole(ctx context.Context, namespace, name string, resourceVersion int64) error
	DeleteControlPlaneRoleBinding(ctx context.Context, namespace, name string, resourceVersion int64) error
}

// RuntimeApprovalDecision carries an authenticated canonical actor. The
// authorization layer is deliberately outside the persistence adapter.
type RuntimeApprovalDecision struct {
	Decision                string
	ExpectedResourceVersion int64
	Issuer                  string
	Subject                 string
}

// RuntimeApprovalStore is the linearizable decision/replay foundation shared
// by standalone and Kubernetes modes.
type RuntimeApprovalStore interface {
	CreateOrGetRuntimeApproval(context.Context, v1alpha1.RuntimeApproval, json.RawMessage, time.Duration) (created bool, approval v1alpha1.RuntimeApproval, err error)
	GetRuntimeApprovalByInvocation(context.Context, string, string) (v1alpha1.RuntimeApproval, error)
	DecideRuntimeApproval(context.Context, string, string, RuntimeApprovalDecision) (v1alpha1.RuntimeApproval, error)
	ExpireRuntimeApprovalDecision(context.Context, string, string, int64) (v1alpha1.RuntimeApproval, error)
	AcknowledgeRuntimeApprovalDecisionAudit(context.Context, string, string, int64, string) (v1alpha1.RuntimeApproval, error)
}

// RuntimeApprovalCompactionStore replaces terminal approvals with bounded
// replay fences and retires those fences with their shared quota tickets.
type RuntimeApprovalCompactionStore interface {
	RuntimeApprovalStore
	CompactRuntimeApproval(context.Context, string, string, time.Duration) (bool, error)
	RetireRuntimeApprovalTombstones(context.Context, time.Time) (int64, error)
}

type RuntimeApprovalClaimantReader interface {
	VerifyRuntimeApprovalClaimant(context.Context, string, string, string, string, v1alpha1.RuntimeApprovalClaimant) error
}

type RuntimeApprovalTerminationReader interface {
	RuntimeApprovalTerminationProof(context.Context, v1alpha1.RuntimeApprovalClaimant) (string, error)
}

// RuntimeApprovalExecutionStore is the next lifecycle layer. Its explicit
// contract prevents gateways from implementing read-then-write claims outside
// the store abstraction.
type RuntimeApprovalExecutionStore interface {
	RuntimeApprovalStore
	PrepareRuntimeApproval(context.Context, string, string, int64, string, time.Time) (v1alpha1.RuntimeApproval, error)
	AbortRuntimeApprovalBeforeDispatch(context.Context, string, string, int64, string, string) (v1alpha1.RuntimeApproval, error)
	ClaimRuntimeApprovalExecution(context.Context, string, string, int64, string, v1alpha1.RuntimeApprovalClaimant) (v1alpha1.RuntimeApproval, error)
	MarkRuntimeApprovalClaimUncertain(context.Context, string, string, int64, int64, string) (v1alpha1.RuntimeApproval, error)
	FinalizeRuntimeApprovalExecution(context.Context, string, string, int64, int64, string) (v1alpha1.RuntimeApproval, error)
	ReconcileRuntimeApprovalExecution(context.Context, string, string, int64, string, string, string, string) (v1alpha1.RuntimeApproval, error)
	ReapRuntimeApprovalPreparation(context.Context, string, string, int64, string) (v1alpha1.RuntimeApproval, error)
}

// RuntimeApprovalRecoveryLease proves exclusive ownership of one stable
// standalone gateway workload identity. Implementations must retain that proof
// until Close returns or Done signals lock loss.
type RuntimeApprovalRecoveryLease interface {
	RecoverRuntimeApprovalClaim(context.Context, string, string, int64, int64) (v1alpha1.RuntimeApproval, error)
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}

// RuntimeApprovalRecoveryStore acquires process-lifetime standalone recovery
// authority. The workload identity must be stable across process restarts; the
// process-start nonce must be unique to the current process incarnation.
type RuntimeApprovalRecoveryStore interface {
	AcquireRuntimeApprovalRecoveryLease(context.Context, string, string) (RuntimeApprovalRecoveryLease, error)
}

// TokenReservation is a durable, pre-charged token reservation.
type TokenReservation struct {
	ID             string
	Namespace      string
	ReservedInput  int64
	ReservedOutput int64
}

// TokenReservationStore atomically persists token reservations with quota usage.
type TokenReservationStore interface {
	// CreateTokenReservation checks token budgets, charges the reservation, and
	// persists it atomically. created=false means an identical reservation exists.
	CreateTokenReservation(context.Context, TokenReservation) (created bool, err error)

	// SettleTokenReservation applies actual usage and removes the reservation
	// atomically. found=false means no reservation exists for the ID.
	SettleTokenReservation(context.Context, string, int64, int64) (found bool, err error)
}

// CanaryReservationDisposition is the permanent terminal accounting decision
// for a canary reservation.
type CanaryReservationDisposition struct {
	ReservationID string
	State         string
	InputTokens   int64
	OutputTokens  int64
}

const (
	CanaryReservationSettled  = "settled"
	CanaryReservationReleased = "released"
)

// CanaryReservationStore keeps active canary reservations out of generic
// expiry reclamation and records an immutable terminal disposition.
type CanaryReservationStore interface {
	CreateCanaryReservation(context.Context, TokenReservation) (created bool, err error)
	SettleCanaryReservation(context.Context, string, int64, int64) (CanaryReservationDisposition, error)
	ReleaseCanaryReservation(context.Context, string) (CanaryReservationDisposition, error)
}

// ExpiredTokenReservationStore reclaims abandoned durable reservations.
type ExpiredTokenReservationStore interface {
	ReapExpiredTokenReservations(context.Context, time.Time, int) (int, error)
}

// ToolCallReservation is a durable pre-dispatch charge for one exact Tool
// invocation preparation.
type ToolCallReservation struct {
	Namespace           string
	InvocationID        string
	Binding             string
	PreparationToken    string
	PreparationDeadline time.Time
}

// ToolCallReservationStore atomically charges one Tool call and persists its
// preparation fence. Identical retries resume without charging quota again.
type ToolCallReservationStore interface {
	PrepareToolCallReservation(context.Context, ToolCallReservation) (created bool, err error)
	SettleToolCallReservation(context.Context, string, string, string) (found bool, err error)
	ReleaseToolCallReservation(context.Context, string, string, string) (released bool, err error)
}

// ExpiredToolCallReservationStore discovers bounded expired Tool preparations
// for exact-token RuntimeApproval reaping.
type ExpiredToolCallReservationStore interface {
	ListExpiredToolCallReservations(context.Context, int) ([]ToolCallReservation, error)
}

// ToolGrantRateReservation identifies one atomic invocation charge against all
// configured rate windows for a persisted ToolGrant revision.
type ToolGrantRateReservation struct {
	Key            string
	Namespace      string
	Now            time.Time
	CallsPerMinute int64
	CallsPerHour   int64
	CallsPerDay    int64
}

// ToolGrantRateReservationStore atomically checks and consumes one invocation
// against a shared, durable ToolGrant rate counter.
type ToolGrantRateReservationStore interface {
	ReserveToolGrantInvocation(context.Context, ToolGrantRateReservation) error
}

// ToolInvocationReceiptStatus records whether a Tool invocation is executing
// or has reached a terminal outcome.
type ToolInvocationReceiptStatus string

const (
	ToolInvocationPending          ToolInvocationReceiptStatus = "Pending"
	ToolInvocationAwaitingApproval ToolInvocationReceiptStatus = "AwaitingApproval"
	ToolInvocationPrepared         ToolInvocationReceiptStatus = "Prepared"
	ToolInvocationSucceeded        ToolInvocationReceiptStatus = "Succeeded"
	ToolInvocationFailed           ToolInvocationReceiptStatus = "Failed"
)

// ToolInvocationReceipt durably binds one invocation ID to its complete Tool
// request and terminal result. Binding rejects a reused ID with changed input.
type ToolInvocationReceipt struct {
	Namespace           string
	InvocationID        string
	Binding             string
	Status              ToolInvocationReceiptStatus
	Output              json.RawMessage
	ExecutionMode       string
	DurationMS          int64
	ErrorMessage        string
	PreparationToken    string
	PreparationDeadline *time.Time
	CreatedAt           time.Time
	CompletedAt         *time.Time
}

// ToolInvocationReceiptStore provides a cross-replica, exactly-once claim for
// side-effecting Tool invocations. Claim returns claimed=false with the
// existing receipt when an identical invocation has already been observed.
type ToolInvocationReceiptStore interface {
	ClaimToolInvocation(context.Context, ToolInvocationReceipt) (claimed bool, receipt ToolInvocationReceipt, err error)
	PrepareToolInvocation(context.Context, ToolInvocationReceipt) (prepared bool, receipt ToolInvocationReceipt, err error)
	ReleasePreparedToolInvocation(context.Context, string, string, string) (released bool, err error)
	CompleteToolInvocation(context.Context, ToolInvocationReceipt) error
}

// MemoryMutationReceiptStatus records whether a Memory store/delete mutation
// is prepared or has reached a terminal outcome.
type MemoryMutationReceiptStatus string

const (
	MemoryMutationPrepared  MemoryMutationReceiptStatus = "Prepared"
	MemoryMutationSucceeded MemoryMutationReceiptStatus = "Succeeded"
	MemoryMutationFailed    MemoryMutationReceiptStatus = "Failed"
)

// MemoryMutationReceipt durably binds one invocation ID to an exact Memory
// store/delete request and its terminal result.
type MemoryMutationReceipt struct {
	Namespace           string
	InvocationID        string
	Binding             string
	Operation           string
	Status              MemoryMutationReceiptStatus
	Result              json.RawMessage
	ErrorMessage        string
	PreparationToken    string
	PreparationDeadline *time.Time
	CreatedAt           time.Time
	CompletedAt         *time.Time
}

// MemoryMutationReceiptStore persists the pre-claim receipt and first durable
// terminal outcome for Memory store/delete mutations.
type MemoryMutationReceiptStore interface {
	PrepareMemoryMutation(context.Context, MemoryMutationReceipt) (prepared bool, receipt MemoryMutationReceipt, err error)
	ReleasePreparedMemoryMutation(context.Context, string, string, string) (released bool, err error)
	CompleteMemoryMutation(context.Context, MemoryMutationReceipt) error
}

type MemoryMutationReceiptReader interface {
	GetMemoryMutation(context.Context, string, string) (MemoryMutationReceipt, bool, error)
}

// ExpiredMemoryMutationReceiptStore discovers bounded expired Memory
// preparations for exact-token RuntimeApproval reaping.
type ExpiredMemoryMutationReceiptStore interface {
	ListExpiredMemoryMutations(context.Context, int) ([]MemoryMutationReceipt, error)
}

// CanaryOperationPhase is a durable fence around one paid reasoning canary.
type CanaryOperationPhase string

const (
	CanaryClaimed           CanaryOperationPhase = "CanaryClaimed"
	CanaryProviderStarted   CanaryOperationPhase = "ProviderStarted"
	CanaryProviderCompleted CanaryOperationPhase = "ProviderCompleted"
	CanaryFinalizing        CanaryOperationPhase = "CanaryFinalizing"
	CanaryCompleted         CanaryOperationPhase = "CanaryCompleted"
)

// CanaryOperation binds an operation ID to one immutable request and its
// metadata-only result. Result must never contain prompts or provider output.
type CanaryOperation struct {
	OperationID          string
	RequestDigest        string
	Phase                CanaryOperationPhase
	Result               json.RawMessage
	UsageEvent           json.RawMessage
	AuditEvent           json.RawMessage
	ReservationCreated   bool
	ReservationFinalized bool
	UsageApplied         bool
	AuditDelivered       bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// CanaryTerminalRecord is the metadata-only terminal intent persisted before
// finalization effects run. AuditEvent retains the exact canonical bytes.
type CanaryTerminalRecord struct {
	Result             json.RawMessage
	UsageEvent         json.RawMessage
	AuditEvent         json.RawMessage
	ReservationCreated bool
}

// CanaryFinalizationStep identifies one idempotent terminal side effect.
type CanaryFinalizationStep string

const (
	CanaryReservationFinalized CanaryFinalizationStep = "reservationFinalized"
	CanaryUsageApplied         CanaryFinalizationStep = "usageApplied"
	CanaryAuditDelivered       CanaryFinalizationStep = "auditDelivered"
)

// CanaryOperationStore provides atomic ownership and replay fencing for paid
// reasoning canaries. A reused ID with a changed request digest conflicts.
type CanaryOperationStore interface {
	GetCanaryOperation(context.Context, string) (CanaryOperation, error)
	ClaimCanaryOperation(context.Context, CanaryOperation) (claimed bool, operation CanaryOperation, err error)
	TransitionCanaryOperation(context.Context, string, string, CanaryOperationPhase, CanaryOperationPhase, *CanaryTerminalRecord) (transitioned bool, operation CanaryOperation, err error)
	AcknowledgeCanaryFinalization(context.Context, string, string, CanaryFinalizationStep) (operation CanaryOperation, err error)
	CompactCanaryOperationsBefore(context.Context, time.Time) (int64, error)
}

// Revision represents an immutable snapshot of a resource at a point in time.
type Revision struct {
	Revision  int64  `json:"revision"`
	Spec      []byte `json:"spec"`
	Data      []byte `json:"data"`
	CreatedAt string `json:"createdAt"`
	CreatedBy string `json:"createdBy,omitempty"`
}

// InstanceEvent represents an execution event for an AgentInstance.
type InstanceEvent struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Reason    string          `json:"reason"`
	Message   string          `json:"message"`
	Details   json.RawMessage `json:"details,omitempty"`
}
