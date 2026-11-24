# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""kognition-kb-ingester entrypoint.

Reads a task spec from KOGNITION_INGEST_INPUT (JSON), ingests every source
into pgvector, and prints a one-line JSON summary on stdout.

Input schema (see internal/controller/knowledge_base_reconciler.go for the
producer side):

    {
      "kb": {"namespace": "default", "name": "orbital-notes-kb"},
      "vectorStore": {
        "dsn": "postgres://...",
        "table": "kognition_kb_chunks",
        "dimension": 1536
      },
      "chunking": {"size": 512, "overlap": 64, "mode": "recursive"},
      "embedding": {
        "provider": "openai" | "azure_openai",
        "model": "text-embedding-3-small",
        "apiKey": "...",
        "baseUrl": "https://...",          # optional
        "apiVersion": "2024-08-01-preview" # azure_openai only
      },
      "sources": [
        {"type": "inline", "data": "...", "uri": "inline://<hash>"},
        {"type": "url",    "uri": "https://..."},
        {"type": "blob",   "provider": "azure",
                             "bucket": "<container>",
                             "prefix": "docs/",
                             "endpoint": "https://<acct>.blob.core.windows.net",
                             "include": ["*.md", "*.pdf"],
                             "exclude": ["**/drafts/*"],
                             "credentials": {"connectionString": "...",
                                              "accountName": "...",
                                              "accountKey": "...",
                                              "sasToken": "..."}},
        {"type": "blob",   "provider": "s3",
                             "bucket": "<bucket>", "prefix": "docs/",
                             "region": "us-east-1", "endpoint": "...",
                             "include": ["*.md"], "exclude": [],
                             "credentials": {"accessKeyId": "...",
                                              "secretAccessKey": "...",
                                              "sessionToken": "..."}}
      ]
    }

Output on stdout (single line):

    {"documents": N, "chunks": M, "sourceDigest": "sha256:..."}
