"""Encoding, test identities and the conformance document format."""

from dataclasses import dataclass
import hashlib
import json
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory

NOW = 1795000000
DIRECTORY = Path(__file__).resolve().parents[1]
CONTRACT = "CENTER-CONTRACT v1 第八版（2026-11-20）"
PKCS8 = bytes.fromhex("302e020100300506032b657004220420")
SPKI = bytes.fromhex("302a300506032b6570032100")


@dataclass(frozen=True)
class Tag:
    number: int
    value: object


def head(major: int, value: int) -> bytes:
    if not 0 <= value < 1 << 64:
        raise ValueError(f"invalid CBOR argument: {value}")
    prefix = major << 5
    if value < 24:
        return bytes([prefix + value])
    for code, width in enumerate((1, 2, 4, 8), 24):
        if value < 1 << (width * 8):
            return bytes([prefix + code]) + value.to_bytes(width, "big")
    raise AssertionError("unreachable width")


def encode(value: object) -> bytes:
    if value is None:
        return b"\xf6"
    if isinstance(value, bool):
        return b"\xf5" if value else b"\xf4"
    if isinstance(value, int):
        return head(0, value) if value >= 0 else head(1, -1 - value)
    if isinstance(value, bytes):
        return head(2, len(value)) + value
    if isinstance(value, str):
        data = value.encode("utf-8")
        return head(3, len(data)) + data
    if isinstance(value, (list, tuple)):
        return head(4, len(value)) + b"".join(map(encode, value))
    if isinstance(value, dict):
        items = sorted((encode(k), encode(v)) for k, v in value.items())
        return head(5, len(items)) + b"".join(k + v for k, v in items)
    if isinstance(value, Tag):
        return head(6, value.number) + encode(value.value)
    raise TypeError(type(value))


def hex_fields(**fields: bytes) -> dict[str, str]:
    return {name: value.hex() for name, value in fields.items()}


def seed(name: str) -> bytes:
    return hashlib.sha256(("renet-conformance-v1/" + name).encode()).digest()


def public_key(name: str) -> bytes:
    der = subprocess.check_output(["openssl", "pkey", "-inform", "DER", "-pubout",
                                   "-outform", "DER"], input=PKCS8 + seed(name))
    if not der.startswith(SPKI) or len(der) != len(SPKI) + 32:
        raise ValueError("unexpected Ed25519 public key")
    return der[len(SPKI):]


def sign(name: str, message: bytes) -> bytes:
    with TemporaryDirectory() as tmp:
        key, source = Path(tmp) / "key.der", Path(tmp) / "input"
        key.write_bytes(PKCS8 + seed(name))
        source.write_bytes(message)
        # pkeyutl rejects a zero-length oneshot input buffer.
        if not message:
            return subprocess.check_output(["openssl", "dgst", "-keyform", "DER",
                                            "-sign", key, source])
        return subprocess.check_output(["openssl", "pkeyutl", "-sign", "-rawin",
                                        "-keyform", "DER", "-inkey", key, "-in", source])


def verify(key: bytes, message: bytes, signature: bytes) -> bool:
    if len(key) != 32 or len(signature) != 64:
        return False
    with TemporaryDirectory() as tmp:
        directory = Path(tmp)
        pub, source, sig = (directory / f for f in ("public.der", "input", "signature"))
        pub.write_bytes(SPKI + key)
        source.write_bytes(message)
        sig.write_bytes(signature)
        if message:
            command = ["openssl", "pkeyutl", "-verify", "-rawin", "-pubin", "-keyform",
                       "DER", "-inkey", pub, "-in", source, "-sigfile", sig]
        else:
            command = ["openssl", "dgst", "-keyform", "DER", "-verify", pub,
                       "-signature", sig, source]
        result = subprocess.run(command, capture_output=True)
        if result.returncode and b"verification failure" not in result.stdout.lower():
            result.check_returncode()
        return result.returncode == 0


def write_vector(filename: str, vector: str, title: str, generator: str,
                 cases: list[dict], check: bool) -> list[Path]:
    document = dict(format=1, contract=CONTRACT, contract_version="v1.9", vector=vector, title=title,
                    generator=generator, cases=cases)
    data = (json.dumps(document, ensure_ascii=False, indent=2) + "\n").encode()
    path = DIRECTORY / filename
    if check:
        return [] if path.is_file() and path.read_bytes() == data else [path]
    path.write_bytes(data)
    return []
