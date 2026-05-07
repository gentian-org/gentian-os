"""
Idempotency tests for derive_password / derive_nats_password.

Each derivation must be deterministic: calling the function N times with the
same inputs must always return the same value.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[4] / "functions" / "derive-secrets"))

import pytest
from derive import derive_password, derive_nats_password, KERNEL_CREDENTIALS

MASTER = "test-master-do-not-use"
RUNS = 10


def test_derive_password_is_idempotent() -> None:
    """Same inputs always produce the same output."""
    first = derive_password(MASTER, "postgres", "postgres_user")
    for _ in range(RUNS - 1):
        assert derive_password(MASTER, "postgres", "postgres_user") == first


def test_derive_nats_password_is_idempotent() -> None:
    first = derive_nats_password(MASTER, "redis", "password")
    for _ in range(RUNS - 1):
        assert derive_nats_password(MASTER, "redis", "password") == first


@pytest.mark.parametrize("cred", KERNEL_CREDENTIALS)
def test_all_kernel_credentials_are_idempotent(cred: dict) -> None:
    """Every credential in KERNEL_CREDENTIALS is stable across multiple calls."""
    first = derive_password(MASTER, cred["context"], cred["purpose"])
    for _ in range(3):
        assert derive_password(MASTER, cred["context"], cred["purpose"]) == first
