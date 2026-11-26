// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

// countingStore counts the reads a check makes and can fail them.
type countingStore struct {
	*callHealthStore
	modelLists   atomic.Int32
	classGets    atomic.Int32
	secretGets   atomic.Int32
	listErr      error
	classGetErrs []error
}

func (s *countingStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == modelKind {
		s.modelLists.Add(1)
		if s.listErr != nil {
			return nil, s.listErr
		}
	}
	return s.callHealthStore.List(ctx, kind, opts)
}

func (s *countingStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == "SecretRef" {
		s.secretGets.Add(1)
	}
	if kind == "ModelClass" {
		call := int(s.classGets.Add(1)) - 1
		if call < len(s.classGetErrs) && s.classGetErrs[call] != nil {
			return nil, s.classGetErrs[call]
		}
	}
	return s.callHealthStore.Get(ctx, kind, namespace, name)
}

type enqueued struct {
	mu   sync.Mutex
	keys []string
}

func (e *enqueued) add(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keys = append(e.keys, key)
}

func (e *enqueued) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.keys...)
}

func newWatcherFixture(t *testing.T) (*ModelHealthWatcher, *countingStore, *enqueued) {
	t.Helper()
	backing := &countingStore{callHealthStore: newCallHealthStore()}
	queue := &enqueued{}
	return NewModelHealthWatcher(backing, backing, queue.add, zaptest.NewLogger(t)), backing, queue
}

func markPassivelyDegraded(model v1alpha1.Model, reason string) v1alpha1.Model {
	model.Status.Phase = v1alpha1.PhaseFailed
	model.Status.Conditions = []v1alpha1.Condition{{
		Type: "Ready", Status: "False", Reason: reason,
		Message: modelhealth.MessagePrefix + "recent model calls are failing with " + reason,
	}}
	return model
}

func TestModelHealthWatcherQueuesModelsWhoseTargetStartedFailing(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	storeModel(t, backing.memStore, newPassiveModel("failing", "anthropic"))
	healthy := newPassiveModel("healthy", "anthropic")
	healthy.Spec.ModelID = "another-model"
	storeModel(t, backing.memStore, healthy)
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if got := queue.all(); len(got) != 1 || got[0] != "default/failing" {
		t.Fatalf("queued %v, want only default/failing", got)
	}
}

func TestModelHealthWatcherDoesNotRequeueAModelThatAlreadyReflectsTheEvidence(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	storeModel(t, backing.memStore, markPassivelyDegraded(newPassiveModel("failing", "anthropic"), cpi.ReasonRateLimited))
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 0 {
		t.Fatalf("queued %v, want nothing while the status already matches", got)
	}
}

func TestModelHealthWatcherQueuesRecoveriesAndReasonChanges(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	target := passiveTarget("anthropic")

	recovering := markPassivelyDegraded(newPassiveModel("recovering", "anthropic"), cpi.ReasonTimeout)
	storeModel(t, backing.memStore, recovering)
	backing.set([]store.ModelCallHealth{recoveryRecord(target, 3)}, nil)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 1 || got[0] != "default/recovering" {
		t.Fatalf("queued %v, want the recovered model", got)
	}

	// The same model marked for one reason is requeued when the evidence changes it.
	backing.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonAuthenticationFailed)}, nil)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 2 || got[1] != "default/recovering" {
		t.Fatalf("queued %v, want the model queued again for the new reason", got)
	}
}

