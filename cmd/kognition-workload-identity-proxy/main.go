// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--health-check" {
		address, err := proxyListenAddress()
		if err == nil {
			err = checkProxyReadiness(address, time.Second)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func proxyListenAddress() (string, error) {
	address := os.Getenv("KOGNITION_WORKLOAD_PROXY_LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:8096"
	}
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return "", errors.New("workload proxy requires a literal loopback listener")
	}
	return address, nil
}

func statusListenConfiguration(lookup func(string) string, domain, workloadID string) (string, string, error) {
	address, peer := lookup("KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR"), lookup("KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID")
	if address == "" && peer == "" {
		return "", "", nil
	}
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	controller, peerErr := spiffeid.FromString(peer)
	if err != nil || net.ParseIP(host) == nil || portErr != nil || number < 1 || number > 65535 || peerErr != nil || controller.Path() == "" || controller.TrustDomain().String() != domain || controller.String() == workloadID {
		return "", "", errors.New("status listener requires a literal IP/port and distinct pinned controller SPIFFE identity")
	}
	return address, peer, nil
}

func run(ctx context.Context) error {
	address, err := proxyListenAddress()
	if err != nil {
		return err
	}
	domain := os.Getenv("KOGNITION_SPIFFE_TRUST_DOMAIN")
	expected, err := runtimeauth.AgentIncarnationSPIFFEID(domain, os.Getenv("KOGNITION_NAMESPACE"), os.Getenv("KOGNITION_AGENT_NAME"), os.Getenv("KOGNITION_AGENT_UID"))
	if err != nil {
		return err
	}
	statusAddress, controllerID, err := statusListenConfiguration(os.Getenv, domain, expected)
	if err != nil {
		return err
	}
	var gateways map[string]runtimeauth.WorkloadGatewayEndpoint
	configuration := os.Getenv("KOGNITION_SPIFFE_GATEWAY_ENDPOINTS")
	if len(configuration) > 32768 || json.Unmarshal([]byte(configuration), &gateways) != nil {
		return errors.New("invalid workload gateway configuration")
	}
	source, err := runtimeauth.NewWorkloadIdentity(ctx, runtimeauth.WorkloadIdentityConfig{SocketAddress: os.Getenv("KOGNITION_SPIFFE_ENDPOINT_SOCKET"), ExpectedID: expected, TrustDomain: domain})
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	proxy, err := runtimeauth.NewWorkloadProxy(source, gateways)
	if err != nil {
		return err
	}
	defer proxy.Close()
	server := &http.Server{Addr: address, Handler: proxy, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Second}
	servers := []*http.Server{server}
	if statusAddress != "" {
		statusServer, err := runtimeauth.NewWorkloadIdentityStatusServer(source, controllerID)
		if err != nil {
			return err
		}
		statusServer.Addr = statusAddress
		servers = append(servers, statusServer)
	}
	return serveProxyServers(ctx, servers)
}

func checkProxyReadiness(address string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/readyz", nil)
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New("workload identity is not ready")
	}
	return nil
}

func serveProxyServers(ctx context.Context, servers []*http.Server) error {
	if ctx.Err() != nil {
		return nil
	}
	listeners := make([]net.Listener, 0, len(servers))
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, server := range servers {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}
	completed := make(chan error, len(servers))
	for index, server := range servers {
		go func(server *http.Server, listener net.Listener) {
			if server.TLSConfig != nil {
				completed <- server.ServeTLS(listener, "", "")
			} else {
				completed <- server.Serve(listener)
			}
		}(server, listeners[index])
	}
	var servingErr error
	select {
	case servingErr = <-completed:
		if errors.Is(servingErr, http.ErrServerClosed) {
			servingErr = nil
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			servingErr = errors.Join(servingErr, err)
			_ = server.Close()
		}
	}
	return servingErr
}
