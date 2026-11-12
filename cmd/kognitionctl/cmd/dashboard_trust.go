// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/observability"
)

var dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) {
	if k8sRESTConfig == nil {
		return nil, errors.New("kube client not initialized")
	}
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register Kubernetes audit scheme: %w", err)
	}
	kubernetesClient, err := ctrlclient.New(k8sRESTConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes audit client: %w", err)
	}
	return k8saudit.NewConfigMapStore(kubernetesClient, effectiveDashboardAuditNamespace())
}

var dashboardTrustNamespaceLister = k8sListNamespaces

func effectiveDashboardAuditNamespace() string {
	if auditNamespace := strings.TrimSpace(dashboardAuditNamespace); auditNamespace != "" {
		return auditNamespace
	}
	return kubeNamespace
}

func handleDashTrustDecisions(w http.ResponseWriter, r *http.Request) {
	query, err := observability.ParseTrustDecisionQuery(r.URL.Query().Get("namespace"), r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !k8sMode {
		if query.Namespace == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "all-namespace trust queries require Kubernetes mode"})
			return
		}
		forwardStandaloneTrustDecisions(w, r, query.Namespace)
		return
	}
	auditStore, err := dashboardAuditStoreFactory()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": fmt.Sprintf("initialize Kubernetes trust decision store: %v", err)})
		return
	}
	result, err := queryDashboardTrustDecisions(r, auditStore, query, time.Now())
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, observability.ErrInvalidTrustDecisionQuery) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func queryDashboardTrustDecisions(r *http.Request, auditStore observability.AuditQueryStore, query observability.TrustDecisionQuery, now time.Time) (*observability.TrustDecisionQueryResult, error) {
	if query.Namespace != "" {
		return observability.QueryTrustDecisions(r.Context(), auditStore, query, now)
	}
	namespaces, err := dashboardTrustNamespaceLister()
	if err != nil {
		return nil, fmt.Errorf("list namespaces for trust decision query: %w", err)
	}
	sort.Strings(namespaces)
	items := make([]observability.TrustDecisionV1, 0)
	truncated := false
	for _, namespace := range namespaces {
		namespacedQuery := query
		namespacedQuery.Namespace = namespace
		result, queryErr := observability.QueryTrustDecisions(r.Context(), auditStore, namespacedQuery, now)
		if errors.Is(queryErr, k8saudit.ErrQueryPageLimit) {
			truncated = true
			continue
		}
		if queryErr != nil {
			return nil, fmt.Errorf("query trust decisions in namespace %q: %w", namespace, queryErr)
		}
		items = append(items, result.Items...)
		truncated = truncated || result.Truncated
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Timestamp.Equal(items[right].Timestamp) {
			return items[left].ID > items[right].ID
		}
		return items[left].Timestamp.After(items[right].Timestamp)
	})
	limit := query.Limit
	if limit == 0 {
		limit = observability.DefaultTrustDecisionLimit
	}
	if len(items) > limit {
		items = items[:limit]
		truncated = true
	}
	return &observability.TrustDecisionQueryResult{APIVersion: observability.TrustDecisionAPIVersion, Items: items, Truncated: truncated}, nil
}

func forwardStandaloneTrustDecisions(w http.ResponseWriter, r *http.Request, targetNamespace string) {
	values := r.URL.Query()
	values.Del("namespace")
	target := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/trust-decisions", serverURL, url.PathEscape(targetNamespace))
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	response, err := doHTTP(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("read trust decision response: %v", err)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}