func TestModelHealthWatcherRecoversModelsWhenAllEvidenceAgedOut(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	storeModel(t, backing.memStore, markPassivelyDegraded(newPassiveModel("degraded", "anthropic"), cpi.ReasonTimeout))
	backing.set(nil, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 1 || got[0] != "default/degraded" {
		t.Fatalf("queued %v, want the degraded model queued for recovery even without evidence", got)
	}
}

func TestModelHealthWatcherSkipsListingModelsWhenIdle(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	storeModel(t, backing.memStore, newPassiveModel("fine", "anthropic"))
	backing.set(nil, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backing.modelLists.Load() != 1 {
		t.Fatalf("first check listed models %d times, want 1 because a predecessor may have marked some", backing.modelLists.Load())
	}
	for i := 0; i < 3; i++ {
		if err := watcher.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if backing.modelLists.Load() != 1 {
		t.Fatalf("idle checks listed models %d times, want them skipped", backing.modelLists.Load())
	}

	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backing.modelLists.Load() != 2 || len(queue.all()) != 1 {
		t.Fatalf("lists=%d queued=%v, want new evidence to resume watching", backing.modelLists.Load(), queue.all())
	}
}

func TestModelHealthWatcherKeepsWatchingWhileAModelIsDegraded(t *testing.T) {
	watcher, backing, _ := newWatcherFixture(t)
	storeModel(t, backing.memStore, markPassivelyDegraded(newPassiveModel("degraded", "anthropic"), cpi.ReasonRateLimited))
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)

	for i := 0; i < 3; i++ {
		if err := watcher.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if backing.modelLists.Load() != 3 {
		t.Fatalf("listed models %d times, want every check while a model is degraded", backing.modelLists.Load())
	}
	backing.set(nil, nil)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !watcher.mayHaveDegraded {
		t.Fatal("a model that is still marked degraded keeps the watcher active until it is reconciled")
	}
}

func TestModelHealthWatcherReadsEachModelClassOncePerCheck(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	storeModelClass(t, backing.memStore, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "shared"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "anthropic"},
	})
	for _, name := range []string{"a", "b", "c"} {
		model := newPassiveModel(name, "")
		model.Spec.ClassName = "shared"
		storeModel(t, backing.memStore, model)
	}
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backing.classGets.Load() != 1 {
		t.Fatalf("ModelClass read %d times, want once for three models", backing.classGets.Load())
	}
	if len(queue.all()) != 3 {
		t.Fatalf("queued %v, want all three models sharing the failing target", queue.all())
	}
}

func TestModelHealthWatcherSkipsModelsItCannotEvaluate(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	missingClass := newPassiveModel("missing-class", "")
	missingClass.Spec.ClassName = "does-not-exist"
	storeModel(t, backing.memStore, missingClass)
	storeModel(t, backing.memStore, markPassivelyDegraded(func() v1alpha1.Model {
		m := newPassiveModel("marked-missing-class", "")
		m.Spec.ClassName = "does-not-exist"
		return m
	}(), cpi.ReasonTimeout))
	terminated := newPassiveModel("terminated", "anthropic")
	terminated.Status.Phase = v1alpha1.PhaseTerminated
	storeModel(t, backing.memStore, terminated)
	if err := backing.Create(context.Background(), modelKind, "default", "garbled", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if got := queue.all(); len(got) != 0 {
		t.Fatalf("queued %v, want unresolvable, terminated and undecodable models skipped", got)
	}
	if !watcher.mayHaveDegraded {
		t.Fatal("a marked model whose provider cannot be resolved must keep the watcher active")
	}
}

func TestModelHealthWatcherReportsStoreFailures(t *testing.T) {
	watcher, backing, _ := newWatcherFixture(t)
	backing.set(nil, errors.New("health store unavailable"))
	if err := watcher.Check(context.Background()); err == nil {
		t.Error("Check() must report a failure to read passive health")
	}

	backing.set([]store.ModelCallHealth{faultRecord("target", cpi.ReasonTimeout)}, nil)
	backing.listErr = errors.New("model store unavailable")
	if err := watcher.Check(context.Background()); err == nil {
		t.Error("Check() must report a failure to list Models")
	}
}

func TestModelHealthWatcherStartChecksPeriodicallyUntilCanceled(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	watcher.interval = 5 * time.Millisecond
	storeModel(t, backing.memStore, newPassiveModel("failing", "anthropic"))
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watcher.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(queue.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(queue.all()) == 0 {
		t.Fatal("Start() never queued the failing model")
	}

	// A failing check is logged and does not stop the watcher.
	backing.set(nil, errors.New("health store unavailable"))
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start() error = %v, want nil on cancellation", err)
	}
}

func TestModelClassCacheCachesSuccessesAndNotFoundButRetriesOtherErrors(t *testing.T) {
	backing := &countingStore{callHealthStore: newCallHealthStore(), classGetErrs: []error{errors.New("transient"), nil}}
	storeModelClass(t, backing.memStore, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "class"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "anthropic"},
	})
	cache := &modelClassCache{Store: backing, cache: make(map[string]modelClassLookup)}
	ctx := context.Background()

	if _, err := cache.Get(ctx, "ModelClass", "", "class"); err == nil {
		t.Fatal("the transient failure must be returned")
	}
	for i := 0; i < 3; i++ {
		if _, err := cache.Get(ctx, "ModelClass", "", "class"); err != nil {
			t.Fatalf("Get() error: %v", err)
		}
	}
	if backing.classGets.Load() != 2 {
		t.Fatalf("store read %d times, want the transient failure retried once and the success cached", backing.classGets.Load())
	}

	for i := 0; i < 3; i++ {
		if _, err := cache.Get(ctx, "ModelClass", "", "absent"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get(absent) error = %v, want not found", err)
		}
	}
	if backing.classGets.Load() != 3 {
		t.Fatalf("store read %d times, want the not-found result cached", backing.classGets.Load())
	}

	storeModel(t, backing.memStore, newPassiveModel("a-model", "anthropic"))
	if _, err := cache.Get(ctx, modelKind, "default", "a-model"); err != nil {
		t.Fatalf("other kinds must pass through: %v", err)
	}
}

