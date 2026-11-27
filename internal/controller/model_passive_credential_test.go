// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

func fingerprintOf(data map[string]string) string {
	return cpi.CredentialFingerprint(&cpi.Credentials{Data: data})
}

// accountTargets are the evidence keys of a Model of newPassiveModel that
// presents the secret with the given fingerprint.
func accountTargets(provider, fingerprint string) modelhealth.Targets {
	return modelhealth.TargetsFor(provider, passiveEndpoint, passiveModelID, fingerprint, true)
}

// accountModel is a Model of newPassiveModel that presents the secret named secret.
func accountModel(name, provider, secret string) v1alpha1.Model {
	model := newPassiveModel(name, provider)
	model.Spec.CredentialRef = &v1alpha1.LocalObjectReference{Name: secret}
	return model
}

func TestModelCredentialFingerprintMatchesWhatTheCallSitesPresent(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	storeSecretRefForModel(t, s, newInternalSecretRef("default", "plain", "apiKey", "sk-one"))

	// The gateway and the reconciler keep the "type" entry in the secret's data; the
	// chat handler and the dispatcher strip it. Every one of them presents the same
	// secret, so the controller must derive the same identity for all of them.
	withType := v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "typed", Namespace: "default", Annotations: map[string]string{internalSecretDataAnnotation: `{"type":"api-key","apiKey":"sk-one"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "secret/typed", Key: "apiKey"},
	}
	storeSecretRefForModel(t, s, withType)

	want := fingerprintOf(map[string]string{"apiKey": "sk-one"})
	for _, name := range []string{"plain", "typed"} {
		model := accountModel("m", "anthropic", name)
		got, known := modelCredentialFingerprint(ctx, s, "default", &model)
		if !known || got != want || got == "" {
			t.Errorf("secret %q: fingerprint = %q known=%t, want the fingerprint %q shared by every call site", name, got, known, want)
		}
	}
}

func TestModelCredentialFingerprintOfEnvironmentSecrets(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	envSecret := v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "from-env", Namespace: "default"},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "env", BackendRef: "KOGNITION_TEST_MODEL_KEY", Key: "apiKey"},
	}
	storeSecretRefForModel(t, s, envSecret)
	model := accountModel("m", "anthropic", "from-env")

	if got, known := modelCredentialFingerprint(ctx, s, "default", &model); known || got != "" {
		t.Fatalf("fingerprint = %q known=%t, want an environment variable only the callers have to be unknown here", got, known)
	}

	t.Setenv("KOGNITION_TEST_MODEL_KEY", "sk-env")
	got, known := modelCredentialFingerprint(ctx, s, "default", &model)
	if want := fingerprintOf(map[string]string{"apiKey": "sk-env"}); !known || got != want {
		t.Fatalf("fingerprint = %q known=%t, want %q as the chat handler and the dispatcher build it", got, known, want)
	}
}

func TestModelCredentialFingerprintIsKnownEmptyWithoutACredentialRef(t *testing.T) {
	ctx := context.Background()
	for name, ref := range map[string]*v1alpha1.LocalObjectReference{
		"no reference": nil,
		"unnamed":      {},
		"empty name":   {Name: ""},
	} {
		model := newPassiveModel("m", "ollama")
		model.Spec.CredentialRef = ref
		if got, known := modelCredentialFingerprint(ctx, newMemStore(), "default", &model); !known || got != "" {
			t.Errorf("%s: fingerprint = %q known=%t, want a known empty identity: such a Model presents no secret", name, got, known)
		}
	}
}

func TestModelCredentialFingerprintIsUnknownWhenTheSecretCannotBeResolved(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	annotated := func(name, data string) {
		storeSecretRefForModel(t, s, v1alpha1.SecretRef{
			ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{internalSecretDataAnnotation: data}},
			Spec:       v1alpha1.SecretRefSpec{BackendType: "internal"},
		})
	}
	annotated("malformed", `{not json`)
	storeSecretRefForModel(t, s, v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Name: "no-annotation", Namespace: "default"}, Spec: v1alpha1.SecretRefSpec{BackendType: "internal"}})
	storeSecretRefForModel(t, s, v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Name: "vault", Namespace: "default"}, Spec: v1alpha1.SecretRefSpec{BackendType: "vault"}})
	if err := s.Create(ctx, "SecretRef", "default", "not-a-secret", []byte(`"a string"`)); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"absent", "malformed", "no-annotation", "vault", "not-a-secret"} {
		model := accountModel("m", "anthropic", name)
		if got, known := modelCredentialFingerprint(ctx, s, "default", &model); known || got != "" {
			t.Errorf("secret %q: fingerprint = %q known=%t, want unknown, so no account's failures are blamed on this Model", name, got, known)
		}
	}
}

// providerStore returns a store whose reads of SecretRefs and Models are plain
// but whose evidence is set by the test.
func newAccountsFixture(t *testing.T) (*ModelReconciler, *callHealthStore) {
	t.Helper()
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeSecretRefForModel(t, s.memStore, newInternalSecretRef("default", "key-a", "apiKey", "secret-a"))
	storeSecretRefForModel(t, s.memStore, newInternalSecretRef("default", "key-b", "apiKey", "secret-b"))
	storeModel(t, s.memStore, accountModel("account-a", "anthropic", "key-a"))
	storeModel(t, s.memStore, accountModel("account-b", "anthropic", "key-b"))
	return r, s
}

func TestModelReconciler_CredentialFailuresDegradeOnlyTheAccountThatMadeThem(t *testing.T) {
	for _, reason := range []string{cpi.ReasonAuthenticationFailed, cpi.ReasonQuotaOrBilling} {
		t.Run(reason, func(t *testing.T) {
			r, s := newAccountsFixture(t)
			a := accountTargets("anthropic", fingerprintOf(map[string]string{"apiKey": "secret-a"}))
			// Account A's calls are rejected; account B's calls to the same
			// destination succeed.
			s.set([]store.ModelCallHealth{
				faultRecord(a.Credential, reason),
				{Target: a.Destination, Calls: 5, ConsecutiveSuccesses: 5, LastSuccessAt: faultRecord("", reason).LastFaultAt},
			}, nil)

			reconcileModel(t, r, "default", "account-a")
			reconcileModel(t, r, "default", "account-b")

			degraded := getModel(t, s.memStore, "default", "account-a")
			condition := readyCondition(t, degraded)
			if degraded.Status.Available || condition.Reason != reason {
				t.Fatalf("account A = available %t reason %q, want it degraded for %s", degraded.Status.Available, condition.Reason, reason)
			}
			if bystander := getModel(t, s.memStore, "default", "account-b"); !bystander.Status.Available || bystander.Status.Phase != v1alpha1.PhaseReady {
				t.Fatalf("account B = %+v, want it unaffected by account A's %s", bystander.Status, reason)
			}
		})
	}
}

func TestModelReconciler_DestinationFailuresDegradeEveryAccount(t *testing.T) {
	r, s := newAccountsFixture(t)
	destination := accountTargets("anthropic", "").Destination
	s.set([]store.ModelCallHealth{faultRecord(destination, cpi.ReasonEndpointError)}, nil)

	for _, name := range []string{"account-a", "account-b"} {
		reconcileModel(t, r, "default", name)
		got := getModel(t, s.memStore, "default", name)
		if got.Status.Available || readyCondition(t, got).Reason != cpi.ReasonEndpointError {
			t.Errorf("%s = %+v, want a provider-wide outage to degrade every account", name, got.Status)
		}
	}
}

func TestModelReconciler_AccountRecoversWhenItsCredentialWorksAgain(t *testing.T) {
	r, s := newAccountsFixture(t)
	a := accountTargets("anthropic", fingerprintOf(map[string]string{"apiKey": "secret-a"}))
	s.set([]store.ModelCallHealth{faultRecord(a.Credential, cpi.ReasonAuthenticationFailed)}, nil)
	reconcileModel(t, r, "default", "account-a")
	if getModel(t, s.memStore, "default", "account-a").Status.Available {
		t.Fatal("precondition: account A must be degraded")
	}

	s.set([]store.ModelCallHealth{recoveryRecord(a.Credential, 3)}, nil)
	reconcileModel(t, r, "default", "account-a")
	if got := getModel(t, s.memStore, "default", "account-a"); !got.Status.Available || got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("account A = %+v, want it recovered once its own credential succeeds again", got.Status)
	}
}

func TestModelReconciler_AModelWithAnUnknownCredentialIsNeverBlamedForAnAccount(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeSecretRefForModel(t, s.memStore, v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "from-env", Namespace: "default"},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "env", BackendRef: "KOGNITION_TEST_UNSET_KEY", Key: "apiKey"},
	})
	storeModel(t, s.memStore, accountModel("env-model", "anthropic", "from-env"))
	unknown := accountTargets("anthropic", "")
	// Whatever account failed, including one that presented no secret at all.
	s.set([]store.ModelCallHealth{faultRecord(unknown.Credential, cpi.ReasonAuthenticationFailed)}, nil)

	reconcileModel(t, r, "default", "env-model")
	if got := getModel(t, s.memStore, "default", "env-model"); !got.Status.Available {
		t.Fatalf("env-model = %+v, want a Model whose credential the controller cannot resolve left to its active probe", got.Status)
	}
}

func TestModelReconciler_ModelsWithoutACredentialShareTheFailuresOfCallsWithoutOne(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("local", "ollama"))
	targets := accountTargets("ollama", "")
	s.set([]store.ModelCallHealth{faultRecord(targets.Credential, cpi.ReasonAuthenticationFailed)}, nil)

	reconcileModel(t, r, "default", "local")
	if got := getModel(t, s.memStore, "default", "local"); got.Status.Available {
		t.Fatalf("local = %+v, want the failures of credential-less calls to count for a credential-less Model", got.Status)
	}
}

func TestModelHealthWatcherQueuesOnlyTheAccountWhoseCredentialFailed(t *testing.T) {
	watcher, backing, queue := newWatcherFixture(t)
	storeSecretRefForModel(t, backing.memStore, newInternalSecretRef("default", "key-a", "apiKey", "secret-a"))
	storeSecretRefForModel(t, backing.memStore, newInternalSecretRef("default", "key-b", "apiKey", "secret-b"))
	storeModel(t, backing.memStore, accountModel("account-a", "anthropic", "key-a"))
	storeModel(t, backing.memStore, accountModel("account-b", "anthropic", "key-b"))
	a := accountTargets("anthropic", fingerprintOf(map[string]string{"apiKey": "secret-a"}))
	backing.set([]store.ModelCallHealth{faultRecord(a.Credential, cpi.ReasonQuotaOrBilling)}, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.all(); len(got) != 1 || got[0] != "default/account-a" {
		t.Fatalf("queued %v, want only the account whose credential failed", got)
	}
}

func TestModelHealthWatcherReadsEachSecretRefOncePerCheck(t *testing.T) {
	watcher, backing, _ := newWatcherFixture(t)
	storeSecretRefForModel(t, backing.memStore, newInternalSecretRef("default", "shared", "apiKey", "secret"))
	for _, name := range []string{"one", "two", "three"} {
		storeModel(t, backing.memStore, accountModel(name, "anthropic", "shared"))
	}
	backing.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)

	if err := watcher.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := backing.secretGets.Load(); got != 1 {
		t.Fatalf("SecretRef read %d times, want once for the three Models sharing it", got)
	}
}

func TestModelClassCacheAlsoCachesSecretRefs(t *testing.T) {
	backing := &countingStore{callHealthStore: newCallHealthStore()}
	storeSecretRefForModel(t, backing.memStore, newInternalSecretRef("default", "key", "apiKey", "secret"))
	cache := &modelClassCache{Store: backing, cache: make(map[string]modelClassLookup)}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := cache.Get(ctx, "SecretRef", "default", "key"); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.Get(ctx, "SecretRef", "default", "absent"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get(absent) = %v, want not found", err)
		}
	}
	if got := backing.secretGets.Load(); got != 2 {
		t.Fatalf("store read %d times, want one read each for the secret and the absent name", got)
	}
	// The same name in another namespace or of another kind is another entry.
	if _, err := cache.Get(ctx, "SecretRef", "other", "key"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a SecretRef of another namespace must not be served from the cache: %v", err)
	}
	var got v1alpha1.SecretRef
	raw, err := cache.Get(ctx, "SecretRef", "default", "key")
	if err != nil || json.Unmarshal(raw, &got) != nil || got.ObjectMeta.Name != "key" {
		t.Fatalf("cached SecretRef = %s, %v, want the stored object", raw, err)
	}
}
