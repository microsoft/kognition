// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func newInvocationProfileDigestCommand() *cobra.Command {
	var filename, name string
	command := &cobra.Command{
		Use:               "invocation-profile-digest",
		Short:             "Compute a reviewed invocation profile spec digest without connecting to a server",
		Args:              cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(command *cobra.Command, _ []string) error {
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			defer file.Close()
			digest, err := readInvocationProfileDigest(file, name)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), digest)
			return err
		},
	}
	command.Flags().StringVarP(&filename, "filename", "f", "", "YAML or JSON profile manifest")
	command.Flags().StringVar(&name, "name", "", "Select one profile by name in a multi-document manifest")
	_ = command.MarkFlagRequired("filename")
	return command
}

func readInvocationProfileDigest(reader io.Reader, name string) (string, error) {
	const maximumBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(reader, maximumBytes+1))
	if err != nil || len(data) > maximumBytes {
		return "", fmt.Errorf("profile input cannot be read or exceeds four MiB")
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	count := 0
	result := ""
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return "", err
		}
		if len(raw) == 0 {
			continue
		}
		var document struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec json.RawMessage `json:"spec"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			return "", err
		}
		if document.Kind != "ModelInvocationProfile" || (name != "" && document.Metadata.Name != name) {
			continue
		}
		if document.APIVersion != api.APIVersion {
			return "", fmt.Errorf("unsupported invocation profile API version %q", document.APIVersion)
		}
		var spec api.ModelInvocationProfileSpec
		specDecoder := json.NewDecoder(bytes.NewReader(document.Spec))
		specDecoder.DisallowUnknownFields()
		if err := specDecoder.Decode(&spec); err != nil {
			return "", err
		}
		if err := spec.Validate(); err != nil {
			return "", err
		}
		result, err = modelinvocation.Digest(spec)
		if err != nil {
			return "", err
		}
		count++
	}
	if count != 1 {
		return "", fmt.Errorf("expected exactly one selected invocation profile, found %d; use --name for a multi-profile manifest", count)
	}
	return result, nil
}

func init() {
	rootCmd.AddCommand(newInvocationProfileDigestCommand())
}
