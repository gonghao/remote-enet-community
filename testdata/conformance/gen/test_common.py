import unittest

from common import Tag, encode, public_key, sign, verify


class EncodingTest(unittest.TestCase):
    def test_appendix_a(self) -> None:
        for value, expected in (
            (23, "17"), (24, "1818"), (1000, "1903e8"), (1000000, "1a000f4240"),
            (1000000000000, "1b000000e8d4a51000"), (-1000, "3903e7"),
            (b"\x01\x02\x03\x04", "4401020304"), ("ü", "62c3bc"),
            ([1, [2, 3], [4, 5]], "8301820203820405"), (Tag(1, 1363896240), "c11a514b67b0"),
            ([False, True, None], "83f4f5f6"),
            ({"": 1, 24: 2}, "a21818026001"),
        ):
            with self.subTest(value=value):
                self.assertEqual(encode(value).hex(), expected)

    def test_signature(self) -> None:
        message = bytes(range(256))
        signature = sign("device-client", message)
        key = public_key("device-client")
        self.assertTrue(verify(key, message, signature))
        self.assertFalse(verify(key, message[::-1], signature))