// watcherClock is the watcher's clock in tests.
type watcherClock struct{ now time.Time }

func (c *watcherClock) Now() time.Time          { return c.now }
func (c *watcherClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newBackoffWatcher(t *testing.T) (*ModelHealthWatcher, *countingStore, *enqueued, *watcherClock) {
	t.Helper()
	watcher, backing, queue := newWatcherFixture(t)
	clock := &watcherClock{now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	watcher.now = clock.Now
	return watcher, backing, queue, clock
}

// replaceModel stores model in place of the stored one, like a reconcile that
// changed its status.
func replaceModel(t *testing.T, s *callHealthStore, model v1alpha1.Model) {
	t.Helper()
	if err := s.memStore.Delete(context.Background(), modelKind, model.ObjectMeta.Namespace, model.ObjectMeta.Name); err != nil {
		t.Fatal(err)
	}
	storeModel(t, s.memStore, model)
}

// markActivelyFailing gives the Model a Ready=False condition that has nothing to
// do with passive health, like a failing probe.
func markActivelyFailing(model v1alpha1.Model) v1alpha1.Model {
	model.Status.Phase = v1alpha1.PhaseFailed
	model.Status.Conditions = []v1alpha1.Condition{{Type: "Ready", Status: "False", Reason: "EndpointUnreachable", Message: "dial tcp: connection refused"}}
	return model
}

func markReady(model v1alpha1.Model) v1alpha1.Model {
	model.Status.Phase = v1alpha1.PhaseReady
	model.Status.Conditions = []v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: "Healthy", Message: "probe passed"}}
	return model
}

func TestModelHealthWatcherDoesNotQueueAModelWhoseActiveCheckFails(t *testing.T) {
	watcher, backing, queue, clock := newBackoffWatcher(t)
	storeModel(t, backing.memStore, markActivelyFailing(newPassiveModel("unreachable", "anthropic")))
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)

	// Far longer than the backoff: the Model must not be queued at all, since a
	// reconcile keeps its active failure and cannot apply the passive verdict.
	for i := 0; i < 20; i++ {
		if err := watcher.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.advance(DefaultModelHealthWatchInterval)
	}
	if got := queue.all(); len(got) != 0 {
		t.Fatalf("queued %v, want a Model with an active failure never queued for passive degradation", got)
	}
	if !watcher.mayHaveDegraded {
		t.Fatal("the watcher must keep watching while the evidence says the target is failing")
	}

	// Once the active failure is gone, the evidence applies and the Model is queued.
	replaceModel(t, backing.callHealthStore, markReady(newPassiveModel("unreachable", "anthropic")))
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 1 || got[0] != "default/unreachable" {
		t.Fatalf("queued %v, want the Model queued once its active check passes", got)
	}
}

