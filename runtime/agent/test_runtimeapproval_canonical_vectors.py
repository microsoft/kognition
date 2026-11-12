# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import base64
from decimal import Decimal
import hashlib
import hmac
import json
import os
from pathlib import Path
from typing import Any

import pytest


FIXTURE_PATH = Path(os.environ["KOGNITION_CANONICAL_VECTOR_FILE"]) if os.environ.get("KOGNITION_CANONICAL_VECTOR_FILE") else (
    Path(__file__).resolve().parents[2]
    / "tests"
    / "runtimeapproval"
    / "canonical_vectors.json"
)


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def _canonical_number(value: Decimal) -> str:
    if value.is_zero():
        return "0"
    sign, digits, exponent = value.as_tuple()
    coefficient = "".join(str(digit) for digit in digits).lstrip("0")
    while coefficient.endswith("0"):
        coefficient = coefficient[:-1]
        exponent += 1
    if sign:
        coefficient = "-" + coefficient
    return coefficient if exponent == 0 else f"{coefficient}e{exponent}"


def _encode_canonical(value: Any) -> str:
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, Decimal):
        return _canonical_number(value)
    if isinstance(value, str):
        return (
            json.dumps(value, ensure_ascii=False, separators=(",", ":"))
            .replace("\u2028", "\\u2028")
            .replace("\u2029", "\\u2029")
        )
    if isinstance(value, list):
        return "[" + ",".join(_encode_canonical(item) for item in value) + "]"
    if isinstance(value, dict):
        members = (
            _encode_canonical(key) + ":" + _encode_canonical(value[key])
            for key in sorted(value)
        )
        return "{" + ",".join(members) + "}"
    raise TypeError(f"unsupported JSON value {type(value)!r}")


def _canonicalize(raw: str) -> bytes:
    value = json.loads(
        raw,
        parse_int=Decimal,
        parse_float=Decimal,
        object_pairs_hook=_reject_duplicate_keys,
    )
    canonical = _encode_canonical(value)
    canonical.encode("utf-8", errors="strict")
    return canonical.encode("utf-8")


def _canonical_action(action: dict[str, Any]) -> bytes:
    envelope = dict(action)
    envelope["format"] = "kognition-runtime-action-v1"
    envelope["authorizationRefs"] = sorted(
        envelope.get("authorizationRefs", []),
        key=lambda ref: (
            ref["kind"], ref["scope"], ref.get("namespace", ""), ref["name"],
            ref["uid"], ref["resourceVersion"],
        ),
    )
    return _encode_canonical(envelope).encode("utf-8")


with FIXTURE_PATH.open(encoding="utf-8") as fixture_file:
    VECTORS = json.load(fixture_file, parse_int=Decimal, parse_float=Decimal)


@pytest.mark.parametrize("vector", VECTORS["accepted"], ids=lambda vector: vector["name"])
def test_runtimeapproval_canonical_vector(vector: dict[str, str]) -> None:
    canonical = _canonicalize(vector["input"])
    assert canonical.decode("utf-8") == vector["canonical"]
    message = VECTORS["domain"].encode() + b"\0" + canonical
    digest = hmac.new(VECTORS["key"].encode(), message, hashlib.sha256).digest()
    assert base64.urlsafe_b64encode(digest).rstrip(b"=").decode() == vector["tag"]


@pytest.mark.parametrize("vector", VECTORS["rejected"], ids=lambda vector: vector["name"])
def test_runtimeapproval_rejected_vector(vector: dict[str, str]) -> None:
    with pytest.raises((UnicodeEncodeError, ValueError)):
        _canonicalize(vector["input"])


@pytest.mark.parametrize("vector", VECTORS["actions"], ids=lambda vector: vector["name"])
def test_runtimeapproval_action_vector(vector: dict[str, Any]) -> None:
    canonical = _canonical_action(vector["action"])
    assert canonical.decode("utf-8") == vector["canonical"]
    message = VECTORS["domain"].encode() + b"\0" + canonical
    digest = hmac.new(VECTORS["key"].encode(), message, hashlib.sha256).digest()
    assert base64.urlsafe_b64encode(digest).rstrip(b"=").decode() == vector["tag"]