"""Generate the v2 COSE fixtures using OpenSSL's Ed25519 implementation."""

from hashlib import sha256
import json
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory


def head(major: int, value: int) -> bytes:
    prefix = major << 5
    if value < 24:
        return bytes([prefix + value])
    for code, width in enumerate((1, 2, 4, 8), 24):
        if value < 1 << (width * 8):
            return bytes([prefix + code]) + value.to_bytes(width, "big")
    raise ValueError(value)


def encode(value: object) -> bytes:
    if value is True:
        return b"\xf5"
    if isinstance(value, int):
        return head(0, value) if value >= 0 else head(1, -1 - value)
    if isinstance(value, str):
        data = value.encode("utf-8")
        return head(3, len(data)) + data
    if isinstance(value, bytes):
        return head(2, len(value)) + value
    if isinstance(value, list):
        return head(4, len(value)) + b"".join(map(encode, value))
    if isinstance(value, dict):
        items = sorted((encode(k), encode(v)) for k, v in value.items())
        return head(5, len(items)) + b"".join(k + v for k, v in items)
    raise TypeError(type(value))


def generate(directory: Path) -> None:
    key = directory / "seed.der"
    # RFC 8410 PrivateKeyInfo containing the fixture seed.
    key.write_bytes(bytes.fromhex("302e020100300506032b657004220420") + bytes(range(32)))
    spki = subprocess.check_output(["openssl", "pkey", "-in", key, "-inform", "DER",
                                    "-pubout", "-outform", "DER"])
    kid = sha256(spki[-32:]).digest()[:8]
    protected = encode({1: -8, 4: kid})
    now = 1700000000
    operator = bytes([2]) * 32
    for name, ttl in (("wait", 600), ("dial", 30 * 86400), ("bootstrap", 3600)):
        claims = {1: 2, 5: 2 if name == "wait" else 1, 9: bytes([3]) * 32,
                  10: now, 11: now, 12: now + ttl, 13: bytes(range(16))}
        if name == "wait":
            claims.update({4: operator, 15: now + 300})
        else:
            claims[6] = operator
            if name == "dial":
                claims[4] = bytes([1]) * 32
            else:
                claims[16] = True
        payload = encode(claims)
        message = encode(["Signature1", protected, b"", payload])
        source = directory / "input.bin"
        source.write_bytes(message)
        signature = subprocess.check_output(["openssl", "pkeyutl", "-sign", "-rawin",
                                             "-inkey", key, "-keyform", "DER", "-in", source])
        raw = head(6, 18) + encode([protected, {}, payload, signature])
        vector = dict(kid=kid.hex(), protected=protected.hex(), payload=payload.hex(),
                      input=message.hex(), raw=raw.hex())
        output = Path(__file__).resolve().parents[1] / (name + ".json")
        output.write_bytes((json.dumps(vector, indent=2) + "\n").encode("ascii"))


if __name__ == "__main__":
    with TemporaryDirectory() as temp:
        generate(Path(temp))