"""

from __future__ import annotations

import fnmatch
import hashlib
import io
import ipaddress
import json
import math
import os
import re
import socket
import subprocess
import sys
import tempfile
import urllib.parse
from dataclasses import dataclass
from typing import Any, Callable, Iterable, Iterator, Protocol
from urllib.parse import urlparse, urlunparse

import httpx
import psycopg
from psycopg import sql
from openai import AzureOpenAI, OpenAI
from pgvector.psycopg import register_vector


# --------------------------- text extraction ---------------------------------


def _extract_text(data: bytes, uri: str) -> str:
    """Best-effort convert raw bytes to text based on file extension."""
    lower = uri.lower().split("?", 1)[0]
    if lower.endswith(".pdf"):
        from pypdf import PdfReader

        reader = PdfReader(io.BytesIO(data))
        pages: list[str] = []
        for page in reader.pages:
            try:
                pages.append(page.extract_text() or "")
            except Exception:
                pages.append("")
        return "\n\n".join(pages)
    # Everything else: treat as UTF-8 text with lenient decoding.
    return data.decode("utf-8", errors="replace")


# --------------------------- source fetchers ---------------------------------


_DEFAULT_MAX_DOCUMENT_BYTES = 10 * 1024 * 1024
_DEFAULT_MAX_DOCUMENTS_PER_SOURCE = 1000
_MAX_URL_REDIRECTS = 5


def _max_document_bytes() -> int:
    raw = os.environ.get("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "")
    try:
        value = int(raw) if raw else _DEFAULT_MAX_DOCUMENT_BYTES
    except ValueError as exc:
        raise ValueError("KOGNITION_INGEST_MAX_DOCUMENT_BYTES must be an integer") from exc
    if value <= 0:
        raise ValueError("KOGNITION_INGEST_MAX_DOCUMENT_BYTES must be greater than zero")
    return value


_SERVICE_INDEXER_EXECUTABLE: str | None = None
_SERVICE_ALLOW_PRIVATE_NETWORKS: bool | None = None
_SERVICE_TRUSTED_EMBEDDING_HOSTS: frozenset[str] = frozenset()


def set_trusted_service_config(
    *,
    indexer_executable: str | None = None,
    allow_private_networks: bool | None = None,
    trusted_embedding_hosts: Iterable[str] | None = None,
) -> None:
    """Configure trusted service settings at daemon startup."""
    global _SERVICE_INDEXER_EXECUTABLE, _SERVICE_ALLOW_PRIVATE_NETWORKS
    global _SERVICE_TRUSTED_EMBEDDING_HOSTS
    if indexer_executable is not None:
        _SERVICE_INDEXER_EXECUTABLE = indexer_executable
    if allow_private_networks is not None:
        _SERVICE_ALLOW_PRIVATE_NETWORKS = allow_private_networks
    if trusted_embedding_hosts is not None:
        _SERVICE_TRUSTED_EMBEDDING_HOSTS = frozenset(
            host.strip().lower() for host in trusted_embedding_hosts if host.strip()
        )


def reset_trusted_service_config() -> None:
    """Reset trusted service settings to unconfigured state (for testing)."""
    global _SERVICE_INDEXER_EXECUTABLE, _SERVICE_ALLOW_PRIVATE_NETWORKS
    global _SERVICE_TRUSTED_EMBEDDING_HOSTS
    _SERVICE_INDEXER_EXECUTABLE = None
    _SERVICE_ALLOW_PRIVATE_NETWORKS = None
    _SERVICE_TRUSTED_EMBEDDING_HOSTS = frozenset()


def _private_networks_allowed() -> bool:
    if _SERVICE_ALLOW_PRIVATE_NETWORKS is not None:
        return _SERVICE_ALLOW_PRIVATE_NETWORKS
    return os.environ.get("KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS", "").lower() == "true"


def _max_documents_per_source() -> int:
    raw = os.environ.get("KOGNITION_INGEST_MAX_DOCUMENTS_PER_SOURCE", "")
    try:
        value = int(raw) if raw else _DEFAULT_MAX_DOCUMENTS_PER_SOURCE
    except ValueError as exc:
        raise ValueError("KOGNITION_INGEST_MAX_DOCUMENTS_PER_SOURCE must be an integer") from exc
    if value <= 0:
        raise ValueError("KOGNITION_INGEST_MAX_DOCUMENTS_PER_SOURCE must be greater than zero")
    return value


def _validate_remote_url(uri: str, *, allow_private_http: bool = False) -> None:
    parsed = urlparse(uri)
    allowed_schemes = {"https"}
    if allow_private_http and _private_networks_allowed():
        allowed_schemes.add("http")
    if parsed.scheme.lower() not in allowed_schemes or not parsed.hostname:
        raise ValueError("URL sources must use HTTPS with a valid hostname")
    if parsed.username or parsed.password:
        raise ValueError("URL sources must not include credentials")
    if _private_networks_allowed():
        return
    try:
        literal_address = ipaddress.ip_address(parsed.hostname)
    except ValueError:
        literal_address = None
    if literal_address is not None:
        if not literal_address.is_global:
            raise ValueError("URL source hostname must resolve only to publicly routable addresses")
        return
    try:
        addresses = {
            item[4][0]
            for item in socket.getaddrinfo(
                parsed.hostname,
                parsed.port or 443,
                type=socket.SOCK_STREAM,
            )
        }
    except socket.gaierror as exc:
        raise ValueError(f"failed to resolve URL source hostname: {parsed.hostname}") from exc
    if not addresses or any(not ipaddress.ip_address(address).is_global for address in addresses):
        raise ValueError("URL source hostname must resolve only to publicly routable addresses")


def _read_http_response(response: httpx.Response, limit: int) -> bytes:
    content_length = response.headers.get("content-length")
    if content_length:
        try:
            if int(content_length) > limit:
                raise ValueError(f"document exceeds {limit} bytes")
        except ValueError as exc:
            if "exceeds" in str(exc):
                raise
    data = bytearray()
    for chunk in response.iter_bytes():
        if len(data) + len(chunk) > limit:
            raise ValueError(f"document exceeds {limit} bytes")
        data.extend(chunk)
    return bytes(data)


def _read_limited_stream(stream: Any, limit: int) -> bytes:
    data = bytearray()
    while True:
        chunk = stream.read(min(64 * 1024, limit - len(data) + 1))
        if not chunk:
            return bytes(data)
        if len(data) + len(chunk) > limit:
            raise ValueError(f"document exceeds {limit} bytes")
        data.extend(chunk)


def _fetch_inline(source: dict[str, Any]) -> Iterator[tuple[str, str]]:
    text = source.get("data") or ""
    if len(text.encode("utf-8")) > _max_document_bytes():
        raise ValueError(f"document exceeds {_max_document_bytes()} bytes")
    uri = source.get("uri") or f"inline://{hashlib.sha256(text.encode()).hexdigest()[:16]}"
    yield text, uri


def _fetch_url(source: dict[str, Any]) -> Iterator[tuple[str, str]]:
    uri = source["uri"]
    current_uri = uri
    limit = _max_document_bytes()
    with httpx.Client(follow_redirects=False, timeout=30.0) as client:
        for redirect_count in range(_MAX_URL_REDIRECTS + 1):
            _validate_remote_url(current_uri, allow_private_http=True)
            with client.stream("GET", current_uri) as response:
                if response.is_redirect:
                    if redirect_count == _MAX_URL_REDIRECTS:
                        raise ValueError(f"URL source exceeded {_MAX_URL_REDIRECTS} redirects")
                    location = response.headers.get("location")
                    if not location:
                        raise ValueError("URL source redirect is missing a Location header")
                    current_uri = str(response.url.join(location))
                    continue
                response.raise_for_status()
                data = _read_http_response(response, limit)
                yield _extract_text(data, current_uri), uri
                return


def _matches_filters(key: str, include: list[str], exclude: list[str]) -> bool:
    if include and not any(fnmatch.fnmatch(key, pat) for pat in include):
        return False
    if any(fnmatch.fnmatch(key, pat) for pat in (exclude or [])):
        return False
    return True


def _fetch_azure_blob(source: dict[str, Any]) -> Iterator[tuple[str, str]]:
    from azure.identity import DefaultAzureCredential
    from azure.storage.blob import ContainerClient

    container = source.get("bucket")
    if not container:
        raise ValueError("azure blob source requires 'bucket' (container name)")
    prefix = source.get("prefix") or ""
    include = source.get("include") or []
    exclude = source.get("exclude") or []
    creds = source.get("credentials") or {}
    endpoint = source.get("endpoint") or ""
    account_name = creds.get("accountName")
    limit = _max_document_bytes()
    max_documents = _max_documents_per_source()
    document_count = 0

    if endpoint:
        _validate_remote_url(endpoint, allow_private_http=True)
    if creds.get("connectionString"):
        fields = dict(
            part.split("=", 1)
            for part in creds["connectionString"].split(";")
            if "=" in part
        )
        if fields.get("BlobEndpoint"):
            _validate_remote_url(fields["BlobEndpoint"], allow_private_http=True)

    if creds.get("connectionString"):
        client = ContainerClient.from_connection_string(
            creds["connectionString"], container_name=container
        )
    elif creds.get("sasToken"):
        if not endpoint and not account_name:
            raise ValueError("azure blob with sasToken requires 'endpoint' or credentials.accountName")
        base = endpoint or f"https://{account_name}.blob.core.windows.net"
        client = ContainerClient.from_container_url(
            f"{base.rstrip('/')}/{container}?{creds['sasToken'].lstrip('?')}"
        )
    elif creds.get("accountKey"):
        if not endpoint and not account_name:
            raise ValueError("azure blob with accountKey requires 'endpoint' or credentials.accountName")
        base = endpoint or f"https://{account_name}.blob.core.windows.net"
        client = ContainerClient(
            account_url=base,
            container_name=container,
            credential=creds["accountKey"],
        )
    else:
        # Workload identity / managed identity fallback.
        if not endpoint and not account_name:
            raise ValueError("azure blob without secret credentials requires 'endpoint' or credentials.accountName")
        base = endpoint or f"https://{account_name}.blob.core.windows.net"
        client = ContainerClient(
            account_url=base,
            container_name=container,
            credential=DefaultAzureCredential(),
        )

    for blob in client.list_blobs(name_starts_with=prefix):
        if not _matches_filters(blob.name, include, exclude):
            continue
        document_count += 1
        if document_count > max_documents:
            raise ValueError(f"source exceeds {max_documents} documents")
        if getattr(blob, "size", 0) > limit:
            raise ValueError(f"document exceeds {limit} bytes")
        downloader = client.download_blob(blob.name)
        data = bytearray()
        for chunk in downloader.chunks():
            if len(data) + len(chunk) > limit:
                raise ValueError(f"document exceeds {limit} bytes")
            data.extend(chunk)
        uri = f"{client.url.rstrip('/')}/{urllib.parse.quote(blob.name)}"
        yield _extract_text(bytes(data), blob.name), uri


def _fetch_s3_blob(source: dict[str, Any]) -> Iterator[tuple[str, str]]:
    import boto3

    bucket = source.get("bucket")
    if not bucket:
        raise ValueError("s3 blob source requires 'bucket'")
    prefix = source.get("prefix") or ""
    include = source.get("include") or []
    exclude = source.get("exclude") or []
    creds = source.get("credentials") or {}
    limit = _max_document_bytes()
    max_documents = _max_documents_per_source()
    document_count = 0

    if source.get("endpoint"):
        _validate_remote_url(source["endpoint"], allow_private_http=True)

    kwargs: dict[str, Any] = {}
    if source.get("region"):
        kwargs["region_name"] = source["region"]
    if creds.get("accessKeyId"):
        kwargs["aws_access_key_id"] = creds["accessKeyId"]
        kwargs["aws_secret_access_key"] = creds.get("secretAccessKey", "")
        if creds.get("sessionToken"):
            kwargs["aws_session_token"] = creds["sessionToken"]
    if source.get("endpoint"):
        kwargs["endpoint_url"] = source["endpoint"]
        # S3-compatible stores reached by a custom endpoint (MinIO, Ceph, some
        # R2 setups) don't support virtual-hosted-style buckets over an
        # arbitrary host, so force path-style addressing.
        from botocore.config import Config

        kwargs["config"] = Config(s3={"addressing_style": "path"})
    client = boto3.client("s3", **kwargs)

    paginator = client.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
        for obj in page.get("Contents") or []:
            key = obj["Key"]
            if key.endswith("/"):
                continue
            if not _matches_filters(key, include, exclude):
                continue
            document_count += 1
            if document_count > max_documents:
                raise ValueError(f"source exceeds {max_documents} documents")
            response = client.get_object(Bucket=bucket, Key=key)
            if response.get("ContentLength", 0) > limit:
                raise ValueError(f"document exceeds {limit} bytes")
            body = _read_limited_stream(response["Body"], limit)
            uri = f"s3://{bucket}/{urllib.parse.quote(key, safe='/')}"
            yield _extract_text(body, key), uri


# Text-extractable extensions ingested from a Git repo (which carries no
# include/exclude filters). Binaries and code are skipped to keep a docs KB
# focused; PDFs are handled by _extract_text.
_GIT_TEXT_EXTS = (
    ".md", ".markdown", ".mdx", ".txt", ".rst", ".adoc", ".pdf",
)

_GIT_TEXT_BASENAMES = {
    "readme",
    "readme.md",
    "readme.txt",
    "changelog",
    "changelog.md",
    "changes",
    "changes.md",
    "contributing",
    "contributing.md",
    "license",
    "license.md",
    "copying",
    "notice",
}


def _is_git_text_candidate(filename: str) -> bool:
    lower = filename.lower()
    if lower.endswith(_GIT_TEXT_EXTS):
        return True
    return lower in _GIT_TEXT_BASENAMES


def _git_subpath(raw: Any) -> str:
    subpath = (raw or "").strip("/")
    parts = re.split(r"[\\/]", subpath)
    if os.path.isabs(subpath) or ".." in parts:
        raise ValueError("git source path must stay inside the repository")
    return subpath


def _is_within(path: str, root: str) -> bool:
    try:
        return os.path.commonpath([path, root]) == root
    except ValueError:
        return False


def _fetch_git(source: dict[str, Any]) -> Iterator[tuple[str, str]]:
    import subprocess
    import tempfile

    url = source["url"]
    _validate_remote_url(url)
    branch = source.get("branch") or "main"
    subpath = _git_subpath(source.get("path"))
    limit = _max_document_bytes()
    max_documents = _max_documents_per_source()
    document_count = 0
    with tempfile.TemporaryDirectory() as tmp:
        try:
            subprocess.run(
                ["git", "clone", "--depth", "1", "--branch", branch, "--", url, tmp],
                check=True,
                capture_output=True,
                text=True,
                timeout=180,
            )
        except subprocess.CalledProcessError as exc:  # surface a helpful error
            raise RuntimeError(f"git clone failed: {(exc.stderr or '').strip() or exc}") from exc
        clone_root = os.path.realpath(tmp)
        root = os.path.realpath(os.path.join(clone_root, subpath)) if subpath else clone_root
        if not _is_within(root, clone_root):
            raise ValueError("git source path must stay inside the repository")
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames[:] = [d for d in dirnames if d != ".git"]
            for fn in sorted(filenames):
                if not _is_git_text_candidate(fn):
                    continue
                full = os.path.join(dirpath, fn)
                # Cloned content is untrusted, so a link must never lead to a file outside the clone.
                if os.path.islink(full):
                    continue
                try:
                    if os.path.getsize(full) > limit:
                        raise ValueError(f"document exceeds {limit} bytes")
                    with open(full, "rb") as fh:
                        data = _read_limited_stream(fh, limit)
                except OSError:
                    continue
                document_count += 1
                if document_count > max_documents:
                    raise ValueError(f"source exceeds {max_documents} documents")
                text = _extract_text(data, fn)
                if text.strip():
                    rel = os.path.relpath(full, clone_root)
                    yield text, f"git+{url}@{branch}#{rel}"


_FETCHERS = {
    ("inline", None): _fetch_inline,
    ("url", None): _fetch_url,
    ("git", None): _fetch_git,
    ("blob", "azure"): _fetch_azure_blob,
    ("blob", "s3"): _fetch_s3_blob,
}



def _fetch(source: dict[str, Any]) -> Iterator[tuple[str, str]]:
    stype = source.get("type")
    provider = source.get("provider") if stype == "blob" else None
    fetcher = _FETCHERS.get((stype, provider))
    if fetcher is None:
        raise ValueError(f"unsupported source: type={stype} provider={provider}")
    return fetcher(source)


# --------------------------- chunking ----------------------------------------


def _chunk(text: str, cfg: dict[str, Any]) -> list[str]:
    size = int(cfg.get("size") or 512)
    overlap = int(cfg.get("overlap") or 0)
    mode = (cfg.get("mode") or "recursive").lower()
    text = text.strip()
    if not text:
        return []
    if mode == "fixed":
        step = max(1, size - overlap)
        return [text[i : i + size] for i in range(0, len(text), step) if text[i : i + size]]
    # default: recursive
    from langchain_text_splitters import RecursiveCharacterTextSplitter

    splitter = RecursiveCharacterTextSplitter(chunk_size=size, chunk_overlap=overlap)
    return [c for c in splitter.split_text(text) if c.strip()]


# --------------------------- embeddings --------------------------------------


@dataclass
class Embedder:
    client: Any
    model: str
    provider: str
    dimension: int
    requested_dimensions: int | None = None

    def embed(self, texts: list[str]) -> list[list[float]]:
        if not texts:
            return []
        if self.provider == "deterministic":
            vectors: list[list[float]] = []
            for text in texts:
                raw = hashlib.shake_256(text.encode("utf-8")).digest(self.dimension * 2)
                vector = [
                    (int.from_bytes(raw[index : index + 2], "big") / 32767.5) - 1.0
                    for index in range(0, len(raw), 2)
                ]
                vectors.append(vector)
            return vectors
        # OpenAI + Azure OpenAI share the .embeddings.create shape.
        options = {}
        if self.requested_dimensions is not None:
            options["dimensions"] = self.requested_dimensions
        resp = self.client.embeddings.create(model=self.model, input=texts, **options)
        vectors = [d.embedding for d in resp.data]
        # Sanity check dimension against configured KB dimension.
        for v in vectors:
            if len(v) != self.dimension:
                raise ValueError(
                    f"embedding dimension mismatch: model returned {len(v)}, "
                    f"KnowledgeBaseClass configured {self.dimension}"
                )
        return vectors


def _build_embedder(cfg: dict[str, Any], dimension: int, env: dict[str, str] | None = None) -> Embedder:
    provider = (cfg.get("provider") or "openai").lower()
    model = cfg.get("model") or "text-embedding-3-small"
    active_env = {**os.environ, **env} if env is not None else os.environ
    base_url = cfg.get("baseUrl")
    requested_dimensions = cfg.get("dimensions")
    if requested_dimensions is not None and (
        isinstance(requested_dimensions, bool)
        or not isinstance(requested_dimensions, int)
        or requested_dimensions != dimension
    ):
        raise ValueError("invocation profile embedding dimensions must match the KnowledgeBase dimension")
    if provider == "deterministic":
        return Embedder(client=None, model=model, provider=provider, dimension=dimension)
    ambient_credentials_allowed = _check_embedding_endpoint(base_url, provider)
    api_key = cfg.get("apiKey") or (
        active_env.get("OPENAI_API_KEY") if ambient_credentials_allowed else ""
    ) or ""
    use_azure_ad = provider == "azure_openai" and not api_key and bool(active_env.get("AZURE_TENANT_ID"))
    if use_azure_ad and not ambient_credentials_allowed:
        raise ValueError(
            "Azure AD credentials may only be used with Azure OpenAI or operator-trusted embedding endpoints"
        )
    if not api_key and not use_azure_ad:
        raise ValueError(
            "embedding endpoint requires an API key; daemon credentials are only sent to trusted endpoints"
        )
    # Credentials must never follow a redirect to another host.
    http_client = httpx.Client(follow_redirects=False)
    if provider == "azure_openai":
        endpoint = _azure_resource_root(base_url or "")
        api_version = cfg.get("apiVersion") or "2024-08-01-preview"
        # Without an explicit apiKey, mint an AAD token from the Azure
        # identity supplied for this task (matches runtime/agent/providers.py).
        if use_azure_ad:
            client = AzureOpenAI(
                azure_ad_token_provider=_azure_ad_token_provider(env=active_env),
                azure_endpoint=endpoint,
                api_version=api_version,
                http_client=http_client,
            )
        else:
            client = AzureOpenAI(
                api_key=api_key,
                azure_endpoint=endpoint,
                api_version=api_version,
                http_client=http_client,
            )
    else:
        kw: dict[str, Any] = {"api_key": api_key, "http_client": http_client}
        if base_url:
            kw["base_url"] = base_url
        client = OpenAI(**kw)
    return Embedder(client=client, model=model, provider=provider, dimension=dimension, requested_dimensions=requested_dimensions)


_OPENAI_DEFAULT_HOSTS = frozenset({"api.openai.com"})
_AZURE_OPENAI_HOST_SUFFIXES = (
    ".openai.azure.com",
    ".cognitiveservices.azure.com",
    ".services.ai.azure.com",
)


def _check_embedding_endpoint(base_url: str | None, provider: str) -> bool:
    """Validate a caller-selected embedding endpoint.

    Returns True only when daemon-held credentials may be sent to it: the
    provider's own service or a host the operator trusted at startup. Any
    other endpoint must pass SSRF validation and receives caller credentials only.
    """
    if not base_url:
        return provider != "azure_openai"
    parsed = urlparse(base_url)
    host = (parsed.hostname or "").lower()
    scheme = parsed.scheme.lower()
    if host and not (parsed.username or parsed.password):
        if host in _SERVICE_TRUSTED_EMBEDDING_HOSTS and scheme in {"http", "https"}:
            return True
        if scheme == "https":
            if provider == "azure_openai" and host.endswith(_AZURE_OPENAI_HOST_SUFFIXES):
                return True
            if provider != "azure_openai" and host in _OPENAI_DEFAULT_HOSTS:
                return True
    _validate_remote_url(base_url, allow_private_http=True)
    return False


def _azure_resource_root(endpoint: str) -> str:
    """Strip `/openai(/...)?` and `/api/projects(/<name>)?` so the OpenAI SDK
    can build the correct Azure URL itself. Mirrors runtime/agent/providers.py.
    """
    if not endpoint:
        return endpoint
    parsed = urlparse(endpoint)
    if not parsed.scheme or not parsed.netloc:
        return endpoint
    path = re.sub(r"(?i)/openai(/.*)?$", "", parsed.path or "")
    path = re.sub(r"(?i)/api/projects(/[^/]+)?$", "", path)
    return urlunparse((parsed.scheme, parsed.netloc, path, "", "", ""))


def _build_azure_credential(env: dict[str, str] | None = None):
    """Select an azure-identity credential, mirroring the agent runtime and the
    Go Foundry driver: client-secret, workload-identity, managed-identity, or
    DefaultAzureCredential (default) for secretless auth on Azure. Driven by
    AZURE_CREDENTIAL_TYPE; falls back to a client secret when one is set,
    otherwise DefaultAzureCredential (managed identity / workload identity)."""
    active_env = {**os.environ, **env} if env is not None else os.environ
    cred_type = active_env.get("AZURE_CREDENTIAL_TYPE", "").strip().lower()
    tenant_id = active_env.get("AZURE_TENANT_ID", "")
    client_id = active_env.get("AZURE_CLIENT_ID", "")
    client_secret = active_env.get("AZURE_CLIENT_SECRET", "")

    if cred_type == "client-secret" or (cred_type == "" and client_secret):
        from azure.identity import ClientSecretCredential

        return ClientSecretCredential(
            tenant_id=tenant_id, client_id=client_id, client_secret=client_secret
        )
    if cred_type == "workload-identity":
        from azure.identity import WorkloadIdentityCredential

        return WorkloadIdentityCredential(
            tenant_id=tenant_id or None,
            client_id=client_id or None,
            token_file_path=active_env.get("AZURE_FEDERATED_TOKEN_FILE"),
        )
    if cred_type == "managed-identity":
        from azure.identity import ManagedIdentityCredential

        if client_id:
            return ManagedIdentityCredential(client_id=client_id)
        return ManagedIdentityCredential()

    from azure.identity import DefaultAzureCredential

    # Pass the task identity explicitly; the SDK would otherwise read only the daemon environment.
    identity = {
        "managed_identity_client_id": client_id,
        "workload_identity_client_id": client_id,
        "workload_identity_tenant_id": tenant_id,
    }
    return DefaultAzureCredential(**{name: value for name, value in identity.items() if value})


def _azure_ad_token_provider(
    scope: str = "https://cognitiveservices.azure.com/.default",
    env: dict[str, str] | None = None,
) -> Callable[[], str]:
    """Token provider for Azure OpenAI embeddings. Supports client-secret,
    managed identity, and workload-identity federation via
    _build_azure_credential."""
    credential = _build_azure_credential(env=env)

    def get_token() -> str:
        return credential.get_token(scope).token

    return get_token


# --------------------------- pgvector persistence ----------------------------


_TABLE_NAME_PATTERN = re.compile(r"^[a-z_][a-z0-9_]{0,54}$")


def _validate_table_name(table: str) -> str:
    if not _TABLE_NAME_PATTERN.fullmatch(table):
        raise ValueError("vectorStore.table must match [a-z_][a-z0-9_]{0,54}")
    return table


def _ensure_schema(conn: psycopg.Connection, table: str, dimension: int) -> None:
    """Create the extension and the chunks table if they don't exist."""
    _validate_table_name(table)
    with conn.cursor() as cur:
        cur.execute("CREATE EXTENSION IF NOT EXISTS vector")
        cur.execute(
            sql.SQL("""
            CREATE TABLE IF NOT EXISTS {table} (
                id            BIGSERIAL PRIMARY KEY,
                kb_namespace  TEXT NOT NULL,
                kb_name       TEXT NOT NULL,
                source_uri    TEXT NOT NULL,
                chunk_idx     INT  NOT NULL,
                content       TEXT NOT NULL,
                embedding     vector({dimension}) NOT NULL,
                metadata      JSONB NOT NULL DEFAULT '{{}}',
                ingested_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                UNIQUE(kb_namespace, kb_name, source_uri, chunk_idx)
            )
            """).format(
                table=sql.Identifier(table),
                dimension=sql.Literal(dimension),
            )
        )
        cur.execute(
            sql.SQL("CREATE INDEX IF NOT EXISTS {index} ON {table} (kb_namespace, kb_name)").format(
                index=sql.Identifier(f"{table}_kb_idx"),
                table=sql.Identifier(table),
            )
        )
    conn.commit()


