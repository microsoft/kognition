// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package callhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kognition-ai/kognition/internal/store"
)

const testNamespace = "kognition-system"

var epoch = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type clock struct{ now atomic.Pointer[time.Time] }

func newClock() *clock {
	c := &clock{}
	c.set(epoch)
	return c
}

func (c *clock) Now() time.Time          { return *c.now.Load() }
func (c *clock) set(t time.Time)         { c.now.Store(&t) }
func (c *clock) advance(d time.Duration) { c.set(c.Now().Add(d)) }

func newFakeClient(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()
}

func newTestStore(t *testing.T, kubernetesClient client.Client, component string, options ...Option) (*ConfigMapStore, *clock) {
	t.Helper()
	s, err := NewConfigMapStore(kubernetesClient, testNamespace, component, options...)
	if err != nil {
		t.Fatalf("NewConfigMapStore() error: %v", err)
	}
	c := newClock()
	s.now = c.Now
	return s, c
}

func healthConfigMap(namespace, reporter, component string, updatedAt time.Time, records ...record) *corev1.ConfigMap {
	encoded, _ := json.Marshal(payload{Reporter: reporter, UpdatedAt: updatedAt, Records: records})
	name := objectName(component, reporter)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name), Labels: map[string]string{HealthLabel: component}, CreationTimestamp: metav1.NewTime(updatedAt)},
		Data:       map[string]string{DataKey: string(encoded)},
	}
}

func rawConfigMap(name string, labels, data map[string]string) *corev1.ConfigMap {
	if component := labels[HealthLabel]; component != "" {
		name = objectName(component, name)
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, Labels: labels}, Data: data}
}

