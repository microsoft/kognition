# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import os
import threading
import time
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from unittest.mock import MagicMock

try:
    from . import service
except ImportError:
    import service


def _wait_for_terminal(registry, task_id):
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        status = registry.get(task_id)
        if status is not None and status.state != "running":
            return status
        time.sleep(0.01)
    raise AssertionError(f"task {task_id} did not finish")


def test_registry_is_idempotent_and_forgets_terminal_task(monkeypatch):
    entered = threading.Event()
    release = threading.Event()

    def execute(payload, *args, **kwargs):
        entered.set()
        assert release.wait(1)
        return {"documents": 2, "chunks": 5, "sourceDigest": "sha256:test"}

    monkeypatch.setattr(service.ingest, "ingest_task", execute)
    registry = service.TaskRegistry()
    first, created = registry.submit("task-1", {"secret": "not-returned"})
    assert created
    assert entered.wait(1)

    duplicate, created = registry.submit("task-1", {"different": "payload"})
    assert not created
    assert duplicate is first
    assert not registry.delete("task-1")

    release.set()
    result = _wait_for_terminal(registry, "task-1")
    assert result.state == "succeeded"
    assert result.documents == 2
    assert result.chunks == 5
    assert result.sourceDigest == "sha256:test"
    assert "secret" not in vars(result)
    assert registry.delete("task-1")
    assert registry.get("task-1") is None


def test_registry_reports_sanitized_failure(monkeypatch):
    monkeypatch.setattr(
        service.ingest,
        "ingest_task",
        lambda *args, **kwargs: (_ for _ in ()).throw(RuntimeError("source unavailable")),
    )
    registry = service.TaskRegistry()
    registry.submit("failed-task", {"credential": "sensitive"})
    result = _wait_for_terminal(registry, "failed-task")
    assert result.state == "failed"
    assert result.error == "source unavailable"
    assert "sensitive" not in json.dumps(vars(result))


def _failed_status(monkeypatch, exc, payload=None, environment=None):
    def fail(*_args, **_kwargs):
        raise exc

    monkeypatch.setattr(service.ingest, "ingest_task", fail)
    registry = service.TaskRegistry()
    registry.submit("failed-task", payload or {}, environment or {})
    return _wait_for_terminal(registry, "failed-task")


def test_failure_redacts_payload_secrets_from_error(monkeypatch):
    payload = {
        "embedding": {"apiKey": "sk-secret-value", "model": "text-embedding-3-small"},
        "sources": [{"type": "blob", "credentials": {"accountKey": "blob-key-12345", "region": "eastus2"}}],
        "vectorStore": {"dsn": "postgres://svc:p%40ss-word@db.internal/kb"},
    }
    exc = ValueError(
        "rejected sk-secret-value / blob-key-12345 / eastus2 / "
        "postgres://svc:p%40ss-word@db.internal/kb / p@ss-word / text-embedding-3-small"
    )

    result = _failed_status(monkeypatch, exc, payload)

    assert result.state == "failed"
    for secret in ("sk-secret-value", "blob-key-12345", "p%40ss-word", "p@ss-word", "postgres://svc"):
        assert secret not in result.error
    assert "[redacted]" in result.error
    assert "text-embedding-3-small" in result.error


def test_failure_redacts_environment_secrets_from_error(monkeypatch):
    result = _failed_status(
        monkeypatch,
        RuntimeError("login failed for secret-from-env-1"),
        environment={"AZURE_CLIENT_SECRET": "secret-from-env-1", "AZURE_CLIENT_ID": "client-id"},
    )

    assert "secret-from-env-1" not in result.error
    assert "login failed for [redacted]" == result.error


class OperationalError(Exception):
    pass


def test_failure_hides_unexpected_exception_details(monkeypatch):
    result = _failed_status(
        monkeypatch,
        OperationalError("connection to host=10.1.2.3 user=svc password=hunter2 failed"),
    )

    assert result.error == "ingestion failed (OperationalError)"


def test_failure_message_is_single_line_and_bounded(monkeypatch):
    result = _failed_status(monkeypatch, ValueError("first\nsecond\x1b[31m " + "x" * 5000))

    assert "\n" not in result.error and "\x1b" not in result.error
    assert len(result.error) <= 500