def _delete_existing(conn: psycopg.Connection, table: str, namespace: str, name: str,
                     source_uri: str) -> None:
    with conn.cursor() as cur:
        cur.execute(
            sql.SQL("DELETE FROM {table} WHERE kb_namespace = %s AND kb_name = %s AND source_uri = %s").format(
                table=sql.Identifier(table)
            ),
            (namespace, name, source_uri),
        )


def _insert_chunks(conn: psycopg.Connection, table: str, namespace: str, name: str,
                   source_uri: str, chunks: list[str], embeddings: list[list[float]],
                   metadata: dict[str, Any]) -> None:
    rows: Iterable[tuple[Any, ...]] = (
        (namespace, name, source_uri, i, chunk, emb, json.dumps(metadata))
        for i, (chunk, emb) in enumerate(zip(chunks, embeddings))
    )
    with conn.cursor() as cur:
        cur.executemany(
            sql.SQL("""
            INSERT INTO {table}
                (kb_namespace, kb_name, source_uri, chunk_idx, content, embedding, metadata)
            VALUES (%s, %s, %s, %s, %s, %s, %s)
            """).format(table=sql.Identifier(table)),
            list(rows),
        )


# --------------------------- execution ---------------------------------------


class ChunkSink(Protocol):
    def replace_document(
        self,
        source_uri: str,
        chunks: list[str],
        embeddings: list[list[float]],
        metadata: dict[str, Any],
    ) -> None: ...


