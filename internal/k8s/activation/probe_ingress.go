// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"errors"
	"reflect"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (manifest Manifest) probeComponent(object *unstructured.Unstructured) string {
	if object.GetKind() != "CiliumNetworkPolicy" || object.GetNamespace() != manifest.Namespace {
		return ""
	}
	prefix := strings.TrimSuffix(manifest.Deployment, "-k8s-controller")
	for _, component := range []string{"postgres", "kb-pgvector", "memory-service", "memory-policy-gateway", "tool-gateway", "model-gateway", "agent-communication-gateway", "knowledge-gateway", "k8s-controller"} {
		if object.GetName() == prefix+"-"+component+"-probes" {
			return component
		}
	}
	return ""
}

func (manifest Manifest) isRuntimeProbe(object *unstructured.Unstructured) bool {
	return object.GetKind() == "CiliumClusterwideNetworkPolicy" && object.GetNamespace() == "" &&
		object.GetName() == strings.TrimSuffix(manifest.Deployment, "-k8s-controller")+"-runtime-probes"
}

func (manifest Manifest) validateProbeIngress(object *unstructured.Unstructured) error {
	if manifest.StdioIsolationProfile != "cilium" {
		return errors.New("Cilium probe ingress requires the Cilium isolation profile")
	}
	component := manifest.probeComponent(object)
	runtime := manifest.isRuntimeProbe(object)
	if component == "" && !runtime {
		return errors.New("unrecognized Cilium probe policy")
	}
	release := ""
	for _, current := range manifest.Objects {
		if current != nil && current.GetKind() == "Deployment" && current.GetName() == manifest.Guard {
			var err error
			release, _, err = unstructured.NestedString(current.Object, "spec", "template", "metadata", "labels", "app.kubernetes.io/instance")
			if err != nil {
				return err
			}
		}
	}
	if release == "" || object.GetLabels()["app.kubernetes.io/instance"] != release {
		return errors.New("probe ingress must select the installation release")
	}
	rules, _, err := unstructured.NestedSlice(object.Object, "spec", "ingress")
	if err != nil || len(rules) != 1 {
		return errors.New("probe ingress requires one scoped rule")
	}
	rule, ok := rules[0].(map[string]interface{})
	if !ok {
		return errors.New("invalid probe ingress rule")
	}
	groups, ok := rule["toPorts"].([]interface{})
	if !ok || len(groups) != 1 {
		return errors.New("probe ingress requires one port group")
	}
	group, ok := groups[0].(map[string]interface{})
	if !ok {
		return errors.New("invalid probe port group")
	}
	ports, ok := group["ports"].([]interface{})
	if !ok || len(ports) != 1 {
		return errors.New("probe ingress requires one TCP port")
	}
	port, ok := ports[0].(map[string]interface{})
	if !ok {
		return errors.New("invalid probe port")
	}
	text, ok := port["port"].(string)
	if !ok {
		return errors.New("probe port must be a string")
	}
	number, err := strconv.Atoi(text)
	if err != nil || number < 1 || number > 65535 || runtime && number != 8080 {
		return errors.New("invalid probe port number")
	}
	selector := map[string]interface{}{
		"matchLabels": map[string]interface{}{
			"k8s:app.kubernetes.io/component": component,
			"k8s:app.kubernetes.io/instance":  release,
		},
	}
	if runtime {
		namespaces := make([]interface{}, len(manifest.Namespaces))
		for index, namespace := range manifest.Namespaces {
			namespaces[index] = namespace
		}
		selector = map[string]interface{}{
			"matchLabels": map[string]interface{}{"k8s:app.kubernetes.io/managed-by": "kognition"},
			"matchExpressions": []interface{}{map[string]interface{}{
				"key": "k8s:io.kubernetes.pod.namespace", "operator": "In", "values": namespaces,
			}},
		}
	}
	expected := map[string]interface{}{
		"endpointSelector":  selector,
		"enableDefaultDeny": map[string]interface{}{"ingress": false, "egress": false},
		"ingress": []interface{}{map[string]interface{}{
			"fromEntities": []interface{}{"host", "remote-node", "health"},
			"toPorts": []interface{}{map[string]interface{}{
				"ports": []interface{}{map[string]interface{}{"port": text, "protocol": "TCP"}},
			}},
		}},
	}
	if object.GetLabels()["app.kubernetes.io/instance"] == "" || !reflect.DeepEqual(object.Object["spec"], expected) || object.Object["specs"] != nil {
		return errors.New("probe policy must allow only node health traffic to the selected component or enrolled runtime namespaces")
	}
	return nil
}
