# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Upload and verify the sample product docs in the local S3 fixture.

Env (set by apply.ps1):
  KOGNITION_S3_ENDPOINT      e.g. http://127.0.0.1:9000
  KOGNITION_S3_ACCESS_KEY    default: minioadmin
  KOGNITION_S3_SECRET_KEY    default: minioadmin
  KOGNITION_S3_REGION        default: us-east-1
  KOGNITION_S3_BUCKET        default: product-docs
  KOGNITION_S3_PREFIX        default: mark7
  KOGNITION_DOCS_DIR         default: <repo>/samples/docs
"""
from __future__ import annotations

import glob
import hashlib
import os
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]


def main() -> int:
    endpoint = os.environ.get("KOGNITION_S3_ENDPOINT")
    if not endpoint:
        print("ERROR: KOGNITION_S3_ENDPOINT not set.", file=sys.stderr)
        return 1

    access = os.environ.get("KOGNITION_S3_ACCESS_KEY", "minioadmin")
    secret = os.environ.get("KOGNITION_S3_SECRET_KEY", "minioadmin")
    region = os.environ.get("KOGNITION_S3_REGION", "us-east-1")
    bucket = os.environ.get("KOGNITION_S3_BUCKET", "product-docs")
    prefix = os.environ.get("KOGNITION_S3_PREFIX", "mark7").strip("/")
    docs_dir = Path(os.environ.get("KOGNITION_DOCS_DIR", str(REPO_ROOT / "samples" / "docs")))

    files = sorted(glob.glob(str(docs_dir / "*.md")) + glob.glob(str(docs_dir / "*.txt")))
    files = [f for f in files if Path(f).name.lower() != "readme.md"]
    if not files:
        print(f"ERROR: no .md/.txt files under {docs_dir}", file=sys.stderr)
        return 1

    try:
        import boto3
        from botocore.config import Config
        from botocore.exceptions import BotoCoreError, ClientError
    except ImportError:
        print(
            "ERROR: boto3 not installed. Install the sample upload dependencies "
            "in the selected environment, then re-run.",
            file=sys.stderr,
        )
        return 1

    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=access,
        aws_secret_access_key=secret,
        region_name=region,
        config=Config(
            s3={"addressing_style": "path"},
            connect_timeout=5,
            read_timeout=30,
            retries={"total_max_attempts": 3, "mode": "standard"},
        ),
    )
    try:
        if region == "us-east-1":
            s3.create_bucket(Bucket=bucket)
        else:
            s3.create_bucket(Bucket=bucket, CreateBucketConfiguration={"LocationConstraint": region})
    except ClientError as exc:
        code = str(exc.response.get("Error", {}).get("Code", "Unknown"))
        if code not in {"BucketAlreadyOwnedByYou", "BucketAlreadyExists"}:
            print(f"ERROR: S3 bucket creation failed ({code}).", file=sys.stderr)
            return 1
        try:
            s3.head_bucket(Bucket=bucket)
        except (BotoCoreError, ClientError) as access_error:
            print(f"ERROR: Existing S3 bucket is not accessible ({type(access_error).__name__}).", file=sys.stderr)
            return 1
    except BotoCoreError as exc:
        print(f"ERROR: S3 bucket creation failed ({type(exc).__name__}).", file=sys.stderr)
        return 1

    expected: dict[str, bytes] = {}
    for f in files:
        name = Path(f).name
        key = f"{prefix}/{name}" if prefix else name
        try:
            body = Path(f).read_bytes()
            s3.put_object(Bucket=bucket, Key=key, Body=body)
            expected[key] = body
            print(f"  uploaded {name} -> s3://{bucket}/{key}")
        except (OSError, BotoCoreError, ClientError) as exc:
            print(f"ERROR: Upload failed for {name} ({type(exc).__name__}).", file=sys.stderr)
            return 1

    try:
        listing_prefix = f"{prefix}/" if prefix else ""
        pages = s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=listing_prefix)
        actual = {item["Key"] for page in pages for item in page.get("Contents", [])}
        if actual != set(expected):
            print("ERROR: S3 verification found an unexpected object inventory.", file=sys.stderr)
            return 1
        for key, body in expected.items():
            with s3.get_object(Bucket=bucket, Key=key)["Body"] as stream:
                downloaded = stream.read(len(body) + 1)
            if hashlib.sha256(downloaded).digest() != hashlib.sha256(body).digest():
                print(f"ERROR: S3 content verification failed for {key}.", file=sys.stderr)
                return 1
    except (BotoCoreError, ClientError) as exc:
        print(f"ERROR: S3 verification failed ({type(exc).__name__}).", file=sys.stderr)
        return 1

    print(f"Verified {len(expected)} uploaded file(s) in s3://{bucket}/{prefix}/.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