@dataclass
class PgvectorChunkSink:
    connection: psycopg.Connection
    table: str
    namespace: str
    knowledge_base: str

    def replace_document(
        self,
        source_uri: str,
        chunks: list[str],
        embeddings: list[list[float]],
        metadata: dict[str, Any],
    ) -> None:
        _delete_existing(
            self.connection, self.table, self.namespace, self.knowledge_base, source_uri
        )
        if chunks:
            _insert_chunks(
                self.connection,
                self.table,
                self.namespace,
                self.knowledge_base,
                source_uri,
                chunks,
                embeddings,
                metadata,
            )


_SPOOL_MAX_BYTES = 64 * 1024 * 1024
_SPOOL_MAX_CHUNKS = 100_000
_SPOOL_MAX_LINE_BYTES = 4 * 1024 * 1024


def _provider_citation(source_uri: str) -> str:
    parsed = urllib.parse.urlsplit(source_uri)
    if parsed.username is not None or parsed.password is not None:
        raise ValueError("provider citations cannot contain user information")
    clean = parsed._replace(query="", fragment="")
    if parsed.scheme in {"http", "https"} and parsed.hostname:
        return urllib.parse.urlunsplit(clean)
    if parsed.scheme == "urn" and parsed.path:
        return urllib.parse.urlunsplit(clean)
    if parsed.scheme in {"s3", "inline", "git+https", "git+http"}:
        identity = parsed.netloc + parsed.path
        if parsed.scheme in {"git+https", "git+http"} and parsed.fragment:
            identity += "#" + parsed.fragment
        if not identity:
            raise ValueError("provider source identity is empty")
        return f"urn:kognition:source:{parsed.scheme.replace('+', '-')}:{urllib.parse.quote(identity, safe='/@:')}"
    raise ValueError("provider source citation scheme is unsupported")


