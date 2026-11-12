// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func Snapshot(plan Plan) (Plan, string, func(), error) {
	directory, err := os.MkdirTemp("", "kognition-install-")
	if err != nil {
		return Plan{}, "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	fail := func(err error) (Plan, string, func(), error) { cleanup(); return Plan{}, "", nil, err }
	if plan.PreviousManifest != nil {
		previous := *plan.PreviousManifest
		plan.PreviousManifest = &previous
	}
	if plan.PreviousDeployedManifest != nil {
		previous := *plan.PreviousDeployedManifest
		plan.PreviousDeployedManifest = &previous
	}
	artifacts := []*Artifact{&plan.Chart, &plan.Values, &plan.Manifest}
	names := []string{"chart.tgz", "values.yaml", "manifest.yaml"}
	if plan.PreviousManifest != nil {
		artifacts = append(artifacts, plan.PreviousManifest)
		names = append(names, "previous.yaml")
	}
	if plan.PreviousDeployedManifest != nil {
		artifacts = append(artifacts, plan.PreviousDeployedManifest)
		names = append(names, "previous-deployed.yaml")
	}
	for index, artifact := range artifacts {
		content, err := ReadArtifact(*artifact)
		if err != nil {
			return fail(err)
		}
		artifact.Path = filepath.Join(directory, names[index])
		if err := os.WriteFile(artifact.Path, content, 0600); err != nil {
			return fail(err)
		}
	}
	config, err := clientcmd.LoadFromFile(plan.Kubeconfig)
	if err != nil {
		return fail(err)
	}
	if err := clientcmdapi.MinifyConfig(config); err != nil {
		return fail(err)
	}
	for _, credentials := range config.AuthInfos {
		if credentials.Exec != nil || credentials.AuthProvider != nil {
			return fail(errors.New("installation requires static administrator credentials; exec and auth-provider plugins are unsupported"))
		}
		if credentials.TokenFile != "" {
			token, err := os.ReadFile(credentials.TokenFile)
			if err != nil {
				return fail(err)
			}
			credentials.Token = strings.TrimSpace(string(token))
			credentials.TokenFile = ""
			if credentials.Token == "" {
				return fail(errors.New("administrator token file is empty"))
			}
		}
	}
	if err := clientcmdapi.FlattenConfig(config); err != nil {
		return fail(err)
	}
	content, err := clientcmd.Write(*config)
	if err != nil {
		return fail(err)
	}
	plan.Kubeconfig = filepath.Join(directory, "kubeconfig")
	if err := os.WriteFile(plan.Kubeconfig, content, 0600); err != nil {
		return fail(err)
	}
	content, err = json.Marshal(plan)
	if err != nil {
		return fail(err)
	}
	path := filepath.Join(directory, "plan.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return fail(err)
	}
	return plan, path, cleanup, nil
}