func listAll(t *testing.T, kubernetesClient client.Client) []corev1.ConfigMap {
	t.Helper()
	var configMaps corev1.ConfigMapList
	if err := kubernetesClient.List(context.Background(), &configMaps, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	return configMaps.Items
}

func TestNewConfigMapStoreValidatesItsArguments(t *testing.T) {
	kubernetesClient := newFakeClient(t, interceptor.Funcs{})
	if _, err := NewConfigMapStore(nil, testNamespace, "controller"); err == nil {
		t.Error("a missing client must be rejected")
	}
	if _, err := NewConfigMapStore(kubernetesClient, "", "controller"); err == nil {
		t.Error("a missing namespace must be rejected")
	}
	for _, component := range []string{"", "Controller", "model_gateway", "-controller", "controller-", "two--dashes", "has space", strings.Repeat("a", 33)} {
		if _, err := NewConfigMapStore(kubernetesClient, testNamespace, component); err == nil {
			t.Errorf("component %q must be rejected", component)
		}
	}
	for _, component := range []string{"controller", "model-gateway", strings.Repeat("a", 32)} {
		if _, err := NewConfigMapStore(kubernetesClient, testNamespace, component, WithLogger(zap.NewNop())); err != nil {
			t.Errorf("component %q must be accepted: %v", component, err)
		}
	}
}

func TestConfigMapStoreRoundTripsRecordsFromEveryReporter(t *testing.T) {
	kubernetesClient := newFakeClient(t, interceptor.Funcs{})
	controller, clk := newTestStore(t, kubernetesClient, "controller")
	gateway, _ := newTestStore(t, kubernetesClient, "model-gateway")
	gateway.now = clk.Now
	reader, _ := newTestStore(t, kubernetesClient, "controller")
	reader.now = clk.Now
	ctx := context.Background()

	faultAt, successAt := epoch.Add(-time.Second), epoch.Add(-2*time.Second)
	if err := controller.ReplaceModelCallHealth(ctx, "controller-pod", []store.ModelCallHealth{
		{Target: "a", Calls: 5, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: "RateLimited", LastFaultAt: faultAt, LastSuccessAt: successAt},
	}); err != nil {
		t.Fatalf("controller Replace() error: %v", err)
	}
	if err := gateway.ReplaceModelCallHealth(ctx, "gateway-pod", []store.ModelCallHealth{{Target: "a", Calls: 2, ConsecutiveSuccesses: 2, LastSuccessAt: epoch}, {Target: "b", Calls: 1}}); err != nil {
		t.Fatalf("gateway Replace() error: %v", err)
	}

	records, err := reader.ListModelCallHealth(ctx, time.Minute)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	byKey := map[string]store.ModelCallHealth{}
	for _, record := range records {
		byKey[fmt.Sprintf("%s/%d", record.Target, record.Calls)] = record
	}
	if len(records) != 3 {
		t.Fatalf("listed %d records, want 3 (a from each reporter and b): %+v", len(records), records)
	}
	faulty := byKey["a/5"]
	if faulty.Faults != 3 || faulty.ConsecutiveFaults != 3 || faulty.LastFaultReason != "RateLimited" || !faulty.LastFaultAt.Equal(faultAt) || !faulty.LastSuccessAt.Equal(successAt) {
		t.Errorf("controller record = %+v, want the published values", faulty)
	}
	healthy := byKey["a/2"]
	if healthy.ConsecutiveSuccesses != 2 || !healthy.LastFaultAt.IsZero() || healthy.LastFaultReason != "" || !healthy.LastSuccessAt.Equal(epoch) {
		t.Errorf("gateway record = %+v, want a success-only record with no fault time", healthy)
	}
}

func TestReplaceKeepsOneConfigMapPerReporterAndRemovesRetiredTargets(t *testing.T) {
	kubernetesClient := newFakeClient(t, interceptor.Funcs{})
	s, _ := newTestStore(t, kubernetesClient, "controller")
	ctx := context.Background()
	reporter := "reporter-with-a-host-and-uuid/0d9e1c52-0a8e-49a6-9e61-1c5e8f3f4e1b"

	for _, records := range [][]store.ModelCallHealth{
		{{Target: "one", Calls: 1}, {Target: "two", Calls: 1}},
		{{Target: "one", Calls: 4}},
		nil,
	} {
		if err := s.ReplaceModelCallHealth(ctx, reporter, records); err != nil {
			t.Fatalf("Replace() error: %v", err)
		}
		configMaps := listAll(t, kubernetesClient)
		if len(configMaps) != 1 {
			t.Fatalf("found %d ConfigMaps, want one per reporter", len(configMaps))
		}
		configMap := configMaps[0]
		if !strings.HasPrefix(configMap.Name, NamePrefix+"controller-") || len(configMap.Name) != len(NamePrefix+"controller-")+32 {
			t.Errorf("name = %q, want %scontroller-<32 hex characters>", configMap.Name, NamePrefix)
		}
		if strings.Contains(configMap.Name, "uuid") {
			t.Errorf("name %q leaks the reporter identity", configMap.Name)
		}
		if len(configMap.Labels) != 1 || configMap.Labels[HealthLabel] != "controller" || len(configMap.Annotations) != 0 || len(configMap.BinaryData) != 0 || len(configMap.Data) != 1 {
			t.Errorf("shape = labels %v annotations %v data keys %d binary %d, want exactly one label and one data key", configMap.Labels, configMap.Annotations, len(configMap.Data), len(configMap.BinaryData))
		}
		if len(configMap.OwnerReferences) != 0 || len(configMap.Finalizers) != 0 {
			t.Error("health ConfigMaps must carry no owner references or finalizers")
		}
	}

	got, err := s.ListModelCallHealth(ctx, time.Minute)
	if err != nil || len(got) != 0 {
		t.Fatalf("after publishing nothing: %+v, %v, want no records", got, err)
	}
}

func TestReplaceRejectsInvalidPublicationsWithoutWriting(t *testing.T) {
	kubernetesClient := newFakeClient(t, interceptor.Funcs{})
	s, _ := newTestStore(t, kubernetesClient, "controller")
	ctx := context.Background()
	for name, test := range map[string]struct {
		reporter string
		records  []store.ModelCallHealth
	}{
		"no reporter":    {"", []store.ModelCallHealth{{Target: "t", Calls: 1}}},
		"empty target":   {"r", []store.ModelCallHealth{{Calls: 1}}},
		"faults > calls": {"r", []store.ModelCallHealth{{Target: "t", Calls: 1, Faults: 2}}},
	} {
		if err := s.ReplaceModelCallHealth(ctx, test.reporter, test.records); err == nil {
			t.Errorf("%s: Replace() accepted invalid input", name)
		}
	}
	if got := listAll(t, kubernetesClient); len(got) != 0 {
		t.Fatalf("invalid input created %d ConfigMaps", len(got))
	}
}

func TestReplaceNeverTakesOverAConfigMapItDoesNotOwn(t *testing.T) {
	probe, _ := newTestStore(t, newFakeClient(t, interceptor.Funcs{}), "controller")
	name := probe.objectName("reporter")
	for owner, labels := range map[string]map[string]string{
		"another component": {HealthLabel: "model-gateway"},
		"no label":          nil,
		"unrelated labels":  {"app": "something"},
	} {
		foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, Labels: labels}, Data: map[string]string{"keep": "me"}}
		s, _ := newTestStore(t, newFakeClient(t, interceptor.Funcs{}, foreign), "controller")
		if err := s.ReplaceModelCallHealth(context.Background(), "reporter", []store.ModelCallHealth{{Target: "t", Calls: 1}}); err == nil {
			t.Errorf("%s: Replace() overwrote a ConfigMap it does not own", owner)
		}
		var after corev1.ConfigMap
		if err := s.client.Get(context.Background(), client.ObjectKeyFromObject(foreign), &after); err != nil || after.Data["keep"] != "me" || len(after.Data) != 1 {
			t.Errorf("%s: the foreign ConfigMap was modified: %+v (%v)", owner, after.Data, err)
		}
	}
}

