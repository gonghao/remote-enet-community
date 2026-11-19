"""Requires Python 3.10+ and libsodium with ristretto255 support (1.0.18+).
From testdata: python3 gen/application.py > application.json
"""

from collections.abc import Callable
import ctypes
import ctypes.util
import hashlib
import hmac
import itertools
import json


# Public fixture inputs standing in for the TLS exporter and peer fingerprints.
SID = bytes.fromhex("c7a40b88bbb7fb81ee3d05319dcccd82619f603d7d311b6140957a6ad2c1911c")
FP_A = bytes.fromhex("4b87fa7fb91f1d16d1e2604af8fd1c1ce1d680ec71ebc5248a424d8b448c6161")
FP_B = bytes.fromhex("bf453a53b05aa97aaa774f143b76529e63174735fb7c6b93de654cf3e7af507e")
DSI = b"CPaceRistretto255"


def lv_cat(*fields: bytes) -> bytes:
    output = bytearray()
    for field in fields:
        size = len(field)
        while size >= 128:
            output.append(0x80 | (size & 0x7f))
            size >>= 7
        output.append(size)
        output.extend(field)
    return bytes(output)


def hello(role: bytes, capability: bytes) -> bytes:
    # Canonical CBOR short text strings and integer-keyed Hello envelope.
    def text(value: bytes) -> bytes:
        assert len(value) < 24
        return bytes([0x60 + len(value)]) + value

    raw = (bytes.fromhex("a2010102a4010102") + text(role)
           + b"\x03" + text(b"test") + b"\x04\x81" + text(capability))
    return len(raw).to_bytes(4, "big") + raw


def binding(capability: bytes, *extra: bytes) -> tuple[bytes, bytes, bytes]:
    hc, ho = hello(b"client", capability), hello(b"operator", capability)
    ci = lv_cat(b"renet-pake-v1", b"CPACE-RISTR255-SHA512", capability, b"renet/1",
                FP_A, FP_B, hashlib.sha256(hc).digest(), hashlib.sha256(ho).digest(), *extra)
    return hc, ho, ci


def point(function: Callable[..., int], *inputs: bytes) -> bytes:
    output = ctypes.create_string_buffer(32)
    if function(output, *inputs) != 0:
        raise ValueError("libsodium rejected the point operation")
    return output.raw


def mac(key: bytes, data: bytes) -> bytes:
    return hmac.digest(key, data, "sha512")


def hex_fields(**fields: bytes) -> dict[str, str]:
    return {name: value.hex() for name, value in fields.items()}


def generate() -> dict[str, str | dict[str, str]]:
    library = ctypes.util.find_library("sodium")
    if library is None:
        raise RuntimeError("libsodium was not found")
    sodium = ctypes.CDLL(library)
    if sodium.sodium_init() < 0:
        raise RuntimeError("libsodium initialization failed")

    prs = b"00001234"
    hc, ho, ci = binding(b"pair-direct/1")
    prefix = lv_cat(DSI, prs)
    padding = bytes(127 - len(prefix))
    digest = hashlib.sha512(prefix + lv_cat(padding, ci, SID)).digest()
    generator = point(sodium.crypto_core_ristretto255_from_hash, digest)
    multiply = sodium.crypto_scalarmult_ristretto255

    # Keep the original example's initiator scalar. Search from its responder
    # scalar (67890) for the first six-digit SAS with a leading zero (67907).
    ya = (12345).to_bytes(32, "little")
    share_a = point(multiply, ya, generator)
    for candidate in itertools.count(67890):
        yb = candidate.to_bytes(32, "little")
        share_b = point(multiply, yb, generator)
        shared = point(multiply, yb, share_a)
        isk = hashlib.sha512(lv_cat(DSI + b"_ISK", SID, shared, share_a, b"", share_b, b"")).digest()
        prk = mac(b"", isk)
        first_block = mac(prk, b"renet-sas-v1\x01")
        sas = f"{int.from_bytes(first_block[:8], 'big') % 1_000_000:06d}"
        if sas.startswith("0"):
            break

    mac_key = hashlib.sha512(b"CPaceMac" + SID + isk).digest()
    ta = mac(mac_key, lv_cat(share_a, b""))
    tb = mac(mac_key, lv_cat(share_b, b""))
    commit = hashlib.sha512(lv_cat(b"renet-sas-commit-v1", share_a, b"")).digest()
    vector = hex_fields(PRS=prs, HC=hc, HO=ho, sid=SID, fpA=FP_A, fpB=FP_B, CI=ci,
                        g=generator, ya=ya, yb=yb, Ya=share_a, Yb=share_b, K=shared,
                        ISK_IR=isk, Ta=ta, Tb=tb, commit=commit)
    vector["SAS"] = sas
    for number, value in enumerate((commit, share_b, share_a, tb, ta), start=11):
        body = bytes([0xa1, 1, 0x58, len(value)]) + value
        vector[f"CBOR{number}"] = (bytes([0xa2, 1, number, 2]) + body).hex()
    vector["CBOR16"] = (bytes.fromhex("a2011002a201f50268") + b"accepted").hex()

    hub_fp = hashlib.sha256(b"vector hub").digest()
    center_id, session_id = b"vector.example", bytes(range(16))
    hc, ho, ci = binding(b"pair-center/1", hub_fp, center_id, session_id)
    center = hex_fields(HC=hc, HO=ho, hub_fp=hub_fp, center_id=center_id, session_id=session_id, CI=ci)
    return {**vector, "center": center}


if __name__ == "__main__":
    print(json.dumps(generate(), indent=2))
