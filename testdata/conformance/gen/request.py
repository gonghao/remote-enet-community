"""V3 request signatures and HTTP request-target examples."""

from base64 import urlsafe_b64encode
from hashlib import sha256
from pathlib import Path
import json
import re
from urllib.parse import urlsplit

import common


def b64(data: bytes) -> str:
    return urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def signed(kind: str, target: str, timestamp: int = common.NOW) -> tuple[dict, dict]:
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
                             target.encode(), timestamp, client, nonce, idem, sha256(body).digest()])
    sig = common.sign(key, message)
    headers = {
        "Renet-Device-Key" if device else "Renet-Node-Id": [b64(principal)],
        "Renet-Timestamp": [str(timestamp)], "Renet-Client": [client], "Renet-Signature": [b64(sig)],
    }
    if nonce:
        headers["Renet-Nonce"] = [b64(nonce)]
    if idem:
        headers["Idempotency-Key"] = [idem]
    inputs = dict(key=key, center_id=center, kind=kind, principal=principal.hex(), method=method,
                  target=target, timestamp=timestamp, renet_client=client, nonce=nonce.hex(),
                  idempotency_key=idem, body=body.hex())
    if not device:
        inputs["public_key"] = public.hex()
    return inputs, dict(result="accept", signing_input=message.hex(), signature=sig.hex(), headers=headers)


def canonical_target(raw: str) -> str:
    target = raw.partition("#")[0]
    if not target.startswith("/"):
        url = urlsplit(target)
        if url.scheme not in {"https", "http"} or not url.hostname or url.username is not None:
            raise ValueError("unsupported URL")
        target = (url.path or "/") + ("?" + url.query if url.query else "")
    if not re.fullmatch(r"(?:[a-zA-Z0-9._~!$&()*+,/:;=?@-]|%[a-fA-F0-9]{2})+", target):
        raise ValueError("target requires encoding")
    path, _, query = target.partition("?")
    output = []
    segments = path[1:].split("/")
    for i, segment in enumerate(segments):
        dot = re.sub("%2e", ".", segment, flags=re.I)
        if dot not in {".", ".."}:
            output.append(segment)
            continue
        if dot == ".." and output:
            output.pop()
        if i + 1 == len(segments):
            output.append("")
    return "/" + "/".join(output) + ("?" + query if query else "")


def generate(check: bool) -> list[Path]:
    cases = []
    get = "/v1/sessions/AAECAwQFBgcICQoLDA0ODw?since_version=1&wait_s=3"
    for kind, target in [("device", get), ("node", "/v1/node/activate")]:
        inputs, expected = signed(kind, target)
        case = dict(name=f"{kind}-request", input=inputs, expect=expected)
        if kind == "node":
            case["note"] = "public_key 是节点激活时登记的 Ed25519 公钥（§7.8）；principal 是节点 ID。"
        cases.append(case)

    targets = {
        "escaped": "/v1/a%2fb/%41?q=%2F&q=%2f",
        "bare-query": "/v1/nodes?",
        "fragment": "https://center.example/v1/nodes?a=1#local",
        "dot-segments": "/v1/./node/../nodes",
        "incomplete-escape": "/v1/nodes?q=100%",
        "query-space": "/v1/nodes?q=a b",
        "non-ascii-path": "/v1/节点",
        "escaped-segment-data": "/v1/%2F/%2efile",
        "empty-segments": "/v1//nodes//",
        "escaped-parent": "/v1/node/%2e%2e/nodes",
    }
    for name, raw in targets.items():
        try:
            canonical = canonical_target(raw)
        except ValueError:
            expected = dict(result="reject", reason="bad_request")
        else:
            expected = dict(result="accept", canonical_target=canonical)
        cases.append(dict(name=f"target-{name}", input=dict(raw_url=raw), expect=expected))

    for name, target, reason in [
        ("malformed-target", targets["incomplete-escape"], "bad_request"),
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
        if name.startswith("signed-"):
            cases[-1]["note"] = "签名针对原始目标，中心以规范化后的目标验签。"
    window = 60
    for offset in (-window - 1, -window, window, window + 1):
        inputs, expected = signed("device", get, common.NOW + offset)
        inputs.update(server_time=common.NOW, request_window=window)
        if abs(offset) > window:
            inputs["headers"] = expected["headers"]
            expected = dict(result="reject", reason="clock_skew")
        side = "past" if offset < 0 else "future"
        cases.append(dict(name=f"time-{side}-{abs(offset)}", input=inputs, expect=expected))

    for name, prefix in [("plus", "+"), ("leading-zero", "0")]:
        inputs, expected = signed("device", get)
        inputs["headers"] = expected["headers"]
        inputs["headers"]["Renet-Timestamp"] = [prefix + str(common.NOW)]
        cases.append(dict(name=f"timestamp-{name}", input=inputs,
                          expect=dict(result="reject", reason="bad_request")))
    return common.write_vector("v03-request-signature.json", "V3", "中心 API 请求签名",
                               "gen/request.py", cases, check)
