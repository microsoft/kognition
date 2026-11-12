// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// kubeconfig types (minimal subset we need)
type kubeConfig struct {
	CurrentContext string              `yaml:"current-context"`
	Clusters       []kubeConfigCluster `yaml:"clusters"`
	Contexts       []kubeConfigContext `yaml:"contexts"`
	Users          []kubeConfigUser    `yaml:"users"`
}

type kubeConfigCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		Server                   string `yaml:"server"`
		CertificateAuthority     string `yaml:"certificate-authority"`
		CertificateAuthorityData string `yaml:"certificate-authority-data"`
		InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	} `yaml:"cluster"`
}

type kubeConfigContext struct {
	Name    string `yaml:"name"`
	Context struct {
		Cluster   string `yaml:"cluster"`
		User      string `yaml:"user"`
		Namespace string `yaml:"namespace"`
	} `yaml:"context"`
}

type kubeConfigUser struct {
	Name string `yaml:"name"`
	User struct {
		Token                 string `yaml:"token"`
		ClientCertificate     string `yaml:"client-certificate"`
		ClientCertificateData string `yaml:"client-certificate-data"`
		ClientKey             string `yaml:"client-key"`
		ClientKeyData         string `yaml:"client-key-data"`
		Exec                  *struct {
			APIVersion string   `yaml:"apiVersion"`
			Command    string   `yaml:"command"`
			Args       []string `yaml:"args"`
			Env        []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"env"`
		} `yaml:"exec"`
	} `yaml:"user"`
}

// execCredential represents the ExecCredential response from exec-based auth plugins.
type execCredential struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// kubeServiceProxy holds the resolved kube transport info.
type kubeServiceProxy struct {
	// baseURL is the K8s API service proxy URL prefix, e.g.:
	// https://<api-server>/api/v1/namespaces/kognition/services/kognition-apiserver:8080/proxy
	baseURL string
	client  *http.Client
}

// resolveKubeTransport loads kubeconfig and builds an HTTP client + service proxy URL.
func resolveKubeTransport(kubeContext, svcNamespace, svcName string, svcPort int) (*kubeServiceProxy, error) {
	apiHost, client, err := resolveKubeAPITransport(kubeContext)
	if err != nil {
		return nil, err
	}
	return &kubeServiceProxy{
		baseURL: fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s:%d/proxy",
			apiHost, svcNamespace, svcName, svcPort),
		client: client,
	}, nil
}

// resolveKubeAPITransport loads kubeconfig and builds an authenticated HTTP
// client whose transport can talk directly to the kube apiserver. Returns the
// apiserver base URL (scheme+host, no trailing slash) alongside the client.
//
// Callers that need to reach a specific in-cluster Service should either use
// resolveKubeTransport (which wraps this and pre-composes the proxy URL) or
// build proxy paths themselves using apiHost + /api/v1/namespaces/.../proxy.
func resolveKubeAPITransport(kubeContext string) (string, *http.Client, error) {
	cfg, err := loadKubeConfig()
	if err != nil {
		return "", nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	ctxName := kubeContext
	if ctxName == "" {
		ctxName = cfg.CurrentContext
	}

	// Find context
	var ctx *kubeConfigContext
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == ctxName {
			ctx = &cfg.Contexts[i]
			break
		}
	}
	if ctx == nil {
		return "", nil, fmt.Errorf("kubeconfig context %q not found", ctxName)
	}

	// Find cluster
	var cluster *kubeConfigCluster
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == ctx.Context.Cluster {
			cluster = &cfg.Clusters[i]
			break
		}
	}
	if cluster == nil {
		return "", nil, fmt.Errorf("kubeconfig cluster %q not found", ctx.Context.Cluster)
	}

	// Find user
	var user *kubeConfigUser
	for i := range cfg.Users {
		if cfg.Users[i].Name == ctx.Context.User {
			user = &cfg.Users[i]
			break
		}
	}
	if user == nil {
		return "", nil, fmt.Errorf("kubeconfig user %q not found", ctx.Context.User)
	}

	// Build TLS config
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cluster.Cluster.InsecureSkipTLSVerify, //nolint:gosec // user-controlled via kubeconfig
	}

	// CA certificate
	if cluster.Cluster.CertificateAuthorityData != "" {
		caCert, err := base64.StdEncoding.DecodeString(cluster.Cluster.CertificateAuthorityData)
		if err != nil {
			return "", nil, fmt.Errorf("decode CA data: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = pool
	} else if cluster.Cluster.CertificateAuthority != "" {
		caCert, err := os.ReadFile(cluster.Cluster.CertificateAuthority)
		if err != nil {
			return "", nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = pool
	}

	// Client certificate auth
	if user.User.ClientCertificateData != "" && user.User.ClientKeyData != "" {
		certPEM, err := base64.StdEncoding.DecodeString(user.User.ClientCertificateData)
		if err != nil {
			return "", nil, fmt.Errorf("decode client cert: %w", err)
		}
		keyPEM, err := base64.StdEncoding.DecodeString(user.User.ClientKeyData)
		if err != nil {
			return "", nil, fmt.Errorf("decode client key: %w", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return "", nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	} else if user.User.ClientCertificate != "" && user.User.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(user.User.ClientCertificate, user.User.ClientKey)
		if err != nil {
			return "", nil, fmt.Errorf("load client certificate files: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Resolve bearer token
	token, err := resolveToken(user)
	if err != nil {
		return "", nil, fmt.Errorf("resolve auth token: %w", err)
	}

	// Build transport with auth
	transport := &kubeRoundTripper{
		base: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
		token: token,
	}

	apiHost := strings.TrimRight(cluster.Cluster.Server, "/")
	return apiHost, &http.Client{Transport: transport}, nil
}

// resolveToken returns a bearer token from the user's kubeconfig entry.
func resolveToken(user *kubeConfigUser) (string, error) {
	// Static token
	if user.User.Token != "" {
		return user.User.Token, nil
	}

	// Exec-based credential plugin (e.g., kubelogin, gcloud, aws-iam-authenticator)
	if user.User.Exec != nil {
		return execToken(context.Background(), user.User.Exec.Command, user.User.Exec.Args, user.User.Exec.Env)
	}

	// Client cert auth — no bearer token needed
	if user.User.ClientCertificateData != "" || user.User.ClientCertificate != "" {
		return "", nil
	}

	return "", fmt.Errorf("no supported auth method found in kubeconfig user entry")
}

// execToken runs an exec-based credential plugin and extracts the token.
func execToken(ctx context.Context, command string, args []string, env []struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // command comes from user's kubeconfig
	cmd.Env = os.Environ()
	for _, e := range env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("exec credential plugin %q: %w", command, err)
	}

	var cred execCredential
	if err := json.Unmarshal(out, &cred); err != nil {
		return "", fmt.Errorf("parse exec credential output: %w", err)
	}

	if cred.Status.Token == "" {
		return "", fmt.Errorf("exec credential plugin %q returned empty token", command)
	}
	return cred.Status.Token, nil
}

// kubeRoundTripper injects the bearer token into requests.
type kubeRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (rt *kubeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.token != "" {
		req.Header.Set("Authorization", "Bearer "+rt.token)
	}
	return rt.base.RoundTrip(req)
}

// loadKubeConfig reads and parses the kubeconfig file.
func loadKubeConfig() (*kubeConfig, error) {
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".kube", "config")
	}

	// Handle multiple kubeconfig paths (first one wins)
	if sep := kubeConfigPathSeparator(); strings.Contains(path, sep) {
		path = strings.SplitN(path, sep, 2)[0]
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig %s: %w", path, err)
	}

	var cfg kubeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	return &cfg, nil
}

// kubeConfigPathSeparator returns the path separator for KUBECONFIG (: on unix, ; on windows).
func kubeConfigPathSeparator() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}
