---
title: Local development
description: Prepare and verify an authenticated standalone development installation.
---

# Local development

Use the standalone Compose deployment to explore the control plane on a local
development machine. It includes PostgreSQL and the gateway components. It is
not a production installation profile.

## 1. Prepare the checkout and tools

Use Docker with Compose and the Go version declared in
[go.mod](../../go.mod). Run commands from the repository root.
Use Bash on Linux/macOS or PowerShell 7 on Windows. Some optional repository
helpers require PowerShell 7 on all platforms; see [command shells](../README.md#command-shells).

Build the CLI:

```powershell
go build -o .\bin\kognitionctl.exe .\cmd\kognitionctl
.\bin\kognitionctl.exe --help
```

On Linux or macOS:

```bash
go build -o ./bin/kognitionctl ./cmd/kognitionctl
./bin/kognitionctl --help
```

## 2. Prepare credentials and persistence

Follow [Standalone Compose: credentials](../standalone-compose.md#credentials)
to generate distinct operator, runtime and capability credentials in an
access-controlled environment. Retain the same values for subsequent starts.
Do not regenerate keys against an existing installation.

For an existing database, follow the [upgrade and migration requirements](../standalone-compose.md#start-or-upgrade)
and verify a recoverable backup first. Do not guess a migration baseline or remove
the database volume to bypass a failed upgrade.

## 3. Start and verify the development stack

After completing that preparation:

```powershell
docker compose config --quiet
docker compose up -d --build --wait --wait-timeout 180
docker compose ps -a
Invoke-RestMethod http://127.0.0.1:8080/healthz
```

**Bash (Linux/macOS):**

```bash
docker compose config --quiet
docker compose up -d --build --wait --wait-timeout 180
docker compose ps -a
curl --fail --silent --show-error http://127.0.0.1:8080/healthz
```

Expected checks:

- Compose starts the configured services.
- The one-shot `migrate` service exits with code zero.
- The API health endpoint returns a successful response.

These checks do not prove that a model provider or an agent invocation works.
Inspect service logs if startup fails, without sharing credential-bearing
configuration or database connection strings.

## 4. Verify authenticated resource access

The CLI uses `KOGNITION_API_TOKEN` or `--token-file` for API authentication.
In the same protected shell used to prepare the Compose credentials:

```powershell
$env:KOGNITION_API_TOKEN = $env:KOGNITION_BOOTSTRAP_TOKEN
.\bin\kognitionctl.exe --mode standalone --server http://127.0.0.1:8080 `
    --namespace default get agents
```

**Bash (Linux/macOS):**

```bash
export KOGNITION_API_TOKEN="$KOGNITION_BOOTSTRAP_TOKEN"
./bin/kognitionctl --mode standalone --server http://127.0.0.1:8080 \
    --namespace default get agents
```

An empty collection is a valid result on a fresh installation. An authentication
failure is not: verify the operator token and endpoint rather than disabling
authentication. Use HTTPS for a non-loopback server.

## 5. Continue to an agent

Ask the operator to prepare an approved model in the intended workload namespace,
then follow [First agent](first-agent.md). A local control plane does not include
a working model deployment or authorize provider charges.

The default synchronous runtime path and the opt-in HTTP runtime have different
capability limits. Read [Standalone Compose](../standalone-compose.md) before
enabling the external runtime overlay.

## Stop without deleting persistent data

The stop command is the same in Bash and PowerShell:

```sh
docker compose stop
```

Keep the Compose project identity, credentials and persistent volume for the next
start. Do not use `docker compose down -v` on data you need to retain.
