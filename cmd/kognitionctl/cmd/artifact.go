// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func init() {
	rootCmd.AddCommand(newArtifactCommand())
}

func newArtifactCommand() *cobra.Command {
	command := &cobra.Command{Use: "artifact", Short: "Prepare locally signed artifact specifications", PersistentPreRunE: func(*cobra.Command, []string) error { return nil }}
	var filename string
	payload := &cobra.Command{Use: "signing-payload -f PATH", Short: "Write the exact Tool or Model signing payload without contacting a server", Args: cobra.NoArgs}
	payload.Flags().StringVarP(&filename, "file", "f", "", "One YAML/JSON artifact file, or - for stdin")
	payload.RunE = func(command *cobra.Command, _ []string) error {
		if filename == "" {
			return errors.New("artifact signing payload requires --file")
		}
		reader := command.InOrStdin()
		if filename != "-" {
			file, err := os.Open(filename)
			if err != nil {
				return fmt.Errorf("open artifact: %w", err)
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		data, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
		if err != nil || len(data) > 4<<20 {
			return errors.New("artifact input is unreadable or exceeds 4 MiB")
		}
		var resource map[string]any
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(&resource); err != nil {
			return fmt.Errorf("decode artifact: %w", err)
		}
		if !errors.Is(decoder.Decode(&map[string]any{}), io.EOF) {
			return errors.New("sign exactly one artifact document")
		}
		kind, _ := resource["kind"].(string)
		metadata, _ := resource["metadata"].(map[string]any)
		namespace, _ := metadata["namespace"].(string)
		name, _ := metadata["name"].(string)
		if namespace == "" || name == "" {
			return errors.New("artifact must declare metadata.namespace and metadata.name")
		}
		encoded, err := json.Marshal(resource)
		if err != nil {
			return fmt.Errorf("encode artifact: %w", err)
		}
		canonical, err := admission.ArtifactSignaturePayload(kind, namespace, name, encoded)
		if err != nil {
			return err
		}
		_, err = command.OutOrStdout().Write(canonical)
		return err
	}
	command.AddCommand(payload)
	return command
}
