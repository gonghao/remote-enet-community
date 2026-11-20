"""V3 request signatures and HTTP request-target examples."""

from base64 import urlsafe_b64encode
from hashlib import sha256
from pathlib import Path
import json

import common


def b64(data: bytes) -> str:
    return urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def signed(kind: str, target: str) -> tuple[dict, dict]:
    device = kind == "device"
    key = "device-operator" if device else "hub-node-1"
    public = common.public_key(key)
    principal = public if device else bytes(range(16))
    method = "GET" if device else "POST"
    client = "operator/1.2.0" if device else "hub/1.2.0"
    body = b"" if device else json.dumps(dict(node_pub=b64(public), version="1.2.0",
                                              kids=[]), separators=(",", ":")).encode()
    nonce = b"" if device else bytes(range(16, 32))
    idem = "" if device else "activate-00000001"
    center = "center.example"
    message = common.encode(["renet-center/v1 request", center, kind, principal, method,
                             target.encode(), common.NOW, client, nonce, idem, sha256(body).digest()])
    sig = common.sign(key, message)
    headers = {
        "Renet-Device-Key" if device else "Renet-Node-Id": [b64(principal)],
        "Renet-Timestamp": [str(common.NOW)], "Renet-Client": [client], "Renet-Signature": [b64(sig)],
    }
    if nonce:
        headers["Renet-Nonce"] = [b64(nonce)]
    if idem:
        headers["Idempotency-Key"] = [idem]
    inputs = dict(key=key, center_id=center, kind=kind, principal=principal.hex(), method=method,
                  target=target, timestamp=common.NOW, renet_client=client, nonce=nonce.hex(),
                  idempotency_key=idem, body=body.hex())
    return inputs, dict(result="accept", signing_input=message.hex(), signature=sig.hex(), headers=headers)


def generate(check: bool) -> list[Path]:
    cases = []
    get = "/v1/sessions/AAECAwQFBgcICQoLDA0ODw?since_version=1&wait_s=3"
    for kind, target in [("device", get), ("node", "/v1/node/activate")]:
        inputs, expected = signed(kind, target)
        cases.append(dict(name=f"{kind}-request", input=inputs, expect=expected))

    targets = [
        ("escaped", "/v1/a%2fb/%41?q=%2F&q=%2f", "/v1/a%2fb/%41?q=%2F&q=%2f"),
        ("bare-query", "/v1/nodes?", "/v1/nodes"),
        ("fragment", "https://center.example/v1/nodes?a=1#local", "/v1/nodes?a=1"),
        ("dot-segments", "/v1/./node/../nodes", "/v1/nodes"),
    ]
    for name, raw, canonical in targets:
        cases.append(dict(name=f"target-{name}", input=dict(raw_url=raw),
                          expect=dict(result="accept", canonical_target=canonical)))

    for name, target, reason in [
        ("changed-client", get, "unauthenticated"),
        ("signed-dot-segments", "/v1/./node/../nodes", "unauthenticated"),
        ("signed-bare-query", "/v1/nodes?", "unauthenticated"),
        ("repeated-client", get, "bad_request"),
        ("comma-client", get, "bad_request"),
    ]:
        inputs, expected = signed("device", target)
        headers = expected["headers"]
        if name == "changed-client":
            headers["Renet-Client"] = ["operator/1.2.1"]
        elif name == "repeated-client":
            headers["Renet-Client"] *= 2
        elif name == "comma-client":
            headers["Renet-Client"] = [", ".join(headers["Renet-Client"] * 2)]
        inputs["headers"] = headers
        cases.append(dict(name=name, input=inputs, expect=dict(result="reject", reason=reason)))
    return common.write_vector("v03-request-signature.json", "V3", "中心 API 请求签名",
                               "gen/request.py", cases, check)