def test_failure_details_are_logged_redacted(monkeypatch, caplog):
    with caplog.at_level("WARNING", logger="kognition.ingester"):
        _failed_status(
            monkeypatch,
            ValueError("bad sk-secret-value"),
            {"embedding": {"apiKey": "sk-secret-value"}},
        )

    assert "sk-secret-value" not in caplog.text
    assert "failed-task" in caplog.text


def test_registry_scopes_task_environment(monkeypatch):
    monkeypatch.setenv("AZURE_CLIENT_ID", "service-default")
    monkeypatch.delenv("AZURE_CLIENT_SECRET", raising=False)

    def execute(_payload, *args, **kwargs):
        assert os.environ["AZURE_CLIENT_ID"] == "task-client"
        assert os.environ["AZURE_CLIENT_SECRET"] == "task-secret"
        return {"documents": 0, "chunks": 0, "sourceDigest": "sha256:env"}

    monkeypatch.setattr(service.ingest, "ingest_task", execute)
    registry = service.TaskRegistry()
    registry.submit(
        "environment-task",
        {},
        {"AZURE_CLIENT_ID": "task-client", "AZURE_CLIENT_SECRET": "task-secret"},
    )
    result = _wait_for_terminal(registry, "environment-task")
    assert result.state == "succeeded"
    assert os.environ["AZURE_CLIENT_ID"] == "service-default"
    assert "AZURE_CLIENT_SECRET" not in os.environ


def test_http_task_lifecycle(monkeypatch):
    monkeypatch.setattr(
        service.ingest,
        "ingest_task",
        lambda *args, **kwargs: {"documents": 1, "chunks": 3, "sourceDigest": "sha256:http"},
    )
    handler = type("TestIngestHandler", (service.IngestHandler,), {"registry": service.TaskRegistry()})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_port}"
    try:
        with urllib.request.urlopen(f"{base_url}/healthz", timeout=1) as response:
            assert response.status == 200

        body = json.dumps({"id": "http-task", "payload": {"kb": {"name": "docs"}}}).encode()
        request = urllib.request.Request(
            f"{base_url}/v1/tasks",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=1) as response:
            assert response.status in (200, 202)

        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            with urllib.request.urlopen(f"{base_url}/v1/tasks/http-task", timeout=1) as response:
                status = json.load(response)
            if status["state"] == "succeeded":
                break
            time.sleep(0.01)
        assert status["documents"] == 1
        assert status["chunks"] == 3

        delete = urllib.request.Request(f"{base_url}/v1/tasks/http-task", method="DELETE")
        with urllib.request.urlopen(delete, timeout=1) as response:
            assert response.status == 204
        try:
            urllib.request.urlopen(f"{base_url}/v1/tasks/http-task", timeout=1)
        except urllib.error.HTTPError as exc:
            assert exc.code == 404
        else:
            raise AssertionError("deleted task remained available")
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=1)


def test_http_rejects_disallowed_environment_keys(monkeypatch):
    monkeypatch.delenv("UNAPPROVED_KEY", raising=False)
    handler = type("TestIngestHandler", (service.IngestHandler,), {"registry": service.TaskRegistry()})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_port}"
    try:
        body = json.dumps({
            "id": "task-bad-key",
            "payload": {"kb": {"name": "docs"}},
            "environment": {"UNAPPROVED_KEY": "malicious-value"},
        }).encode()
        request = urllib.request.Request(
            f"{base_url}/v1/tasks",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(request, timeout=1)
        except urllib.error.HTTPError as exc:
            assert exc.code == 400
            err_data = json.loads(exc.read().decode())
            assert "not allowed" in err_data.get("error", "")
            assert "malicious-value" not in err_data.get("error", "")
        else:
            raise AssertionError("expected HTTP 400 for disallowed environment variable")
        assert "UNAPPROVED_KEY" not in os.environ
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=1)