func TestReplaceRetriesConflictsAndRaces(t *testing.T) {
	ctx := context.Background()
	records := []store.ModelCallHealth{{Target: "t", Calls: 1}}

	t.Run("update conflicts are retried", func(t *testing.T) {
		var updates atomic.Int32
		funcs := interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if updates.Add(1) <= 2 {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("changed"))
			}
			return c.Update(ctx, obj, opts...)
		}}
		s, _ := newTestStore(t, newFakeClient(t, funcs), "controller")
		if err := s.ReplaceModelCallHealth(ctx, "r", records); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceModelCallHealth(ctx, "r", records); err != nil || updates.Load() != 3 {
			t.Fatalf("Replace() = %v after %d updates, want success on the third attempt", err, updates.Load())
		}
	})

	t.Run("a create race falls back to updating", func(t *testing.T) {
		var gets atomic.Int32
		funcs := interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if gets.Add(1) == 1 {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
			},
		}
		s, _ := newTestStore(t, newFakeClient(t, funcs), "controller")
		if err := s.ReplaceModelCallHealth(ctx, "r", records); err != nil {
			t.Fatalf("Replace() error: %v", err)
		}
	})

	t.Run("persistent conflicts give up", func(t *testing.T) {
		funcs := interceptor.Funcs{Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("changed"))
		}}
		s, _ := newTestStore(t, newFakeClient(t, funcs), "controller")
		if err := s.ReplaceModelCallHealth(ctx, "r", records); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceModelCallHealth(ctx, "r", records); err == nil || !strings.Contains(err.Error(), "kept changing") {
			t.Fatalf("Replace() error = %v, want it to give up after the attempts", err)
		}
	})
}

