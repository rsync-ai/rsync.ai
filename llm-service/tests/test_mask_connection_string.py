"""mask_connection_string hides a URL's password and leaves the rest readable.

The user name may be empty (``redis://:password@host``, how compose builds the
Redis URL), and a raw ``@`` in a password must hide both halves. A URL with no
password, or an ``@`` after the host, is left as it is. All values are synthetic.
"""

import pytest

from src.utils.masking import MASKED_VALUE as M
from src.utils.masking import mask_connection_string


@pytest.mark.parametrize(
    "raw, masked",
    [
        ("postgresql://app:pw1@db:5432/x", f"postgresql://app:{M}@db:5432/x"),
        ("redis://:pw2@redis:6379/0", f"redis://:{M}@redis:6379/0"),
        ("redis://:pw-a@pw-b@redis:6379/0", f"redis://:{M}@redis:6379/0"),
        (
            "mongodb://u:pw3@h1:27017,h2:27017/db?replicaSet=rs0",
            f"mongodb://u:{M}@h1:27017,h2:27017/db?replicaSet=rs0",
        ),
        ("redis://redis:6379/0?password=pw4", f"redis://redis:6379/0?password={M}"),
        (
            "from redis://:pw5@h1:6379/0 to redis://:pw6@h2:6379/0",
            f"from redis://:{M}@h1:6379/0 to redis://:{M}@h2:6379/0",
        ),
        ("redis://redis:6379/0", "redis://redis:6379/0"),
        ("http://user@host:8080/", "http://user@host:8080/"),
        (
            "https://api.example.com:8443/v1/users?email=a@b.example",
            "https://api.example.com:8443/v1/users?email=a@b.example",
        ),
        ("", ""),
    ],
)
def test_mask_connection_string(raw, masked):
    assert mask_connection_string(raw) == masked
