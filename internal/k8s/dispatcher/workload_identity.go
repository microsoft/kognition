// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

type WorkloadIdentityDelivery struct {
	ProxyImage   string
	TrustDomain  string
	ControllerID string
	Gateways     map[string]runtimeauth.WorkloadGatewayEndpoint
}

func (d *K8sDispatcher) workloadIdentityServicePorts(ports []corev1.ServicePort) []corev1.ServicePort {
	result := make([]corev1.ServicePort, 0, len(ports)+1)
	for _, port := range ports {
		if port.Name != "identity-status" {
			result = append(result, port)
		}
	}
	if d.config.WorkloadIdentityMode == "spire" {
		result = append(result, corev1.ServicePort{Name: "identity-status", Port: 8097, TargetPort: intstr.FromInt(8097), Protocol: corev1.ProtocolTCP})
	}
	return result
}

func WorkloadIdentityDeliveryFromEnv(lookup func(string) string) (*WorkloadIdentityDelivery, error) {
	mode := strings.TrimSpace(lookup("KOGNITION_WORKLOAD_IDENTITY_MODE"))
	image := lookup("KOGNITION_WORKLOAD_PROXY_IMAGE")
	if mode == "" || mode == "legacy" {
		if image != "" || lookup("KOGNITION_SPIFFE_GATEWAY_ENDPOINTS") != "" {
			return nil, fmt.Errorf("workload proxy configuration requires SPIRE mode")
		}
		return nil, nil
	}
	if mode != "spire" || image == "" {
		return nil, fmt.Errorf("unsupported or incomplete workload identity delivery")
	}
	configuration := &WorkloadIdentityDelivery{ProxyImage: image, TrustDomain: lookup("KOGNITION_SPIFFE_TRUST_DOMAIN"), ControllerID: lookup("KOGNITION_SPIFFE_ID")}
	raw := lookup("KOGNITION_SPIFFE_GATEWAY_ENDPOINTS")
	if len(raw) > 32768 {
		return nil, fmt.Errorf("invalid operator SPIFFE gateway endpoints")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, fmt.Errorf("invalid operator SPIFFE gateway endpoints")
	}
	configuration.Gateways = map[string]runtimeauth.WorkloadGatewayEndpoint{}
	for decoder.More() {
		token, err := decoder.Token()
		plane, valid := token.(string)
		if err != nil || !valid {
			return nil, fmt.Errorf("invalid gateway plane")
		}
		if _, exists := configuration.Gateways[plane]; exists {
			return nil, fmt.Errorf("duplicate gateway plane")
		}
		opening, err := decoder.Token()
		if err != nil || opening != json.Delim('{') {
			return nil, fmt.Errorf("invalid gateway binding")
		}
		fields := map[string]string{}
		for decoder.More() {
			token, err := decoder.Token()
			field, valid := token.(string)
			if err != nil || !valid || (field != "url" && field != "peerID") {
				return nil, fmt.Errorf("invalid gateway binding field")
			}
			if _, exists := fields[field]; exists {
				return nil, fmt.Errorf("duplicate gateway binding field")
			}
			value, err := decoder.Token()
			text, valid := value.(string)
			if err != nil || !valid || text == "" {
				return nil, fmt.Errorf("invalid gateway binding value")
			}
			fields[field] = text
		}
		if _, err := decoder.Token(); err != nil || len(fields) != 2 {
			return nil, fmt.Errorf("incomplete gateway binding")
		}
		gateway := runtimeauth.WorkloadGatewayEndpoint{URL: fields["url"], PeerID: fields["peerID"]}
		configuration.Gateways[plane] = gateway
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("invalid gateway endpoints")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing gateway configuration")
	}
	if err := configuration.validate(); err != nil {
		return nil, err
	}
	return configuration, nil
}