func TestReplaceAndListReportAPIFailures(t *testing.T) {
	ctx := context.Background()
	records := []store.ModelCallHealth{{Target: "t", Calls: 1}}
	apiErr := errors.New("api unavailable")
	existing := healthConfigMap(testNamespace, "r", "controller", epoch)

	for name, test := range map[string]struct {
		funcs   interceptor.Funcs
		objects []client.Object
		wantErr string
	}{
		"get": {
			funcs: interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apiErr
			}},
			wantErr: "load model call health",
		},
		"create": {
			funcs:   interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return apiErr }},
			wantErr: "create model call health",
		},
		"update": {
			funcs:   interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return apiErr }},
			objects: []client.Object{existing},
			wantErr: "update model call health",
		},
	} {
		s, _ := newTestStore(t, newFakeClient(t, test.funcs, test.objects...), "controller")
		if err := s.ReplaceModelCallHealth(ctx, "r", records); err == nil || !errors.Is(err, apiErr) || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: Replace() error = %v, want %q wrapping the API error", name, err, test.wantErr)
		}
	}

	failing := interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return apiErr }}
	s, _ := newTestStore(t, newFakeClient(t, failing), "controller")
	if _, err := s.ListModelCallHealth(ctx, time.Minute); !errors.Is(err, apiErr) {
		t.Errorf("List() error = %v, want the API error", err)
	}
}

func TestListFiltersStaleReportersAndSkipsMalformedConfigMaps(t *testing.T) {
	good := record{Target: "good", Calls: 2, Faults: 1}
	oversized := healthConfigMap(testNamespace, "kognition-mch-controller-oversized", "controller", epoch, good)
	oversized.Data[DataKey] = strings.Repeat("x", maxPayloadBytes+1)
	tooManyRecords := payload{Reporter: "many", UpdatedAt: epoch, Records: make([]record, store.MaxModelCallHealthTargets+1)}
	tooManyEncoded, _ := json.Marshal(tooManyRecords)
	labeled := map[string]string{HealthLabel: "controller"}
	objects := []client.Object{
		healthConfigMap(testNamespace, "kognition-mch-controller-fresh", "controller", epoch.Add(-10*time.Second), good),
		healthConfigMap(testNamespace, "kognition-mch-controller-stale", "controller", epoch.Add(-10*time.Minute), record{Target: "stale", Calls: 1}),
		healthConfigMap(testNamespace, "kognition-mch-controller-mixed", "controller", epoch, record{Target: "kept", Calls: 1}, record{Target: "", Calls: 1}, record{Target: "bad", Calls: 1, Faults: 5}),
		healthConfigMap("another-namespace", "kognition-mch-controller-elsewhere", "controller", epoch, record{Target: "elsewhere", Calls: 1}),
		rawConfigMap("no-label", nil, map[string]string{DataKey: `{"reporter":"x","updatedAt":"2026-11-24T12:00:00Z","records":[{"target":"unlabeled","calls":1}]}`}),
		rawConfigMap("kognition-mch-controller-garbage", labeled, map[string]string{DataKey: "{not json"}),
		rawConfigMap("kognition-mch-controller-nokey", labeled, map[string]string{"other": "x"}),
		rawConfigMap("kognition-mch-controller-notime", labeled, map[string]string{DataKey: `{"reporter":"x","records":[{"target":"notime","calls":1}]}`}),
		rawConfigMap("kognition-mch-controller-many", labeled, map[string]string{DataKey: string(tooManyEncoded)}),
		oversized,
	}
	s, clk := newTestStore(t, newFakeClient(t, interceptor.Funcs{}, objects...), "controller")
	clk.set(epoch)

	records, err := s.ListModelCallHealth(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	targets := map[string]bool{}
	for _, record := range records {
		targets[record.Target] = true
	}
	want := map[string]bool{"good": true, "kept": true}
	if len(targets) != len(want) || !targets["good"] || !targets["kept"] {
		t.Fatalf("listed targets %v, want only %v: stale, foreign, unlabeled and malformed objects must be ignored", targets, want)
	}

	everything, err := s.ListModelCallHealth(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range everything {
		found = found || record.Target == "stale"
	}
	if !found {
		t.Error("a longer maxAge must include the older reporter")
	}
}

func TestListCachesBrieflyAndWritesInvalidateTheCache(t *testing.T) {
	var lists atomic.Int32
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		lists.Add(1)
		return c.List(ctx, list, opts...)
	}}
	s, clk := newTestStore(t, newFakeClient(t, funcs), "controller")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.ListModelCallHealth(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if lists.Load() != 1 {
		t.Fatalf("API lists = %d, want 1 within the cache window", lists.Load())
	}
	clk.advance(listCacheTTL + time.Second)
	if _, err := s.ListModelCallHealth(ctx, time.Minute); err != nil || lists.Load() != 2 {
		t.Fatalf("API lists = %d (%v), want a refresh after the cache window", lists.Load(), err)
	}

	if err := s.ReplaceModelCallHealth(ctx, "r", []store.ModelCallHealth{{Target: "mine", Calls: 1}}); err != nil {
		t.Fatal(err)
	}
	records, err := s.ListModelCallHealth(ctx, time.Minute)
	if err != nil || len(records) != 1 || records[0].Target != "mine" {
		t.Fatalf("List() = %+v, %v, want the store to see its own publication immediately", records, err)
	}
}

func TestListReadsEveryPageAndBoundsThem(t *testing.T) {
	var objects []client.Object
	for i := 0; i < 250; i++ {
		objects = append(objects, healthConfigMap(testNamespace, fmt.Sprintf("kognition-mch-controller-%03d", i), "controller", epoch, record{Target: fmt.Sprintf("t%03d", i), Calls: 1}))
	}
	paged := func(pageLimit int) interceptor.Funcs {
		return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			configMaps, ok := list.(*corev1.ConfigMapList)
			if !ok {
				return c.List(ctx, list, opts...)
			}
			listOptions := &client.ListOptions{}
			listOptions.ApplyOptions(opts)
			start := 0
			if listOptions.Continue != "" {
				fmt.Sscanf(listOptions.Continue, "%d", &start)
			}
			if err := c.List(ctx, configMaps, client.InNamespace(listOptions.Namespace)); err != nil {
				return err
			}
			end := start + pageLimit
			if end < len(configMaps.Items) {
				configMaps.Continue = fmt.Sprintf("%d", end)
			} else {
				end = len(configMaps.Items)
			}
			configMaps.Items = configMaps.Items[start:end]
			return nil
		}}
	}

	s, _ := newTestStore(t, newFakeClient(t, paged(listPageSize), objects...), "controller")
	records, err := s.ListModelCallHealth(context.Background(), time.Minute)
	if err != nil || len(records) != 250 {
		t.Fatalf("List() = %d records, %v, want all 250 across pages", len(records), err)
	}

	endless := interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
		list.(*corev1.ConfigMapList).Continue = "more"
		return nil
	}}
	s, _ = newTestStore(t, newFakeClient(t, endless), "controller")
	if _, err := s.ListModelCallHealth(context.Background(), time.Minute); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("List() error = %v, want a bound on the number of pages", err)
	}
}

