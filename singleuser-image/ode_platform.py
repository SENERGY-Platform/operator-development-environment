"""Reaching the platform from inside an ODE kernel.

ODE installs the developer's own access token and the platform URLs in the
kernel's environment when a session opens, and again whenever the token is
refreshed (SPEC §5.6 item 4). This is a thin reader for those variables, so that
a cell does not begin by rebuilding an HTTP client.

The authorisation here is the developer's own and nothing more: code written by
a developer and code written by an LLM reach exactly the same data, which is the
non-escalation property §5.6 rests on.

Deliberately small. It is a convenience for the console, not a client library —
the profiler, the ontology and semantic selection live in ODE and are reached
through it, not reimplemented here.
"""

import os
from typing import Any

import requests

__all__ = ["token", "workspace", "device_repository", "timescale", "query"]


def token() -> str:
    """The developer's current platform access token."""
    value = os.environ.get("SENERGY_TOKEN", "")
    if not value:
        raise RuntimeError(
            "SENERGY_TOKEN is not set: this kernel was not started by ODE, "
            "or the session has not pushed a token yet"
        )
    return value


def workspace() -> str:
    """The persistent working directory. Anything written elsewhere is lost when
    the pod is culled."""
    return os.environ.get("ODE_WORKSPACE", ".")


def _base(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        raise RuntimeError(f"{name} is not set in this kernel")
    return value.rstrip("/")


def device_repository(path: str, **params: Any) -> Any:
    """GET from the device repository, as the developer."""
    response = requests.get(
        f"{_base('SENERGY_DEVICE_REPO_URL')}/{path.lstrip('/')}",
        headers={"Authorization": f"Bearer {token()}"},
        params=params or None,
        timeout=60,
    )
    response.raise_for_status()
    return response.json()


def timescale(path: str, body: Any = None, **params: Any) -> Any:
    """Call timescale-wrapper, as the developer. POST when a body is given."""
    url = f"{_base('SENERGY_TIMESCALE_URL')}/{path.lstrip('/')}"
    headers = {"Authorization": f"Bearer {token()}"}
    if body is None:
        response = requests.get(url, headers=headers, params=params or None, timeout=300)
    else:
        response = requests.post(url, headers=headers, json=body, timeout=300)
    response.raise_for_status()
    return response.json()


def query(elements: list[dict]) -> Any:
    """Issue one batched POST /queries/v2.

    Batching is the point: alignment and resampling belong on the server
    (§5.3.1), so one request with a shared groupTime beats several and a
    client-side merge.
    """
    return timescale("queries/v2", body=elements)
