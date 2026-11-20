"""Generate account and numeric code normalization vectors."""

from collections.abc import Callable
from pathlib import Path
import string

from common import write_vector

ACCOUNT_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
# Unicode White_Space excludes the extra control separators accepted by str.isspace().
WHITE_SPACE = ("\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003"
               "\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029"
               "\u202f\u205f\u3000")
REMOVE_SEPARATORS = str.maketrans("", "", WHITE_SPACE + "-")
ASCII_UPPER = str.maketrans(string.ascii_lowercase, string.ascii_uppercase)
ACCOUNT_ALIASES = str.maketrans("ILO", "110")
FULL_WIDTH_DIGITS = str.maketrans("０１２３４５６７８９", string.digits)


def normalize_account_code(value: str) -> str:
    code = value.translate(REMOVE_SEPARATORS).translate(ASCII_UPPER).translate(ACCOUNT_ALIASES)
    if any(c not in ACCOUNT_ALPHABET for c in code):
        raise ValueError("character")
    if len(code) != 8:
        raise ValueError("length")
    return code


def normalize_numeric_code(value: str) -> str:
    code = value.translate(REMOVE_SEPARATORS).translate(FULL_WIDTH_DIGITS)
    if any(c not in string.digits for c in code):
        raise ValueError("character")
    if len(code) != 8:
        raise ValueError("length")
    return code


def make_cases(examples: dict[str, str], normalize: Callable[[str], str],
               section: str) -> list[dict]:
    cases = []
    for name, value in examples.items():
        try:
            code = normalize(value)
        except ValueError as err:
            expect = dict(result="reject", reason=f"{section}/{err}")
        else:
            expect = dict(result="accept", normalized=code)
        cases.append(dict(name=name, input=value, expect=expect))
    return cases


def generate(check: bool) -> list[Path]:
    account_cases = make_cases({
        "alphabet-digits-first": "01234567",
        "alphabet-digits-and-letters": "89ABCDEF",
        "alphabet-letters-middle": "GHJKMNPQ",
        "alphabet-letters-last": "RSTVWXYZ",
        "lowercase": "abcdefgh",
        "mixed-case": "aBcDeFgH",
        "aliases": "iIlLoO2Z",
        "mixed-separators": " \taB-cD\u3000Ef\n-Gh\u00a0",
        "uppercase-u": "ABCUEFGH",
        "lowercase-u": "ABCuefgh",
        "seven-symbols": "ABC-DEFG",
        "nine-symbols": "ABCD-EFGH1",
        "invalid-after-nine-symbols": "123456789*",
        "punctuation": "ABC*EFGH",
        "full-width-letter": "ＡBCDEFGH",
        "non-ascii-case-fold": "ABCDſFGH",
    }, normalize_account_code, "4.1")
    for case in account_cases:
        expect = case["expect"]
        if expect["result"] == "accept":
            code = expect["normalized"]
            expect["display"] = code[:4] + "-" + code[4:]

    numeric_cases = make_cases({
        "leading-zeros": "00001234",
        "all-zeros": "00000000",
        "space-grouping": "0123 4567",
        "hyphen-grouping": "0123-4567",
        "full-width-digits": "００９９ ８７６５",
        "unicode-whitespace": "\u2003０0\u202f12\u300034\t5\u00856\u00a0",
        "seven-digits": "1234 567",
        "nine-digits": "1234-56789",
        "invalid-after-nine-digits": "123456789A",
        "mixed-letter": "0123A567",
        "other-decimal-digits": "٠١٢٣٤٥٦٧",
        "zero-width-space": "0123\u200b4567",
        "control-separator": "0123\x1c4567",
        "leading-bom": "\ufeff01234567",
    }, normalize_numeric_code, "4.2")
    return (
        write_vector("v01-account-code.json", "V1", "账号码规范化",
                     "gen/codes.py", account_cases, check)
        + write_vector("v02-numeric-code.json", "V2", "数字码规范化",
                       "gen/codes.py", numeric_cases, check)
    )
