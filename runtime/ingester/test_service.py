# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import os
import threading
import time
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

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

    def execute(payload):
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
        lambda _payload: (_ for _ in ()).throw(RuntimeError("source unavailable")),
    )
    registry = service.TaskRegistry()
    registry.submit("failed-task", {"credential": "sensitive"})
    result = _wait_for_terminal(registry, "failed-task")
    assert result.state == "failed"
    assert result.error == "source unavailable"
    assert "sensitive" not in json.dumps(vars(result))


def test_registry_scopes_task_environment(monkeypatch):
    monkeypatch.setenv("AZURE_CLIENT_ID", "service-default")
    monkeypatch.delenv("AZURE_CLIENT_SECRET", raising=False)

    def execute(_payload):
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
        lambda _payload: {"documents": 1, "chunks": 3, "sourceDigest": "sha256:http"},
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