func TestGarbageCollectionRemovesOnlyAbandonedReporters(t *testing.T) {
	stale := healthConfigMap(testNamespace, "kognition-mch-model-gateway-stale", "model-gateway", epoch.Add(-3*time.Hour), record{Target: "t", Calls: 1})
	malformedOld := healthConfigMap(testNamespace, "malformed", "controller", epoch.Add(-3*time.Hour))
	malformedOld.Data[DataKey] = "{bad"
	malformedNew := healthConfigMap(testNamespace, "new", "controller", epoch)
	malformedNew.Data[DataKey] = "{bad"
	fresh := healthConfigMap(testNamespace, "kognition-mch-model-gateway-fresh", "model-gateway", epoch.Add(-30*time.Minute), record{Target: "t", Calls: 1})
	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "kognition-cost-unrelated", CreationTimestamp: metav1.NewTime(epoch.Add(-9 * time.Hour))}}

	t.Run("collecting stores delete abandoned objects", func(t *testing.T) {
		var lists atomic.Int32
		funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, list, opts...)
		}}
		kubernetesClient := newFakeClient(t, funcs, stale, malformedOld, malformedNew, fresh, unrelated)
		s, clk := newTestStore(t, kubernetesClient, "controller", WithGarbageCollection())
		ctx := context.Background()
		if err := s.ReplaceModelCallHealth(ctx, "r", []store.ModelCallHealth{{Target: "t", Calls: 1}}); err != nil {
			t.Fatal(err)
		}
		if lists.Load() != 0 {
			t.Fatal("publishing must not trigger collection")
		}
		s.sweep(ctx)
		names := map[string]bool{}
		for _, configMap := range listAll(t, kubernetesClient) {
			names[configMap.Name] = true
		}
		for _, gone := range []string{stale.Name, malformedOld.Name} {
			if names[gone] {
				t.Errorf("%s was not collected", gone)
			}
		}
		for _, kept := range []string{malformedNew.Name, fresh.Name, unrelated.Name, s.objectName("r")} {
			if !names[kept] {
				t.Errorf("%s must not be collected", kept)
			}
		}

		before := lists.Load()
		clk.advance(time.Minute)
		if err := s.ReplaceModelCallHealth(ctx, "r", nil); err != nil || lists.Load() != before {
			t.Fatalf("a second publish within the interval must not collect again (lists %d -> %d, err %v)", before, lists.Load(), err)
		}
		clk.advance(garbageCollectionInterval)
		s.sweep(ctx)
		if lists.Load() == before {
			t.Fatalf("an independent sweep must collect again (lists %d)", lists.Load())
		}
	})

	t.Run("other stores never delete", func(t *testing.T) {
		kubernetesClient := newFakeClient(t, interceptor.Funcs{}, stale, malformedOld)
		s, _ := newTestStore(t, kubernetesClient, "model-gateway")
		if err := s.ReplaceModelCallHealth(context.Background(), "r", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := len(listAll(t, kubernetesClient)); got != 3 {
			t.Fatalf("found %d ConfigMaps, want the abandoned ones left for a collecting store", got)
		}
	})

	t.Run("failures are logged and never fail the publish", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		listFails := interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("list failed")
		}}
		s, _ := newTestStore(t, newFakeClient(t, listFails), "controller", WithGarbageCollection(), WithLogger(zap.New(core)))
		if err := s.ReplaceModelCallHealth(context.Background(), "r", nil); err != nil {
			t.Fatalf("Replace() error = %v, want success despite the failed collection", err)
		}
		s.sweep(context.Background())
		if logs.Len() != 1 {
			t.Fatalf("logged %d warnings, want 1", logs.Len())
		}

		deleteFails := interceptor.Funcs{Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			if obj.GetName() == stale.Name {
				return errors.New("forbidden")
			}
			return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
		}}
		core, logs = observer.New(zap.WarnLevel)
		s, _ = newTestStore(t, newFakeClient(t, deleteFails, stale, malformedOld), "controller", WithGarbageCollection(), WithLogger(zap.New(core)))
		if err := s.ReplaceModelCallHealth(context.Background(), "r", nil); err != nil {
			t.Fatal(err)
		}
		s.sweep(context.Background())
		if logs.Len() != 1 {
			t.Fatalf("logged %d warnings, want 1 for the forbidden delete and none for the vanished object", logs.Len())
		}
	})
}