class JsonlChunkSink:
    def __init__(
        self,
        output_path: str,
        *,
        max_bytes: int = _SPOOL_MAX_BYTES,
        max_chunks: int = _SPOOL_MAX_CHUNKS,
    ) -> None:
        if not output_path or max_bytes <= 0 or max_chunks <= 0:
            raise ValueError("invalid provider spool configuration")
        output_directory = os.path.dirname(os.path.abspath(output_path))
        self._output_path = output_path
        self._max_bytes = max_bytes
        self._max_chunks = max_chunks
        self._bytes_written = 0
        self._chunks_written = 0
        self._completed = False
        self._stream = tempfile.NamedTemporaryFile(
            mode="wb", prefix=".kognition-spool-", dir=output_directory, delete=False
        )

    def _write(self, value: dict[str, Any]) -> None:
        encoded = json.dumps(
            value, ensure_ascii=False, allow_nan=False, separators=(",", ":")
        ).encode("utf-8") + b"\n"
        if len(encoded) > _SPOOL_MAX_LINE_BYTES:
            raise ValueError("provider spool record exceeds line limit")
        if self._bytes_written + len(encoded) > self._max_bytes:
            raise ValueError("provider spool exceeds byte limit")
        self._stream.write(encoded)
        self._bytes_written += len(encoded)

    def replace_document(
        self,
        source_uri: str,
        chunks: list[str],
        embeddings: list[list[float]],
        metadata: dict[str, Any],
    ) -> None:
        del metadata
        if self._completed or not source_uri.strip() or len(source_uri.encode("utf-8")) > 4096:
            raise ValueError("invalid provider spool source URI")
        if len(chunks) != len(embeddings):
            raise ValueError("provider spool chunk and embedding counts differ")
        if self._chunks_written + len(chunks) > self._max_chunks:
            raise ValueError("provider spool exceeds chunk limit")
        document_id = hashlib.sha256(source_uri.encode("utf-8")).hexdigest()
        citation = _provider_citation(source_uri)
        for index, (content, vector) in enumerate(zip(chunks, embeddings)):
            if not content.strip() or len(content.encode("utf-8")) > 65536:
                raise ValueError("invalid provider spool chunk content")
            if not vector or len(vector) > 65536 or any(not math.isfinite(value) for value in vector):
                raise ValueError("invalid provider spool embedding")
            self._write(
                {
                    "type": "chunk",
                    "documentId": document_id,
                    "chunkId": f"{index:08d}",
                    "content": content,
                    "sourceUri": citation,
                    "location": "",
                    "vector": vector,
                }
            )
            self._chunks_written += 1

    def complete(self, summary: dict[str, Any]) -> None:
        source_documents = summary.get("documents")
        indexed_chunks = summary.get("chunks")
        source_digest = summary.get("sourceDigest")
        if (
            self._completed
            or not isinstance(source_documents, int)
            or source_documents < 0
            or indexed_chunks != self._chunks_written
            or not isinstance(source_digest, str)
            or not re.fullmatch(r"sha256:[a-f0-9]{64}", source_digest)
        ):
            raise ValueError("invalid provider spool summary")
        self._write(
            {
                "type": "summary",
                "sourceDocuments": source_documents,
                "indexedChunks": indexed_chunks,
                "sourceDigest": source_digest,
            }
        )
        self._stream.flush()
        os.fsync(self._stream.fileno())
        self._stream.close()
        os.replace(self._stream.name, self._output_path)
        self._completed = True

    def abort(self) -> None:
        if not self._stream.closed:
            self._stream.close()
        if not self._completed:
            try:
                os.remove(self._stream.name)
            except FileNotFoundError:
                pass


