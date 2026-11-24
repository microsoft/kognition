// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"errors"
	"iter"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGRPCUnaryInterceptor(t *testing.T) {
	interceptor := GRPCUnaryInterceptor(NewStaticTokenAuthenticator(map[string]string{"expected": "caller"}), nil)
	handler := func(ctx context.Context, _ any) (any, error) {
		identity, ok := IdentityFromContext(ctx)
		if !ok || identity.Subject != "caller" {
			t.Fatalf("identity = %#v, present = %t", identity, ok)
		}
		return "ok", nil
	}
	_, err := interceptor(context.Background(), nil, nil, handler)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("missing token status = %s, want %s", got, codes.Unauthenticated)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer expected"))
	response, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil || response != "ok" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestGRPCUnaryInterceptorRequiresConfiguredGroup(t *testing.T) {
	authenticator := fakeAuth{id: &Identity{Subject: "caller", Groups: []string{"operators"}, Method: "token"}}
	interceptor := GRPCUnaryInterceptor(authenticator, []string{"admins"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer token"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run")
		return nil, nil
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("group status = %s, want %s", got, codes.PermissionDenied)
	}
}

type workflowGRPCAuthenticationFixture struct {
	principal     api.ControlPlaneRoleSubject
	upstream      *Identity
	reviewer      *workflowTokenReviewer
	pod           *corev1.Pod
	reader        client.Client
	authenticator Authenticator
}

func newWorkflowGRPCAuthenticationFixture(t *testing.T) workflowGRPCAuthenticationFixture {
	t.Helper()
	principal := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://cluster.example", Subject: "system:serviceaccount:team-a:executor"}
	upstream := &Identity{Method: principal.Method, Issuer: principal.Issuer, Subject: principal.Subject, Claims: map[string]any{"controllerUID": "caller-chosen-uid"}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "executor-pod", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "executor"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "executor", UID: "account-uid"}}
	reviewer := &workflowTokenReviewer{status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{WorkflowExecutorTokenAudience}, User: authenticationv1.UserInfo{Username: principal.Subject, UID: "account-uid", Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"executor-pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"}}}}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, account).Build()
	authenticator, err := NewWorkflowExecutorAuthenticator(fakeAuth{id: upstream}, []api.ControlPlaneRoleSubject{principal}, reviewer, reader)
	if err != nil {
		t.Fatal(err)
	}
	return workflowGRPCAuthenticationFixture{principal: principal, upstream: upstream, reviewer: reviewer, pod: pod, reader: reader, authenticator: authenticator}
}