func TestHealthConfigMapIdentityFiltersReadersAndCollection(t *testing.T) {
	mutations := map[string]func(*corev1.ConfigMap){
		"unrelated labeled name": func(cm *corev1.ConfigMap) { cm.Name = "unrelated" },
		"wrong component":        func(cm *corev1.ConfigMap) { cm.Labels[HealthLabel] = "model-gateway" },
		"invalid component":      func(cm *corev1.ConfigMap) { cm.Labels[HealthLabel] = "Controller" },
		"oversized component": func(cm *corev1.ConfigMap) {
			cm.Labels[HealthLabel] = strings.Repeat("a", 33)
			cm.Name = objectName(cm.Labels[HealthLabel], "r")
		},
		"invalid hash": func(cm *corev1.ConfigMap) { cm.Name = NamePrefix + "controller-" + strings.Repeat("g", 32) },
		"short hash":   func(cm *corev1.ConfigMap) { cm.Name = NamePrefix + "controller-abcd" },
		"extra label":  func(cm *corev1.ConfigMap) { cm.Labels["foreign"] = "true" },
		"extra data":   func(cm *corev1.ConfigMap) { cm.Data["foreign"] = "true" },
		"binary data":  func(cm *corev1.ConfigMap) { cm.BinaryData = map[string][]byte{"foreign": {1}} },
		"owner":        func(cm *corev1.ConfigMap) { cm.OwnerReferences = []metav1.OwnerReference{{Name: "foreign"}} },
		"finalizer":    func(cm *corev1.ConfigMap) { cm.Finalizers = []string{"foreign"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			foreign := healthConfigMap(testNamespace, "r", "controller", epoch.Add(-3*time.Hour), record{Target: "t", Calls: 1})
			mutate(foreign)
			kubeClient := newFakeClient(t, interceptor.Funcs{}, foreign)
			s, _ := newTestStore(t, kubeClient, "controller", WithGarbageCollection())
			if records, err := s.ListModelCallHealth(context.Background(), 4*time.Hour); err != nil || len(records) != 0 {
				t.Fatalf("foreign evidence accepted: %+v, %v", records, err)
			}
			if err := s.collect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(listAll(t, kubeClient)) != 1 {
				t.Fatal("an unrelated labeled ConfigMap must survive collection")
			}
		})
	}
}