func TestModelHealthWatcherQueuesTheSameOutcomeOnlyOncePerBackoff(t *testing.T) {
	watcher, backing, queue, clock := newBackoffWatcher(t)
	storeModel(t, backing.memStore, markReady(newPassiveModel("slow", "anthropic")))
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)
	check := func() {
		t.Helper()
		if err := watcher.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// The reconcile that was queued never changes the status, as when the probe
	// is still running or fails to be written. Checking every interval for two
	// backoff periods queues the Model once per period: now, after one, after two.
	for elapsed := time.Duration(0); elapsed <= 2*DefaultModelHealthRequeueBackoff; elapsed += DefaultModelHealthWatchInterval {
		check()
		clock.advance(DefaultModelHealthWatchInterval)
	}
	if got := queue.all(); len(got) != 3 {
		t.Fatalf("queued %d times over two backoff periods, want once per period: %v", len(got), got)
	}
}
func TestModelHealthWatcherQueuesAChangedOutcomeAtOnceDespiteTheBackoff(t *testing.T) {
	watcher, backing, queue, clock := newBackoffWatcher(t)
	target := passiveTarget("anthropic")
	storeModel(t, backing.memStore, markReady(newPassiveModel("flaky", "anthropic")))
	backing.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonTimeout)}, nil)
	check := func() {
		t.Helper()
		if err := watcher.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	check() // queued for degradation
	// The reconcile degrades it, and the provider recovers 15 seconds later.
	replaceModel(t, backing.callHealthStore, markPassivelyDegraded(newPassiveModel("flaky", "anthropic"), cpi.ReasonTimeout))
	backing.set([]store.ModelCallHealth{recoveryRecord(target, 3)}, nil)
	clock.advance(DefaultModelHealthWatchInterval)
	check()
	if got := queue.all(); len(got) != 2 {
		t.Fatalf("queued %v, want the recovery queued at once, not held back by the earlier degradation", got)
	}

	// A degradation for another reason of a Model already degraded is also a change.
	replaceModel(t, backing.callHealthStore, markPassivelyDegraded(newPassiveModel("flaky", "anthropic"), cpi.ReasonTimeout))
	backing.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonAuthenticationFailed)}, nil)
	clock.advance(DefaultModelHealthWatchInterval)
	check()
	clock.advance(DefaultModelHealthWatchInterval)
	check()
	if got := queue.all(); len(got) != 3 {
		t.Fatalf("queued %v, want the new reason queued once and then held for the backoff", got)
	}
}

func TestModelHealthWatcherForgetsModelsThatNeedNoMoreAttention(t *testing.T) {
	watcher, backing, queue, clock := newBackoffWatcher(t)
	target := passiveTarget("anthropic")
	storeModel(t, backing.memStore, markReady(newPassiveModel("transient", "anthropic")))
	backing.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonTimeout)}, nil)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(watcher.queued) != 1 {
		t.Fatalf("remembered %d queued Models, want 1", len(watcher.queued))
	}

	// The status now reflects the evidence: nothing is pending.
	replaceModel(t, backing.callHealthStore, markPassivelyDegraded(newPassiveModel("transient", "anthropic"), cpi.ReasonTimeout))
	clock.advance(DefaultModelHealthWatchInterval)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(watcher.queued) != 0 {
		t.Fatalf("remembered %v, want a Model whose status matches the evidence forgotten", watcher.queued)
	}

	// A new degradation of the same Model is queued at once, with no stale backoff.
	replaceModel(t, backing.callHealthStore, markReady(newPassiveModel("transient", "anthropic")))
	clock.advance(DefaultModelHealthWatchInterval)
	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 2 {
		t.Fatalf("queued %v, want the Model queued again as soon as it needs it", got)
	}

	// A deleted Model is forgotten too, and an idle watcher keeps nothing.
	if err := backing.memStore.Delete(context.Background(), modelKind, "default", "transient"); err != nil {
		t.Fatal(err)
	}
	backing.set(nil, nil)
	for i := 0; i < 2; i++ {
		clock.advance(DefaultModelHealthWatchInterval)
		if err := watcher.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(watcher.queued) != 0 || watcher.mayHaveDegraded {
		t.Fatalf("remembered %v, mayHaveDegraded=%t, want an idle watcher to hold nothing", watcher.queued, watcher.mayHaveDegraded)
	}
}