func TestWorkflowExecutorGRPCRequiresLiveProof(t *testing.T) {
	for _, scenario := range []struct {
		name string
		code codes.Code
	}{
		{"send", codes.OK}, {"poll", codes.OK}, {"case-insensitive-scheme", codes.OK},
		{"missing-authenticator", codes.Unavailable}, {"missing-handler", codes.Unavailable},
		{"missing-credential", codes.Unauthenticated}, {"duplicate-credential", codes.Unauthenticated},
		{"same-duplicate-credential", codes.Unauthenticated}, {"empty-token", codes.Unauthenticated},
		{"wrong-scheme", codes.Unauthenticated}, {"padded-token", codes.Unauthenticated},
		{"oversized-token", codes.Unauthenticated}, {"control-token", codes.Unauthenticated},
		{"vertical-tab-token", codes.Unauthenticated}, {"delete-control-token", codes.Unauthenticated},
		{"unicode-space-token", codes.Unauthenticated},
		{"claim-only", codes.Unauthenticated}, {"foreign-principal", codes.Unauthenticated},
		{"nil-identity", codes.Unauthenticated}, {"failed-authentication", codes.Unauthenticated},
		{"review-denied", codes.Unauthenticated}, {"unready-controller", codes.Unauthenticated},
		{"unsupported-method", codes.Unimplemented}, {"missing-method", codes.Unimplemented},
		{"canceled", codes.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newWorkflowGRPCAuthenticationFixture(t)
			principal, upstream, reviewer, pod, reader, authenticator := fixture.principal, fixture.upstream, fixture.reviewer, fixture.pod, fixture.reader, fixture.authenticator
			if scenario.name == "review-denied" {
				reviewer.status.Authenticated = false
			}
			if scenario.name == "unready-controller" {
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
				if err := reader.Status().Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			}
			values := []string{"Bearer same-verified-token"}
			info := &grpc.UnaryServerInfo{FullMethod: "/lf.a2a.v1.A2AService/SendMessage"}
			switch scenario.name {
			case "poll":
				info.FullMethod = "/lf.a2a.v1.A2AService/GetTask"
			case "case-insensitive-scheme":
				values[0] = "bEaReR same-verified-token"
			case "missing-authenticator":
				authenticator = nil
			case "missing-credential":
				values = nil
			case "duplicate-credential":
				values = append(values, "Bearer different-token")
			case "same-duplicate-credential":
				values = append(values, values[0])
			case "empty-token":
				values[0] = "Bearer "
			case "wrong-scheme":
				values[0] = "Basic same-verified-token"
			case "padded-token":
				values[0] += " "
			case "oversized-token":
				values[0] = "Bearer " + strings.Repeat("a", (64<<10)+1)
			case "control-token":
				values[0] += "\x00"
			case "vertical-tab-token":
				values[0] += "\v"
			case "delete-control-token":
				values[0] += "\x7f"
			case "unicode-space-token":
				values[0] += "\u00a0"
			case "claim-only":
				authenticator = fakeAuth{id: upstream}
			case "foreign-principal":
				upstream.Subject = "system:serviceaccount:team-a:other"
			case "nil-identity":
				authenticator = fakeAuth{}
			case "failed-authentication":
				authenticator = fakeAuth{err: errors.New("secret-detail-must-not-escape")}
			case "unsupported-method":
				info.FullMethod = "/lf.a2a.v1.A2AService/CancelTask"
			case "missing-method":
				info = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario.name == "canceled" {
				cancel()
			}
			ctx = metadata.NewIncomingContext(WithIdentity(ctx, &Identity{Subject: "forged-context"}), metadata.MD{"authorization": values})
			calls := 0
			handler := grpc.UnaryHandler(func(admitted context.Context, request any) (any, error) {
				calls++
				if scenario.code != codes.OK {
					t.Fatal("unqualified credential reached execution")
				}
				identity, present := IdentityFromContext(admitted)
				if !present || identity.Subject != principal.Subject || request != "original-request" {
					t.Fatalf("incorrect admitted identity/request: %+v %v", identity, request)
				}
				uid, err := AuthenticatedWorkflowControllerUID(admitted)
				if err != nil || uid != "pod-uid" || reviewer.token != "same-verified-token" {
					t.Fatalf("missing live same-token controller proof: %q %v", uid, err)
				}
				if deadline, present := admitted.Deadline(); present && time.Until(deadline) <= 5*time.Second {
					t.Fatal("authentication timeout leaked into task execution")
				}
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
				if err := reader.Status().Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
				if _, err := AuthenticatedWorkflowControllerUID(admitted); err == nil {
					t.Fatal("retained gRPC proof survived controller revocation")
				}
				return "accepted", nil
			})
			if scenario.name == "missing-handler" {
				handler = nil
			}
			response, err := WorkflowExecutorGRPCUnaryInterceptor(authenticator)(ctx, "original-request", info, handler)
			if status.Code(err) != scenario.code {
				t.Fatalf("status=%s want=%s: %v", status.Code(err), scenario.code, err)
			}
			if strings.Contains(status.Convert(err).Message(), "secret-detail-must-not-escape") {
				t.Fatal("authentication failure leaked credential diagnostic")
			}
			if scenario.code == codes.OK {
				if calls != 1 || response != "accepted" {
					t.Fatalf("admitted calls=%d response=%v", calls, response)
				}
			} else if calls != 0 || response != nil {
				t.Fatalf("denial reached execution: calls=%d response=%v", calls, response)
			}
			if upstream.workflowControllerProof != nil {
				t.Fatal("gRPC authentication mutated upstream identity")
			}
		})
	}
}

type workflowGRPCProbeExecutor struct{ calls atomic.Int64 }

func (executor *workflowGRPCProbeExecutor) Execute(_ context.Context, task *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		executor.calls.Add(1)
		if yield(a2a.NewSubmittedTask(task, task.Message), nil) {
			yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil), nil)
		}
	}
}

func (executor *workflowGRPCProbeExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		executor.calls.Add(1)
		yield(nil, a2a.ErrUnsupportedOperation)
	}
}

