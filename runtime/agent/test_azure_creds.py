# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Unit tests for azure_creds credential selection.

The azure.identity credential classes are monkeypatched with recording fakes so
no real Azure calls are made; only the selection logic is exercised.
"""

from __future__ import annotations

import azure.identity as ai

from azure_creds import azure_ad_token_provider, build_azure_credential


class _FakeCred:
    kind = "base"

    def __init__(self, **kwargs):
        self.kwargs = kwargs


class _ClientSecret(_FakeCred):
    kind = "client-secret"


class _Workload(_FakeCred):
    kind = "workload-identity"


class _Managed(_FakeCred):
    kind = "managed-identity"


class _Default(_FakeCred):
    kind = "default"


def _patch_all(monkeypatch):
    monkeypatch.setattr(ai, "ClientSecretCredential", _ClientSecret)
    monkeypatch.setattr(ai, "WorkloadIdentityCredential", _Workload)
    monkeypatch.setattr(ai, "ManagedIdentityCredential", _Managed)
    monkeypatch.setattr(ai, "DefaultAzureCredential", _Default)


def _clear_env(monkeypatch):
    for k in (
        "AZURE_CREDENTIAL_TYPE",
        "AZURE_TENANT_ID",
        "AZURE_CLIENT_ID",
        "AZURE_CLIENT_SECRET",
        "AZURE_FEDERATED_TOKEN_FILE",
    ):
        monkeypatch.delenv(k, raising=False)


def test_client_secret_when_secret_present(monkeypatch):
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)
    monkeypatch.setenv("AZURE_TENANT_ID", "t")
    monkeypatch.setenv("AZURE_CLIENT_ID", "c")
    monkeypatch.setenv("AZURE_CLIENT_SECRET", "s")
    cred = build_azure_credential()
    assert cred.kind == "client-secret"
    assert cred.kwargs == {"tenant_id": "t", "client_id": "c", "client_secret": "s"}


def test_default_when_no_secret(monkeypatch):
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)
    cred = build_azure_credential()
    assert cred.kind == "default"


def test_explicit_managed_identity_user_assigned(monkeypatch):
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)
    monkeypatch.setenv("AZURE_CREDENTIAL_TYPE", "managed-identity")
    monkeypatch.setenv("AZURE_CLIENT_ID", "uami-123")
    cred = build_azure_credential()
    assert cred.kind == "managed-identity"
    assert cred.kwargs == {"client_id": "uami-123"}


def test_explicit_managed_identity_system_assigned(monkeypatch):
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)
    monkeypatch.setenv("AZURE_CREDENTIAL_TYPE", "managed-identity")
    cred = build_azure_credential()
    assert cred.kind == "managed-identity"
    assert cred.kwargs == {}


def test_explicit_workload_identity(monkeypatch):
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)
    monkeypatch.setenv("AZURE_CREDENTIAL_TYPE", "workload-identity")
    monkeypatch.setenv("AZURE_TENANT_ID", "t")
    monkeypatch.setenv("AZURE_CLIENT_ID", "c")
    monkeypatch.setenv("AZURE_FEDERATED_TOKEN_FILE", "/var/run/secrets/token")
    cred = build_azure_credential()
    assert cred.kind == "workload-identity"
    assert cred.kwargs["token_file_path"] == "/var/run/secrets/token"


def test_explicit_client_secret_type_overrides_missing_secret(monkeypatch):
    # AZURE_CREDENTIAL_TYPE=client-secret forces client-secret even if the
    # secret env var is empty (surfacing a config error rather than silently
    # falling back).
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)
    monkeypatch.setenv("AZURE_CREDENTIAL_TYPE", "client-secret")
    monkeypatch.setenv("AZURE_TENANT_ID", "t")
    monkeypatch.setenv("AZURE_CLIENT_ID", "c")
    cred = build_azure_credential()
    assert cred.kind == "client-secret"


def test_token_provider_returns_token(monkeypatch):
    _patch_all(monkeypatch)
    _clear_env(monkeypatch)

    class _WithToken(_FakeCred):
        def get_token(self, scope):
            assert scope == "https://cognitiveservices.azure.com/.default"
            return type("T", (), {"token": "abc.def"})()

    monkeypatch.setattr(ai, "DefaultAzureCredential", _WithToken)
    provider = azure_ad_token_provider()
    assert provider() == "abc.def"