def _ingest_sources(
    task: dict[str, Any], sink: ChunkSink, embedder: Embedder
) -> dict[str, Any]:
    chunking = task.get("chunking") or {}
    sources = task.get("sources") or []
    digest = hashlib.sha256()
    total_docs = 0
    total_chunks = 0

    for source in sources:
        docs = list(_fetch(source))
        for text, source_uri in docs:
            digest.update(source_uri.encode())
            digest.update(b"\0")
            digest.update(text.encode("utf-8", errors="replace"))

            chunks = _chunk(text, chunking)
            embeddings = embedder.embed(chunks)
            sink.replace_document(
                source_uri,
                chunks,
                embeddings,
                {"sourceType": source.get("type"), "provider": source.get("provider")},
            )
            total_docs += 1
            total_chunks += len(chunks)

    return {
        "documents": total_docs,
        "chunks": total_chunks,
        "sourceDigest": f"sha256:{digest.hexdigest()}",
    }


def ingest_task(task: dict[str, Any], env: dict[str, str] | None = None) -> dict[str, Any]:
    """Execute one validated ingestion task and return aggregate statistics."""
    kb = task.get("kb") or {}
    namespace = kb.get("namespace") or "default"
    name = kb.get("name")
    if not name:
        raise ValueError("kb.name is required")

    provider_index = task.get("providerIndex")
    vs = task.get("vectorStore")
    if provider_index is not None and vs is not None:
        raise ValueError("providerIndex and vectorStore are mutually exclusive")
    if provider_index is not None:
        if not isinstance(provider_index, dict):
            raise ValueError("providerIndex must be an object")
        dimension = int(provider_index.get("dimension") or 0)
        if dimension < 1:
            raise ValueError("providerIndex.dimension must be positive")
        embedder = _build_embedder(task.get("embedding") or {}, dimension, env=env)
        spool_path = os.environ.get(
            "KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE",
            "/var/run/kognition/index/chunks.jsonl",
        )
        sink = JsonlChunkSink(spool_path)
        try:
            summary = _ingest_sources(task, sink, embedder)
            sink.complete(summary)
            return _invoke_provider_indexer()
        except Exception:
            sink.abort()
            raise

    vs = vs or {}
    dsn = vs.get("dsn")
    table = vs.get("table") or "kognition_kb_chunks"
    dimension = int(vs.get("dimension") or 1536)
    if not dsn:
        raise ValueError("vectorStore.dsn is required")
    _validate_table_name(table)

    embedder = _build_embedder(task.get("embedding") or {}, dimension, env=env)
    with psycopg.connect(dsn, autocommit=False) as conn:
        register_vector(conn)
        _ensure_schema(conn, table, dimension)
        summary = _ingest_sources(
            task, PgvectorChunkSink(conn, table, namespace, name), embedder
        )
        conn.commit()
    return summary


