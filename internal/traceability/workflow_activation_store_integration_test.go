//go:build integration

package traceability

import (
	"context"
	"os"
	"testing"
	"time"

	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/workflowactivationtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestWorkflowActivationStoreRealAPIServer(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Error(err)
		}
	})
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workflow-control"}}); err != nil {
		test.Fatal(err)
	}
	workflowactivationtest.Run(test, func(test *testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore) {
		firstClient, err := client.New(config, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			test.Fatal(err)
		}
		secondClient, err := client.New(config, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			test.Fatal(err)
		}
		clock := func() (time.Time, error) { return time.Now().UTC(), nil }
		first, err := k8scontroller.NewWorkflowActivationGateStore(firstClient, firstClient, "workflow-control", clock)
		if err != nil {
			test.Fatal(err)
		}
		second, err := k8scontroller.NewWorkflowActivationGateStore(secondClient, secondClient, "workflow-control", clock)
		if err != nil {
			test.Fatal(err)
		}
		return first, second
	})
}
