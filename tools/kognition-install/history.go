// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"errors"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
)

func validateFailedHistory(content []byte, plan installation.Plan) error {
	var history []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(content, &history); err != nil {
		return err
	}
	latest, deployed := 0, 0
	seen := map[int]bool{}
	for _, entry := range history {
		if entry.Revision <= 0 || entry.Revision > plan.ReleaseRevision || seen[entry.Revision] {
			return errors.New("Helm history advanced or contains invalid or duplicate revisions")
		}
		seen[entry.Revision] = true
		if entry.Revision > latest {
			latest = entry.Revision
		}
		if entry.Revision == plan.ReleaseRevision && entry.Status != "failed" {
			return errors.New("current Helm revision is not failed")
		}
		if entry.Status == "deployed" && entry.Revision > deployed {
			deployed = entry.Revision
		}
	}
	if latest != plan.ReleaseRevision || deployed != plan.PreviousDeployedRevision {
		return errors.New("Helm history differs from reviewed failed and last-deployed revisions")
	}
	return nil
}