func TestListRejectsMismatchedReportersAndDuplicateTargets(t *testing.T) {
	for name, published := range map[string]payload{
		"empty reporter": {UpdatedAt: epoch, Records: []record{{Target: "t", Calls: 1}}},
		"wrong reporter": {Reporter: "other", UpdatedAt: epoch, Records: []record{{Target: "t", Calls: 1}}},
		"duplicate targets": {Reporter: "r", UpdatedAt: epoch, Records: []record{
			{Target: "t", Calls: 1, Faults: 1, ConsecutiveFaults: 1},
			{Target: "t", Calls: 1, Faults: 1, ConsecutiveFaults: 1},
			{Target: "t", Calls: 1, Faults: 1, ConsecutiveFaults: 1},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			configMap := healthConfigMap(testNamespace, "r", "controller", epoch)
			encoded, err := json.Marshal(published)
			if err != nil {
				t.Fatal(err)
			}
			configMap.Data[DataKey] = string(encoded)
			good := healthConfigMap(testNamespace, "good", "model-gateway", epoch, record{Target: "good", Calls: 1})
			s, _ := newTestStore(t, newFakeClient(t, interceptor.Funcs{}, configMap, good), "controller")
			records, err := s.ListModelCallHealth(context.Background(), time.Minute)
			if err != nil || len(records) != 1 || records[0].Target != "good" {
				t.Fatalf("List() = %+v, %v, want only the unaffected reporter", records, err)
			}
		})
	}
}

func TestCollectorRunsAtStartupAndPeriodicallyWithoutPublications(t *testing.T) {
	var lists atomic.Int32
	swept := make(chan struct{}, 10)
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		err := c.List(ctx, list, opts...)
		lists.Add(1)
		select {
		case swept <- struct{}{}:
		default:
		}
		return err
	}}
	stale := healthConfigMap(testNamespace, "stale", "model-gateway", epoch.Add(-2*time.Hour))
	kubeClient := newFakeClient(t, funcs, stale)
	s, _ := newTestStore(t, kubeClient, "controller", WithGarbageCollection())
	s.collectionInterval = 10 * time.Millisecond
	if !s.NeedLeaderElection() {
		t.Fatal("collection must require leader election")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-swept:
		case <-time.After(5 * time.Second):
			t.Fatal("idle collector did not sweep at startup and on the next tick")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not stop on cancellation")
	}
	if objects := listAll(t, kubeClient); len(objects) != 0 {
		t.Fatalf("idle collector left %d abandoned reporters", len(objects))
	}
}

