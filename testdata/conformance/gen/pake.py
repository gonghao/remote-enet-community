"""Publish the independent CPace application generator's transcripts."""

from hashlib import sha256
from pathlib import Path
import runpy

from common import DIRECTORY, write_vector


def generate(check: bool) -> list[Path]:
    source = DIRECTORY.parents[1] / "internal/pairing/cpace/testdata/gen/application.py"
    application = runpy.run_path(str(source))["generate"]
    direct, hub = application(False), application(True)
    ci_cases, messages = [], []
    for mode, v in (("direct", direct), ("hub", hub), ("center", direct["center"])):
        inputs = dict(capability=f"pair-{mode}/1", fp_client=direct["fpA"],
                      fp_operator=direct["fpB"], hc=v["HC"], ho=v["HO"])
        if mode != "direct":
            inputs["hub_fp"] = v["hub_fp"]
        if mode == "center":
            inputs.update(center_id=bytes.fromhex(v["center_id"]).decode("ascii"),
                          session_id=v["session_id"])
        ci_cases.append(dict(name=mode, input=inputs, expect=dict(
            result="accept", hc_sha256=sha256(bytes.fromhex(v["HC"])).hexdigest(),
            ho_sha256=sha256(bytes.fromhex(v["HO"])).hexdigest(), ci=v["CI"])))
        if mode != "center":
            expected = dict(result="accept", commit=v["commit"], isk=v["ISK_IR"],
                            tb=v["Tb"], ta=v["Ta"], sas=v["SAS"])
            expected.update({f"cbor{n}": v[f"CBOR{n}"] for n in range(11, 17)})
            messages.append(dict(name=mode, input=dict(prs=v["PRS"], ci=v["CI"],
                                 sid=v["sid"], ya=v["ya"], yb=v["yb"]), expect=expected))
    return (write_vector("v06-pake-ci.json", "V6", "配对通道绑定", "gen/pake.py", ci_cases, check)
            + write_vector("v07-pairing-messages.json", "V7", "配对消息", "gen/pake.py", messages, check))
