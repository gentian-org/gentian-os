"""
Known-vector tests for derive_password / derive_nats_password.

Vectors are cross-checked against the bash reference in scripts/seed-openbao.sh:

    echo -n "${context}:${purpose}" \\
      | openssl dgst -sha256 -hmac "${MASTER_PASSWORD}" -binary \\
      | sha1sum | awk '{print $1}'

Master password used in all tests: "test-master-do-not-use"
"""
import sys
from pathlib import Path

# Allow importing the module from crossplane/functions/derive-secrets/
sys.path.insert(0, str(Path(__file__).parents[4] / "functions" / "derive-secrets"))

import pytest
from derive import derive_password, derive_nats_password

MASTER = "test-master-do-not-use"

# fmt: off
KNOWN_VECTORS = [
    # (context, purpose, expected_hex)
    ("postgres",  "postgres_user",        "25dc50a1b9486bae94296a3b4a5ef0af32517e5f"),
    ("postgres",  "keycloak_user",        "567f834e881601f7a5c8ff58f0c93579a7887480"),
    ("mariadb",   "root_password",        "7db68ebdf9de05cf086391485bfb0c01c9289a71"),
    ("redis",     "password",             "a1005e60b5eaf44da992f98197efec41c87eb0c4"),
    ("minio",     "root_password",        "ca8db84a2f061c5f3575420180ab4c9314215a77"),
    ("keycloak",  "adminPassword",        "b634c36a33fd80a13efca2e08778dda15ce059d0"),
    ("cn=admin",  "ldap",                 "eb25ee26c646589f7b93d9fd08872645fa1ac4b9"),
    ("nubus",     "Administrator",        "daa3f450bfe3d4f20168eb64c2783c657c3a818b"),
]
# fmt: on


@pytest.mark.parametrize("context,purpose,expected", KNOWN_VECTORS)
def test_derive_password_known_vectors(context: str, purpose: str, expected: str) -> None:
    """Each (context, purpose) pair must produce the expected hex string."""
    assert derive_password(MASTER, context, purpose) == expected


@pytest.mark.parametrize("context,purpose,_expected", KNOWN_VECTORS)
def test_derive_password_output_is_40_hex(context: str, purpose: str, _expected: str) -> None:
    """Output is always a 40-character lowercase hex string (SHA1 length)."""
    result = derive_password(MASTER, context, purpose)
    assert len(result) == 40
    assert all(c in "0123456789abcdef" for c in result)


@pytest.mark.parametrize("context,purpose,base_expected", KNOWN_VECTORS)
def test_derive_nats_password(context: str, purpose: str, base_expected: str) -> None:
    """NATS passwords are the base password prefixed with 'n'."""
    result = derive_nats_password(MASTER, context, purpose)
    assert result == "n" + base_expected
    assert result[0] == "n"


def test_different_masters_produce_different_outputs() -> None:
    """Different master passwords must produce different outputs."""
    a = derive_password("master-A", "postgres", "postgres_user")
    b = derive_password("master-B", "postgres", "postgres_user")
    assert a != b


def test_different_contexts_produce_different_outputs() -> None:
    a = derive_password(MASTER, "postgres", "password")
    b = derive_password(MASTER, "mariadb",  "password")
    assert a != b


def test_different_purposes_produce_different_outputs() -> None:
    a = derive_password(MASTER, "postgres", "user_a")
    b = derive_password(MASTER, "postgres", "user_b")
    assert a != b


def test_empty_master_raises_no_exception_and_produces_hex() -> None:
    """Edge case: empty master is technically valid HMAC key; should not raise."""
    result = derive_password("", "postgres", "postgres_user")
    assert len(result) == 40