func TestCollectionProgressesBeyondTheReaderPageLimit(t *testing.T) {
	count := maxListPages*listPageSize + 5
	objects := make([]client.Object, 0, count+1)
	for i := 0; i < count; i++ {
		objects = append(objects, healthConfigMap(testNamespace, fmt.Sprintf("r-%d", i), "model-gateway", epoch.Add(-2*time.Hour)))
	}
	fresh := healthConfigMap(testNamespace, "fresh", "model-gateway", epoch, record{Target: "t", Calls: 1})
	objects = append(objects, fresh)
	funcs := interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		options := &client.ListOptions{}
		options.ApplyOptions(opts)
		start := 0
		if options.Continue != "" {
			if _, err := fmt.Sscanf(options.Continue, "%d", &start); err != nil {
				return err
			}
		}
		end := min(start+listPageSize, len(objects))
		page := list.(*corev1.ConfigMapList)
		for _, obj := range objects[start:end] {
			page.Items = append(page.Items, *obj.(*corev1.ConfigMap).DeepCopy())
		}
		if end < len(objects) {
			page.Continue = fmt.Sprintf("%d", end)
		}
		return nil
	}}
	kubeClient := newFakeClient(t, funcs, objects...)
	s, _ := newTestStore(t, kubeClient, "controller", WithGarbageCollection())
	if _, err := s.ListModelCallHealth(context.Background(), time.Minute); err == nil {
		t.Fatal("reader must still enforce its page bound")
	}
	if err := s.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects[:count] {
		var remaining corev1.ConfigMap
		if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(obj), &remaining); !apierrors.IsNotFound(err) {
			t.Fatalf("abandoned reporter %s survived: %v", obj.GetName(), err)
		}
	}
	var remaining corev1.ConfigMap
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(fresh), &remaining); err != nil {
		t.Fatalf("fresh reporter must survive: %v", err)
	}
}

func TestCollectionSkipsRefreshedAndReplacedObjects(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
			stale := healthConfigMap(testNamespace, "r", "model-gateway", epoch.Add(-2*time.Hour))
			var raced bool
			funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := &client.DeleteOptions{}
				options.ApplyOptions(opts)
				if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
					*options.Preconditions.UID != obj.GetUID() || *options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
					t.Fatal("delete must carry the listed UID and resource version")
				}
				var current corev1.ConfigMap
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &current); err != nil {
					return err
				}
				if !raced {
					raced = true
					fresh := healthConfigMap(testNamespace, "r", "model-gateway", epoch)
					if replace {
						if err := c.Delete(ctx, &current); err != nil {
							return err
						}
						fresh.UID = "replacement"
						if err := c.Create(ctx, fresh); err != nil {
							return err
						}
					} else {
						current.Data = fresh.Data
						if err := c.Update(ctx, &current); err != nil {
							return err
						}
					}
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &current); err != nil {
						return err
					}
				}
				if *options.Preconditions.UID != current.UID || *options.Preconditions.ResourceVersion != current.ResourceVersion {
					return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("changed"))
				}
				return c.Delete(ctx, obj, opts...)
			}}
			kubeClient := newFakeClient(t, funcs, stale)
			s, clk := newTestStore(t, kubeClient, "controller", WithGarbageCollection())
			if err := s.collect(context.Background()); err != nil {
				t.Fatalf("a raced delete must be skipped, not fail the sweep: %v", err)
			}
			var remaining corev1.ConfigMap
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(stale), &remaining); err != nil {
				t.Fatalf("fresh evidence was removed: %v", err)
			}
			clk.advance(2 * time.Hour)
			if err := s.collect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(stale), &remaining); !apierrors.IsNotFound(err) {
				t.Fatalf("a later sweep must reconsider abandoned evidence: %v", err)
			}
		})
	}
}