func TestWorkflowExecutorGRPCRejectsStreamingBeforeExecution(t *testing.T) {
	fixture := newWorkflowGRPCAuthenticationFixture(t)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(WorkflowExecutorGRPCServerOptions(fixture.authenticator)...)
	executor := &workflowGRPCProbeExecutor{}
	a2agrpc.NewHandler(a2asrv.NewHandler(executor)).RegisterWith(server)
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close(); <-stopped })
	connection, err := grpc.NewClient("passthrough:///workflow-authentication", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	transport := a2agrpc.NewGRPCTransport(connection)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	params := a2aclient.ServiceParams{"a2a-version": {"1.0"}}
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("untrusted request"))
	request := &a2a.SendMessageRequest{Tenant: "team-a", Message: message}
	var streamError error
	for _, err := range transport.SendStreamingMessage(ctx, params, request) {
		streamError = err
		break
	}
	if streamError == nil || executor.calls.Load() != 0 {
		t.Fatalf("unsupported streaming reached execution: err=%v calls=%d", streamError, executor.calls.Load())
	}
	if result, err := transport.SendMessage(ctx, params, request); err == nil || result != nil || executor.calls.Load() != 0 {
		t.Fatalf("anonymous unary request reached execution: result=%v err=%v calls=%d", result, err, executor.calls.Load())
	}
	duplicate := a2aclient.ServiceParams{"a2a-version": {"1.0"}, "authorization": {"Bearer same-verified-token", "Bearer conflicting-token"}}
	if result, err := transport.SendMessage(ctx, duplicate, request); err == nil || result != nil || executor.calls.Load() != 0 {
		t.Fatalf("ambiguous wire credentials reached execution: result=%v err=%v calls=%d", result, err, executor.calls.Load())
	}
	verified := a2aclient.ServiceParams{"a2a-version": {"1.0"}, "authorization": {"Bearer same-verified-token"}}
	result, err := transport.SendMessage(ctx, verified, request)
	if err != nil {
		t.Fatal(err)
	}
	task, present := result.(*a2a.Task)
	if !present || task.Status.State != a2a.TaskStateCompleted || executor.calls.Load() != 1 {
		t.Fatalf("authenticated wire control failed: task=%v calls=%d", result, executor.calls.Load())
	}
	retained, err := transport.GetTask(ctx, verified, &a2a.GetTaskRequest{Tenant: "team-a", ID: task.ID})
	if err != nil || retained == nil || retained.ID != task.ID || executor.calls.Load() != 1 {
		t.Fatalf("capability-free authenticated polling failed: task=%v err=%v calls=%d", retained, err, executor.calls.Load())
	}
	for _, method := range []string{"stream", "subscribe", "cancel", "list", "push", "card"} {
		t.Run(method, func(t *testing.T) {
			var rejected error
			switch method {
			case "stream":
				for _, err := range transport.SendStreamingMessage(ctx, verified, request) {
					rejected = err
					break
				}
			case "subscribe":
				for _, err := range transport.SubscribeToTask(ctx, verified, &a2a.SubscribeToTaskRequest{Tenant: "team-a", ID: task.ID}) {
					rejected = err
					break
				}
			case "cancel":
				_, rejected = transport.CancelTask(ctx, verified, &a2a.CancelTaskRequest{Tenant: "team-a", ID: task.ID})
			case "list":
				_, rejected = transport.ListTasks(ctx, verified, &a2a.ListTasksRequest{Tenant: "team-a"})
			case "push":
				_, rejected = transport.CreateTaskPushConfig(ctx, verified, &a2a.PushConfig{})
			case "card":
				_, rejected = transport.GetExtendedAgentCard(ctx, verified, &a2a.GetExtendedAgentCardRequest{Tenant: "team-a"})
			}
			if rejected == nil || executor.calls.Load() != 1 {
				t.Fatalf("unqualified authenticated method reached execution: err=%v calls=%d", rejected, executor.calls.Load())
			}
		})
	}
	fixture.pod.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := fixture.reader.Status().Update(t.Context(), fixture.pod); err != nil {
		t.Fatal(err)
	}
	if result, err := transport.GetTask(ctx, verified, &a2a.GetTaskRequest{Tenant: "team-a", ID: task.ID}); err == nil || result != nil || executor.calls.Load() != 1 {
		t.Fatalf("revoked controller retained wire access: task=%v err=%v calls=%d", result, err, executor.calls.Load())
	}
}