func (configuration *WorkloadIdentityDelivery) validate() error {
	imageParts := strings.Split(configuration.ProxyImage, "@sha256:")
	if len(imageParts) != 2 || imageParts[0] == "" || strings.ContainsAny(imageParts[0], " \t\r\n") || len(imageParts[1]) != 64 {
		return fmt.Errorf("workload proxy image must be digest pinned")
	}
	if _, err := hex.DecodeString(imageParts[1]); err != nil {
		return fmt.Errorf("invalid workload proxy image digest")
	}
	controller, err := spiffeid.FromString(configuration.ControllerID)
	if err != nil || controller.TrustDomain().String() != configuration.TrustDomain || controller.Path() == "" || configuration.ProxyImage == "" || len(configuration.Gateways) != 4 {
		return fmt.Errorf("SPIRE delivery requires proxy image, controller identity and four fixed gateway peers")
	}
	peers := map[string]bool{configuration.ControllerID: true}
	for _, plane := range []string{"model", "tool", "memory", "communication"} {
		gateway, exists := configuration.Gateways[plane]
		origin, err := url.Parse(gateway.URL)
		peer, peerErr := spiffeid.FromString(gateway.PeerID)
		if !exists || err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" || origin.Opaque != "" || (origin.Path != "" && origin.Path != "/") || peerErr != nil || peer.Path() == "" || peer.TrustDomain().String() != configuration.TrustDomain || peers[gateway.PeerID] {
			return fmt.Errorf("SPIRE delivery requires fixed HTTPS gateway origins and distinct pinned peers")
		}
		peers[gateway.PeerID] = true
	}
	return nil
}

func (d *K8sDispatcher) addWorkloadIdentityProxy(deployment *appsv1.Deployment, agent *api.Agent) error {
	if d.config.WorkloadIdentityMode != "spire" {
		return nil
	}
	configuration := d.config.WorkloadIdentityDelivery
	if configuration == nil {
		return fmt.Errorf("SPIRE-managed Agent pods require operator CSI delivery configuration")
	}
	if err := configuration.validate(); err != nil {
		return err
	}
	if _, err := runtimeauth.AgentIncarnationSPIFFEID(configuration.TrustDomain, agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, agent.ObjectMeta.UID); err != nil {
		return err
	}
	endpoints, err := json.Marshal(configuration.Gateways)
	if err != nil {
		return err
	}
	pod := &deployment.Spec.Template.Spec
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "workload-api", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "csi.spiffe.io", ReadOnly: ptr.To(true)}}})
	pod.Containers[0].Env = mergeEnvVars(pod.Containers[0].Env, []corev1.EnvVar{{Name: "KOGNITION_WORKLOAD_IDENTITY_MODE", Value: "spire"}, {Name: "KOGNITION_WORKLOAD_PROXY_URL", Value: "http://127.0.0.1:8096"}})
	pod.Containers = append(pod.Containers, corev1.Container{
		Name: "workload-identity-proxy", Image: configuration.ProxyImage,
		SecurityContext: &corev1.SecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		VolumeMounts:    []corev1.VolumeMount{{Name: "workload-api", MountPath: "/spiffe-workload-api", ReadOnly: true}},
		Ports:           []corev1.ContainerPort{{Name: "identity-status", ContainerPort: 8097, Protocol: corev1.ProtocolTCP}},
		Env: []corev1.EnvVar{
			{Name: "KOGNITION_NAMESPACE", Value: agent.ObjectMeta.Namespace}, {Name: "KOGNITION_AGENT_NAME", Value: agent.ObjectMeta.Name}, {Name: "KOGNITION_AGENT_UID", Value: agent.ObjectMeta.UID},
			{Name: "KOGNITION_SPIFFE_TRUST_DOMAIN", Value: configuration.TrustDomain}, {Name: "KOGNITION_SPIFFE_ENDPOINT_SOCKET", Value: "unix:///spiffe-workload-api/spire-agent.sock"},
			{Name: "KOGNITION_SPIFFE_GATEWAY_ENDPOINTS", Value: string(endpoints)}, {Name: "KOGNITION_WORKLOAD_PROXY_LISTEN_ADDR", Value: "127.0.0.1:8096"},
			{Name: "KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR", Value: "0.0.0.0:8097"}, {Name: "KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID", Value: configuration.ControllerID},
		},
		Resources:      corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/kognition-workload-identity-proxy", "--health-check"}}}, PeriodSeconds: 5, TimeoutSeconds: 2},
	})
	return nil
}

func (d *K8sDispatcher) updateWorkloadIdentityProxy(existing, desired *appsv1.Deployment) bool {
	if len(existing.Spec.Template.Spec.Containers) == 0 {
		return false
	}
	current := existing.Spec.Template.Spec.Containers
	sidecars := make([]corev1.Container, 0, len(current))
	for _, container := range current[1:] {
		if container.Name != "workload-identity-proxy" {
			sidecars = append(sidecars, container)
		}
	}
	for _, container := range desired.Spec.Template.Spec.Containers[1:] {
		if container.Name == "workload-identity-proxy" {
			sidecars = append(sidecars, container)
		}
	}
	if reflect.DeepEqual(current[1:], sidecars) || len(current[1:]) == 0 && len(sidecars) == 0 {
		return false
	}
	existing.Spec.Template.Spec.Containers = append(current[:1], sidecars...)
	return true
}
