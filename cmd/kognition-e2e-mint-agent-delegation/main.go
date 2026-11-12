// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-e2e-mint-agent-delegation creates a source-session capability for the live gateway E2E test.
package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func main() {
	keyID := flag.String("key-id", runtimeauth.ActiveCapabilityKeyID, "Agent Communication Gateway HMAC key identifier")
	namespace := flag.String("namespace", "", "source agent namespace")
	agent := flag.String("agent", "", "source agent name")
	identity := flag.String("identity", "", "automatic source AgentIdentity name")
	instance := flag.String("instance", "", "source AgentInstance name")
	instanceUID := flag.String("instance-uid", "", "source AgentInstance UID")
	session := flag.String("session", "", "active source AgentInstance session ID")
	flag.Parse()

	keyInput, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read capability key from stdin:", err)
		os.Exit(2)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyInput)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode capability key:", err)
		os.Exit(2)
	}
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(*keyID, key, "", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create capability key ring:", err)
		os.Exit(2)
	}
	issuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuerWithKeyRing(keys, 2*time.Minute)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create delegation session capability issuer:", err)
		os.Exit(2)
	}
	capability, err := issuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: *namespace, AgentName: *agent, AgentIdentityName: *identity,
		InstanceName: *instance, InstanceUID: *instanceUID, SessionID: *session,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue agent delegation session capability:", err)
		os.Exit(2)
	}
	fmt.Println(capability)
}