def test_http_indexer_executable_cannot_be_overridden(monkeypatch):
    monkeypatch.delenv("KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE", raising=False)
    handler = type("TestIngestHandler", (service.IngestHandler,), {"registry": service.TaskRegistry()})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_port}"
    try:
        body = json.dumps({
            "id": "task-override-indexer",
            "payload": {"kb": {"name": "docs"}},
            "environment": {"KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE": "/bin/sh"},
        }).encode()
        request = urllib.request.Request(
            f"{base_url}/v1/tasks",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(request, timeout=1)
        except urllib.error.HTTPError as exc:
            assert exc.code == 400
            err_data = json.loads(exc.read().decode())
            assert "KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE" in err_data.get("error", "")
            assert "/bin/sh" not in err_data.get("error", "")
        else:
            raise AssertionError("expected HTTP 400 when attempting to override indexer executable")
        assert "KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE" not in os.environ
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=1)


def test_http_private_networks_flag_cannot_be_enabled(monkeypatch):
    monkeypatch.setenv("KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS", "false")
    service.ingest.set_trusted_service_config(allow_private_networks=False)
    handler = type("TestIngestHandler", (service.IngestHandler,), {"registry": service.TaskRegistry()})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_port}"
    try:
        body = json.dumps({
            "id": "task-enable-ssrf",
            "payload": {"kb": {"name": "docs"}},
            "environment": {"KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS": "true"},
        }).encode()
        request = urllib.request.Request(
            f"{base_url}/v1/tasks",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(request, timeout=1)
        except urllib.error.HTTPError as exc:
            assert exc.code == 400
            err_data = json.loads(exc.read().decode())
            assert "KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS" in err_data.get("error", "")
            assert "true" not in err_data.get("error", "")
        else:
            raise AssertionError("expected HTTP 400 when attempting to enable private networks")
        assert not service.ingest._private_networks_allowed()
    finally:
        service.ingest.reset_trusted_service_config()
        server.shutdown()
        server.server_close()
        thread.join(timeout=1)


def test_http_allows_legitimate_azure_environment(monkeypatch):
    observed = {}

    def execute(_payload, *args, **kwargs):
        observed["client_id"] = os.environ.get("AZURE_CLIENT_ID")
        observed["client_secret"] = os.environ.get("AZURE_CLIENT_SECRET")
        return {"documents": 2, "chunks": 4, "sourceDigest": "sha256:legit"}

    monkeypatch.setattr(service.ingest, "ingest_task", execute)
    handler = type("TestIngestHandler", (service.IngestHandler,), {"registry": service.TaskRegistry()})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_port}"
    try:
        body = json.dumps({
            "id": "task-legit",
            "payload": {"kb": {"name": "docs"}},
            "environment": {
                "AZURE_CLIENT_ID": "azure-client-123",
                "AZURE_CLIENT_SECRET": "azure-secret-456",
            },
        }).encode()
        request = urllib.request.Request(
            f"{base_url}/v1/tasks",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=1) as response:
            assert response.status in (200, 202)

        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            with urllib.request.urlopen(f"{base_url}/v1/tasks/task-legit", timeout=1) as response:
                status = json.load(response)
            if status["state"] == "succeeded":
                break
            time.sleep(0.01)

        assert status["state"] == "succeeded"
        assert observed["client_id"] == "azure-client-123"
        assert observed["client_secret"] == "azure-secret-456"
        assert "AZURE_CLIENT_ID" not in os.environ
        assert "AZURE_CLIENT_SECRET" not in os.environ
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=1)


def test_concurrent_tasks_do_not_leak_environment(monkeypatch):
    observed = {}
    barrier = threading.Barrier(2)

    def execute(payload, *args, **kwargs):
        task_name = payload["kb"]["name"]
        client_id = os.environ.get("AZURE_CLIENT_ID")
        # Sleep to ensure execution windows overlap across threads
        time.sleep(0.05)
        # Ensure under execution lock no other task has leaked its environment
        assert os.environ.get("AZURE_CLIENT_ID") == client_id
        observed[task_name] = client_id
        return {"documents": 1, "chunks": 1, "sourceDigest": f"sha256:{task_name}"}

    monkeypatch.setattr(service.ingest, "ingest_task", execute)
    registry = service.TaskRegistry()

    def submit_worker(task_id, client_id):
        barrier.wait()
        return registry.submit(
            task_id,
            {"kb": {"name": task_id}},
            {"AZURE_CLIENT_ID": client_id},
        )

    t1 = threading.Thread(target=submit_worker, args=("task-1", "client-1"))
    t2 = threading.Thread(target=submit_worker, args=("task-2", "client-2"))
    t1.start()
    t2.start()
    t1.join()
    t2.join()

    res1 = _wait_for_terminal(registry, "task-1")
    res2 = _wait_for_terminal(registry, "task-2")

    assert res1.state == "succeeded"
    assert res2.state == "succeeded"
    assert observed["task-1"] == "client-1"
    assert observed["task-2"] == "client-2"
    assert "AZURE_CLIENT_ID" not in os.environ


