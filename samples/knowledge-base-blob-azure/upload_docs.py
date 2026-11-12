# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Upload the sample product docs to the Azure Blob container the
knowledge-base-blob-azure sample indexes.

Two modes:

* **Azurite (default local path)** — set `KOGNITION_BLOB_CONNECTION_STRING` to
  the emulator connection string. Uses shared-key auth and creates the
  container if needed. This is what apply.ps1 does (the emulator runs
  in-cluster; apply.ps1 port-forwards it for the upload).

* **Real Azure (passwordless)** — no connection string; authenticates as the
  service principal in the repo-root .env (the same SP the KB ingester uses).
  Reads .env directly so no secret ever touches the command line. Requires the
  SP to have Storage Blob Data Contributor on the account.

Env overrides (all optional):
  KOGNITION_BLOB_CONNECTION_STRING  if set, use shared-key auth (Azurite path)
  KOGNITION_BLOB_ACCOUNT    default: kognition   (passwordless path only)
  KOGNITION_BLOB_CONTAINER  default: product-docs
  KOGNITION_BLOB_PREFIX     default: mark7
  KOGNITION_DOCS_DIR        default: <repo>/samples/docs
"""
from __future__ import annotations

import glob
import os
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]


def load_env() -> dict[str, str]:
    env: dict[str, str] = {}
    envfile = REPO_ROOT / ".env"
    if not envfile.exists():
        return env
    for line in envfile.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def _resolve_files(docs_dir: Path) -> list[str]:
    files = sorted(glob.glob(str(docs_dir / "*.md")) + glob.glob(str(docs_dir / "*.txt")))
    # The folder README documents the samples themselves, not the product — skip
    # it so it doesn't pollute retrieval.
    return [f for f in files if Path(f).name.lower() != "readme.md"]


def main() -> int:
    container = os.environ.get("KOGNITION_BLOB_CONTAINER", "product-docs")
    prefix = os.environ.get("KOGNITION_BLOB_PREFIX", "mark7").strip("/")
    docs_dir = Path(os.environ.get("KOGNITION_DOCS_DIR", str(REPO_ROOT / "samples" / "docs")))
    conn = os.environ.get("KOGNITION_BLOB_CONNECTION_STRING")

    files = _resolve_files(docs_dir)
    if not files:
        print(f"SKIP: no .md/.txt files under {docs_dir}", file=sys.stderr)
        return 0

    try:
        from azure.storage.blob import ContainerClient
    except ImportError:
        print(
            "SKIP: azure-storage-blob not installed in this Python environment. "
            "Activate the repo venv or `pip install azure-identity "
            "azure-storage-blob`, then re-run.",
            file=sys.stderr,
        )
        return 0  # non-fatal

    if conn:
        # Azurite / shared-key path.
        cc = ContainerClient.from_connection_string(conn, container_name=container)
        try:
            cc.create_container()
        except Exception:  # noqa: BLE001 - already exists is fine
            pass
        target = f"container '{container}' (connection string)"
    else:
        # Real Azure, passwordless (service principal).
        env = load_env()
        tenant, client, secret = env.get("AZURE_TENANT_ID"), env.get("AZURE_CLIENT_ID"), env.get("AZURE_CLIENT_SECRET")
        if not (tenant and client and secret):
            print("SKIP: no KOGNITION_BLOB_CONNECTION_STRING and .env missing AZURE_* creds.", file=sys.stderr)
            return 0
        account = os.environ.get("KOGNITION_BLOB_ACCOUNT", "kognition")
        from azure.identity import ClientSecretCredential

        cred = ClientSecretCredential(tenant_id=tenant, client_id=client, client_secret=secret)
        cc = ContainerClient(
            account_url=f"https://{account}.blob.core.windows.net",
            container_name=container,
            credential=cred,
        )
        target = f"'{account}' account"

    uploaded = 0
    for f in files:
        name = Path(f).name
        blob_name = f"{prefix}/{name}" if prefix else name
        try:
            with open(f, "rb") as fh:
                cc.get_blob_client(blob_name).upload_blob(fh, overwrite=True)
            print(f"  uploaded {name} -> {container}/{blob_name}")
            uploaded += 1
        except Exception as exc:  # noqa: BLE001 - surface a helpful hint
            first = str(exc).splitlines()[0] if str(exc) else exc.__class__.__name__
            print(f"  FAILED {name}: {first}", file=sys.stderr)
            if not conn:
                print(
                    "  -> ensure the SP has 'Storage Blob Data Contributor' on the "
                    f"{target} and that the account firewall allows this host.",
                    file=sys.stderr,
                )
            return 1

    print(f"Uploaded {uploaded} file(s) to {container}/{prefix}/ ({target}).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

