"""Center admission tickets signed over the original COSE byte strings."""

from hashlib import sha256
from pathlib import Path

from common import NOW, Tag, encode, hex_fields, public_key, sign, write_vector

ISSUER = "center-signing-1"
NAMES = ("v", "center", "acct", "dev", "role", "peer", "sid", "node",
         "hub", "iat", "nbf", "exp", "jti", "gen")
MAX_UINT32 = 2**32 - 1


def issue(payload: bytes, protected: bytes) -> dict:
    message = encode(["Signature1", protected, b"", payload])
    signature = sign(ISSUER, message)
    token = encode(Tag(18, [protected, {}, payload, signature]))
    return dict(result="accept", length=len(token), **hex_fields(
        payload=payload, protected=protected, sig_structure=message,
        signature=signature, token=token))


def generate(check: bool) -> list[Path]:
    pub = public_key(ISSUER)
    kid = sha256(pub).digest()[:8]
    protected = encode({1: -8, 4: kid})

    def fp(name: str) -> bytes:
        return sha256(public_key(name)).digest()

    def test_id(name: str) -> bytes:
        return sha256(("renet-conformance-v1/" + name).encode()).digest()[:16]

    dial = {1: 1, 2: "test.center", 3: test_id("account"), 4: fp("device-client"),
            5: 1, 6: fp("device-operator"), 7: test_id("session"), 8: test_id("node"),
            9: fp("hub-node-1"), 10: NOW, 11: NOW, 12: NOW + 120, 13: test_id("dial-jti"),
            14: 42}
    wait = dial | {4: dial[6], 5: 2, 6: dial[4], 13: test_id("wait-jti")}
    config = dict(center_id=dial[2], node_id=dial[8].hex(), hub=dial[9].hex(),
                  gen=dial[14], keys=[hex_fields(kid=kid, public_key=pub)],
                  clock_skew=30, ticket_max_ttl=300)
    valid = []
    for name, fields, ttl, deadline in (
            ("v4-v5-dial", dial, 120, NOW+600),
            ("v4-hard-deadline", dial, 120, NOW+100),
            ("v4-max-ttl", dial, 300, NOW+600),
            ("v4-max-size-wait", wait | {2: "a" * 32, 14: MAX_UINT32}, 120, NOW+600)):
        fields = fields | {12: min(NOW + ttl, deadline - 2 * config["clock_skew"])}
        claims = {NAMES[k-1]: v.hex() if isinstance(v, bytes) else v for k, v in fields.items()}
        expected = issue(encode(fields), protected) | hex_fields(kid=kid)
        assert expected["length"] <= 512
        valid.append(dict(name=name, input=dict(key=ISSUER, public_key=pub.hex(), claims=claims,
                          ticket_ttl=ttl, hard_deadline=deadline, clock_skew=config["clock_skew"],
                          ticket_max_ttl=config["ticket_max_ttl"], now=NOW), expect=expected))
    assert valid[-1]["expect"]["length"] == 326

    cases = []
    raw = bytes.fromhex(valid[0]["expect"]["token"])
    payload = encode(dial)

    def signed(fields: dict) -> bytes:
        return bytes.fromhex(issue(encode(fields), protected)["token"])

    def add(name: str, step: int | None, token: bytes = raw, *, cfg: dict | None = None,
            role: int = 1, target: bytes = dial[6], device: bytes = dial[4],
            now: int = NOW) -> None:
        inputs = dict(token=token.hex(), config=config | (cfg or {}),
                      route=dict(role=role, target=target.hex()), tls_device=device.hex(), now=now)
        expected = dict(result="accept") if step is None else dict(result="reject", reason=f"8.4/{step}")
        cases.append(dict(name=name, input=inputs, expect=expected))

    add("v8-signature", 4, raw[:-1] + bytes([raw[-1] ^ 1]))
    # A malformed map after a one-byte edit must still fail at authentication.
    add("v8-payload-byte", 4, raw.replace(payload, b"\xff" + payload[1:], 1))
    add("v8-modified-role", 4, raw.replace(payload, encode(dial | {5: 2}), 1))
    duplicate = b"\xaf" + payload[1:] + encode(14) + encode(dial[14])
    add("v10-duplicate-payload-key", 5, bytes.fromhex(issue(duplicate, protected)["token"]))
    other = public_key("device-operator")
    add("v8-unknown-kid", 3, cfg=dict(keys=[hex_fields(kid=sha256(other).digest()[:8], public_key=other)]))
    for name, header in (("extra-protected", encode({1: -8, 4: kid, 7: 0})),
                         ("duplicate-key", b"\xa3" + protected[1:] + encode(4) + encode(kid))):
        add("v9-" + name, 2, bytes.fromhex(issue(payload, header)["token"]))
    signature = bytes.fromhex(valid[0]["expect"]["signature"])
    add("v9-unprotected", 2, encode(Tag(18, [protected, {7: 1}, payload, signature])))
    add("v9-missing-tag", 2, raw[1:])
    add("v9-indefinite", 2, b"\xd2\x9f" + raw[2:] + b"\xff")
    add("v9-nonminimal-tag", 2, b"\xd8\x12" + raw[1:])

    selfhost = {k: dial[k] for k in (4, 5, 6, 9, 10, 11, 12, 13)}
    add("v10-selfhost-v2", 5, signed(selfhost | {1: 2}))
    add("v10-constraint", 5, signed(dial | {15: NOW}))
    add("v10-information", None, signed(dial | {64: {"source": "test"}}))
    add("v10-missing-account", 5, signed({k: v for k, v in dial.items() if k != 3}))
    for name, k, v in (("center-alphabet", 2, "TEST"), ("short-node", 8, bytes(15)),
                       ("role-type", 5, True), ("generation-overflow", 14, 2**32)):
        add("v10-" + name, 5, signed(dial | {k: v}))
    add("v10-nonminimal-version", 5, bytes.fromhex(issue(
        payload[:2] + b"\x18\x01" + payload[3:], protected)["token"]))

    for key, value in (("center_id", "other"), ("node_id", bytes(16).hex()),
                       ("hub", bytes(32).hex()), ("gen", dial[14]+1)):
        add("v11-" + key.replace("_id", ""), 6, cfg={key: value})
    add("v12-device", 7, device=dial[6])
    add("v13-role", 8, role=2)
    add("v13-peer", 8, target=dial[4])
    # Both fingerprints are zero: only the explicit nonzero route check can reject it.
    add("v13-wait-zero-target", 8, signed(wait | {6: bytes(32)}),
        role=2, target=bytes(32), device=wait[4])

    for offset, name, step in ((-1, "before", None), (0, "at", None), (1, "after", 9)):
        add("v14-expiry-" + name, step, now=dial[12] + config["clock_skew"] + offset)
    for name, fields in (("nbf-future", {11: NOW+31}), ("iat-future", {10: NOW+31, 11: NOW+31}),
                         ("ttl", {12: NOW+301}), ("nbf-at-exp", {11: dial[12]}),
                         ("iat-after-nbf", {11: NOW-1})):
        add("v14-" + name, 9, signed(dial | fields))
    add("v14-nbf-within-skew", None, signed(dial | {11: NOW + 30}))
    near_max = dial | {10: MAX_UINT32-120, 11: MAX_UINT32-120, 12: MAX_UINT32}
    add("v14-uint32-upper", None, signed(near_max), now=MAX_UINT32+30)
    add("v15-empty", 1, b"")
    for length, step in ((1024, None), (1025, 2)):
        # The extension length and outer payload length each use a uint16 argument here.
        base = signed(dial | {64: bytes(512)})
        token = signed(dial | {64: bytes(512 + length - len(base))})
        assert len(token) == length
        add(f"v15-size-{length}", step, token)
    return (write_vector("v04-ticket.json", "V4/V5", "合法中心票据与 kid 推导",
                         "gen/ticket.py", valid, check)
            + write_vector("v08-ticket-reject.json", "V8–V15", "中心票据准入边界",
                           "gen/ticket.py", cases, check))