def test_embedder_inherits_daemon_openai_api_key_with_empty_request_env(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", "daemon-secret-key")
    created_client_kwargs = {}

    def fake_openai(**kwargs):
        created_client_kwargs.update(kwargs)
        return MagicMock()

    monkeypatch.setattr(service.ingest, "OpenAI", fake_openai)
    service.ingest._build_embedder({"provider": "openai"}, 1536, env={})
    assert created_client_kwargs.get("api_key") == "daemon-secret-key"


def test_http_validates_federated_token_file(monkeypatch, tmp_path):
    allowed_dir = tmp_path / "allowed_tokens"
    allowed_dir.mkdir()
    valid_token = allowed_dir / "token.jwt"
    valid_token.write_text("dummy-token", encoding="utf-8")

    outside_dir = tmp_path / "outside"
    outside_dir.mkdir()
    outside_file = outside_dir / "token.jwt"
    outside_file.write_text("evil-token", encoding="utf-8")

    symlink_file = allowed_dir / "symlink_escape.jwt"
    try:
        symlink_file.symlink_to(outside_file)
        has_symlink = True
    except (OSError, NotImplementedError):
        has_symlink = False

    monkeypatch.setenv("KOGNITION_ALLOWED_TOKEN_FILE_DIR", str(allowed_dir))

    # 1. Relative path -> rejected
    valid, _ = service._validate_environment({"AZURE_FEDERATED_TOKEN_FILE": "relative/token.jwt"})
    assert not valid

    # 2. Path with '..' -> rejected
    valid, _ = service._validate_environment({"AZURE_FEDERATED_TOKEN_FILE": f"{allowed_dir}/../outside/token.jwt"})
    assert not valid

    # 3. Path outside allowed directory -> rejected
    valid, _ = service._validate_environment({"AZURE_FEDERATED_TOKEN_FILE": str(outside_file)})
    assert not valid

    # 4. Symlink escaping allowed directory -> rejected (if OS allows symlink creation)
    if has_symlink:
        valid, _ = service._validate_environment({"AZURE_FEDERATED_TOKEN_FILE": str(symlink_file)})
        assert not valid

    # 5. Valid path within allowed directory -> accepted
    valid, err = service._validate_environment({"AZURE_FEDERATED_TOKEN_FILE": str(valid_token)})
    assert valid, f"expected valid token file, got error: {err}"


def test_http_validates_client_secret_strictness():
    # Whitespace rejected
    valid, _ = service._validate_environment({"AZURE_CLIENT_SECRET": "secret with space"})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_CLIENT_SECRET": "secret\twithtab"})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_CLIENT_SECRET": "secret\nwithnewline"})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_CLIENT_SECRET": "secret\rwithcr"})
    assert not valid

    # Non-printable and control characters rejected
    valid, _ = service._validate_environment({"AZURE_CLIENT_SECRET": "secret\x00null"})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_CLIENT_SECRET": "secret\x07bell"})
    assert not valid

    # Valid secret accepted
    valid, err = service._validate_environment({"AZURE_CLIENT_SECRET": "my-secret_123.ABC~/+"})
    assert valid, f"expected valid secret, got: {err}"


def test_http_validates_unstripped_exact_values():
    valid, _ = service._validate_environment({"AZURE_CREDENTIAL_TYPE": "client-secret "})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_CREDENTIAL_TYPE": " client-secret"})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_TENANT_ID": " tenant-123"})
    assert not valid
    valid, _ = service._validate_environment({"AZURE_CLIENT_ID": "client-123 "})
    assert not valid