def _invoke_provider_indexer() -> dict[str, Any]:
    if _SERVICE_INDEXER_EXECUTABLE is not None:
        executable = _SERVICE_INDEXER_EXECUTABLE
    else:
        executable = os.environ.get(
            "KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE",
            "/app/kognition-knowledge-indexer",
        )
    try:
        completed = subprocess.run(
            [executable],
            check=True,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as exc:
        detail = exc.stderr.strip() or "provider indexing failed"
        raise RuntimeError(detail) from exc
    try:
        result = json.loads(completed.stdout)
    except json.JSONDecodeError as exc:
        raise RuntimeError("provider indexer returned invalid confirmation") from exc
    if not isinstance(result, dict):
        raise RuntimeError("provider indexer returned invalid confirmation")
    return result


# --------------------------- entrypoint --------------------------------------


def main() -> int:
    input_file = os.environ.get("KOGNITION_INGEST_INPUT_FILE")
    if input_file:
        try:
            with open(input_file, "r", encoding="utf-8") as stream:
                raw = stream.read()
        except OSError as exc:
            print(f"failed to read KOGNITION_INGEST_INPUT_FILE: {exc}", file=sys.stderr)
            return 2
    else:
        raw = os.environ.get("KOGNITION_INGEST_INPUT")
    if not raw:
        print("KOGNITION_INGEST_INPUT_FILE or KOGNITION_INGEST_INPUT is required", file=sys.stderr)
        return 2
    try:
        task = json.loads(raw)
    except json.JSONDecodeError as exc:
        print(f"failed to parse KOGNITION_INGEST_INPUT as JSON: {exc}", file=sys.stderr)
        return 2

    try:
        summary = ingest_task(task)
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    except Exception as exc:
        print(f"ingestion failed: {exc}", file=sys.stderr)
        return 1

    payload = json.dumps(summary)
    print(payload)
    # Also write to the container's termination-log so the controller can read
    # the summary from Pod status without tailing logs.
    try:
        with open("/dev/termination-log", "w", encoding="utf-8") as f:
            f.write(payload)
    except OSError:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
