# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Azure credential selection for the agent runtime.

Mirrors the Go Foundry driver
(internal/cpi/drivers/foundry/token_provider.go) so agents can authenticate to
Azure OpenAI / Foundry with a managed identity or workload-identity federation
instead of an explicit client secret.

Selection is driven by ``AZURE_CREDENTIAL_TYPE``:

  - ``client-secret``     ClientSecretCredential (tenant/client/secret)
  - ``workload-identity`` WorkloadIdentityCredential (federated token file)
  - ``managed-identity``  ManagedIdentityCredential (optional user-assigned id)
  - ``default`` / unset   ClientSecretCredential when AZURE_CLIENT_SECRET is set,
                          otherwise DefaultAzureCredential (which itself covers
                          managed identity, workload identity, environment, and
                          the Azure CLI).
"""

from __future__ import annotations

import logging
import os
from typing import Any, Callable

logger = logging.getLogger(__name__)

COGNITIVE_SERVICES_SCOPE = "https://cognitiveservices.azure.com/.default"


def build_azure_credential() -> Any:
    """Construct an azure-identity credential based on the environment."""
    cred_type = os.getenv("AZURE_CREDENTIAL_TYPE", "").strip().lower()
    tenant_id = os.getenv("AZURE_TENANT_ID", "")
    client_id = os.getenv("AZURE_CLIENT_ID", "")
    client_secret = os.getenv("AZURE_CLIENT_SECRET", "")

    if cred_type == "client-secret" or (cred_type == "" and client_secret):
        from azure.identity import ClientSecretCredential

        logger.info("azure auth: client-secret")
        return ClientSecretCredential(
            tenant_id=tenant_id, client_id=client_id, client_secret=client_secret
        )

    if cred_type == "workload-identity":
        from azure.identity import WorkloadIdentityCredential

        logger.info("azure auth: workload-identity")
        return WorkloadIdentityCredential(
            tenant_id=tenant_id or None,
            client_id=client_id or None,
            token_file_path=os.getenv("AZURE_FEDERATED_TOKEN_FILE"),
        )

    if cred_type == "managed-identity":
        from azure.identity import ManagedIdentityCredential

        logger.info("azure auth: managed-identity")
        if client_id:
            return ManagedIdentityCredential(client_id=client_id)
        return ManagedIdentityCredential()

    from azure.identity import DefaultAzureCredential

    logger.info("azure auth: default (DefaultAzureCredential)")
    return DefaultAzureCredential()


def azure_ad_token_provider(
    scope: str = COGNITIVE_SERVICES_SCOPE,
) -> Callable[[], str]:
    """Return a callable yielding a bearer token for ``scope``."""
    credential = build_azure_credential()

    def get_token() -> str:
        return credential.get_token(scope).token

    return get_token
