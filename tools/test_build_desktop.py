"""Offline packaging tests; fixture images are never executed."""
from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import plistlib
import struct
import tempfile
import unittest
from unittest.mock import patch
import zipfile

SPEC = importlib.util.spec_from_file_location('build_desktop', Path(__file__).with_name('build-desktop.py'))
assert SPEC is not None and SPEC.loader is not None
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


def mach_image(minimum: int = 13 << 16) -> bytes:
    header = struct.pack('<8I', 0xfeedfacf, 0x100000c, 0, 2, 2, 40, 0, 0)
    commands = struct.pack('<6I', 0x32, 24, 1, minimum, minimum, 0)
    signature = struct.pack('>5I', 0xfade0cc0, 36, 1, 0, 20)
    signature += struct.pack('>4I', 0xfade0c02, 16, 0x20400, 2)
    return header + commands + struct.pack('<4I', 0x1d, 16, 72, len(signature)) + signature


def pe_image(gui: bool = True) -> bytes:
    data = bytearray(256)
    data[:2] = b'MZ'
    struct.pack_into('<I', data, 60, 64)
    data[64:68] = b'PE\0\0'
    struct.pack_into('<H', data, 68, 0x8664)
    struct.pack_into('<H', data, 88, 0x20b)
    struct.pack_into('<H', data, 156, 2 if gui else 3)
    return bytes(data)


class PackagingTests(unittest.TestCase):
    def test_diagnostics_version_grammar_and_final_length(self) -> None:
        for version in ['dev', '0.1.0', 'v1.2.3', '1.2.3-alpha.0', '1.2.3-beta.2', '1.2.3-rc.1', '0.1.0-preview.wave10.0', '0.1.0-preview.abcdef0-worktree']:
            with self.subTest(version=version):
                self.assertEqual(builder.build_version(version, False), version)
        for version in ['', '01.2.3', '1.2.3-rc.01', '0.1.0-preview', '0.1.0-preview.secret', '0.1.0-preview.ABCDEF0', '0.1.0-preview.wave1000', '0.1.0-preview.wave10.0\n', '1.2.3+metadata', '0.1.0-preview.' + 'a' * 41]:
            with self.subTest(version=version), self.assertRaises(ValueError):
                builder.build_version(version, False)
        for version in ['dev', '1.2.3', '1.2.3-rc.1']:
            with self.subTest(version=version), self.assertRaises(ValueError):
                builder.build_version(version, True)
        version = '999999.0.0-preview.' + 'a' * 36
        self.assertEqual(len(builder.build_version(version, True)), 64)
        with self.assertRaises(ValueError):
            builder.build_version(version + 'a', True)
        version = '0.1.0-preview.wave10.0-worktree'
        self.assertEqual(builder.build_version(version, True), version)

    def test_four_layouts_and_hashes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary, patch.object(builder, 'WINTUN_SHA256', builder.sha256(pe_image())):
            for platform in ['windows', 'darwin']:
                for role in ['client', 'operator']:
                    with self.subTest(platform=platform, role=role):
                        binary = mach_image() if platform == 'darwin' else pe_image()
                        helper = mach_image(14 << 16) if (platform, role) == ('darwin', 'operator') else None
                        dll = pe_image() if (platform, role) == ('windows', 'operator') else None
                        result = builder.package(Path(temporary), platform, role, '1.2.3-preview.wave10.0-worktree', 'abc123', True, binary, helper, dll)
                        archive = Path(temporary) / result['archive']
                        self.assertEqual(builder.sha256(archive.read_bytes()), result['sha256'])
                        self.assertEqual(archive.with_suffix('.zip.sha256').read_text(), f'{result["sha256"]}  {archive.name}\n')
                        with zipfile.ZipFile(archive) as bundle:
                            readme = bundle.read('README.txt').decode('utf-8')
                            self.assertIn('预览版', readme)
                            if role == 'operator':
                                self.assertIn('仅网络助手' if platform == 'darwin' else 'Run as administrator', readme)
                            manifest = json.loads(bundle.read('manifest.json'))
                            self.assertEqual(manifest, json.loads(archive.with_suffix('.manifest.json').read_text()))
                            self.assertEqual(manifest['role'], role)
                            self.assertTrue(manifest['dirty'])
                            self.assertEqual(manifest['helper_sha256'], builder.sha256(helper) if helper else None)
                            self.assertEqual(manifest['wintun_sha256'], builder.sha256(dll) if dll else None)
                            for name, metadata in manifest['files'].items():
                                self.assertEqual(builder.sha256(bundle.read(name)), metadata['sha256'])
                                self.assertEqual(oct(bundle.getinfo(name).external_attr >> 16 & 0o777), metadata['mode'])
                            if platform == 'darwin':
                                prefix = f'Remote ENET {role.title()}.app/Contents/'
                                plist = plistlib.loads(bundle.read(prefix + 'Info.plist'))
                                self.assertEqual(plist['CFBundleIdentifier'], f'dev.remote-enet.{role}')
                                self.assertEqual(plist['CFBundleShortVersionString'], '1.2.3')
                                self.assertEqual(plist['RemoteENETPreviewVersion'], manifest['version'])
                                self.assertEqual(plist['LSMinimumSystemVersion'], '14.0.0' if helper else '13.0.0')
                                self.assertEqual(plist['CFBundleLocalizations'], ['en', 'zh-Hans', 'zh-Hant'])
                                self.assertEqual(plist['CFBundleIconFile'], 'AppIcon.icns')
                                self.assertTrue(bundle.read(prefix + 'Resources/AppIcon.icns').startswith(b'icns'))
                                self.assertEqual(bundle.read(prefix + 'Resources/licenses/PixelCode-OFL.txt'), (builder.ROOT / 'internal/ui/assets/fonts/pixel-code/OFL.txt').read_bytes())
                                for language in plist['CFBundleLocalizations']:
                                    resource = bundle.read(prefix + f'Resources/{language}.lproj/InfoPlist.strings').decode('utf-8')
                                    self.assertIn('CFBundleDisplayName', resource)
                                    self.assertIn('NSLocalNetworkUsageDescription', resource)
                                self.assertEqual(prefix + 'MacOS/renet-net-helper' in bundle.namelist(), role == 'operator')
                            else:
                                self.assertIn(f'renet-{role}.exe', bundle.namelist())
                                self.assertEqual(bundle.read('RemoteENET.ico')[:4], b'\x00\x00\x01\x00')
                                self.assertEqual(bundle.read('licenses/PixelCode-OFL.txt'), (builder.ROOT / 'internal/ui/assets/fonts/pixel-code/OFL.txt').read_bytes())
                                self.assertEqual('wintun.dll' in bundle.namelist(), role == 'operator')
                            notice_prefix = prefix + 'Resources/' if platform == 'darwin' else ''
                            self.assertEqual(bundle.read(notice_prefix + 'LICENSE'), (builder.ROOT / 'LICENSE').read_bytes())
                            self.assertIn('AGPL-3.0-only', readme)
                            self.assertEqual(bundle.read(notice_prefix + 'THIRD_PARTY_NOTICES.md'), (builder.ROOT / 'THIRD_PARTY_NOTICES.md').read_bytes())
                            inventory = json.loads(bundle.read(notice_prefix + 'third_party/manifest.json'))
                            self.assertEqual(inventory['projectLicense'], 'AGPL-3.0-only')
                            self.assertEqual(inventory['projectLicenseSHA256'], builder.sha256(bundle.read(notice_prefix + 'LICENSE')))
                            self.assertEqual(inventory['goModSHA256'], builder.sha256((builder.ROOT / 'go.mod').read_bytes()))
                            for component in inventory['components']:
                                for item in component['files']:
                                    self.assertEqual(builder.sha256(bundle.read(notice_prefix + 'third_party/' + item['path'])), item['sha256'])
                            self.assertIn(b'Mozilla Public License', bundle.read(notice_prefix + 'third_party/licenses/go/github.com/hashicorp/yamux/LICENSE'))
                            self.assertIn(b'Prebuilt Binaries License', bundle.read(notice_prefix + 'third_party/licenses/wintun-binary/LICENSE'))

    def test_unreviewed_dependencies_or_missing_notices_do_not_publish(self) -> None:
        original = Path.read_bytes
        license_path = builder.ROOT / 'third_party/licenses/go/github.com/hashicorp/yamux/LICENSE'
        for case in ['missing', 'corrupt', 'go.mod changed', 'project license changed']:
            def read(path: Path) -> bytes:
                if case == 'go.mod changed' and path == builder.ROOT / 'go.mod':
                    return original(path) + b'\n// changed dependency inventory\n'
                if case == 'project license changed' and path == builder.ROOT / 'LICENSE':
                    return b'changed project license'
                if path == license_path:
                    if case == 'missing':
                        raise FileNotFoundError('missing fixture license')
                    if case == 'corrupt':
                        return b'changed license terms'
                return original(path)

            with self.subTest(case=case), tempfile.TemporaryDirectory() as temporary, patch.object(Path, 'read_bytes', read):
                stage = Path(temporary)
                with self.assertRaises((ValueError, FileNotFoundError)):
                    builder.package(stage, 'darwin', 'client', '1.2.3', 'abc', False, mach_image(), None, None)
                self.assertEqual(list(stage.iterdir()), [])

    def test_reject_invalid_images(self) -> None:
        for data in [b'', mach_image()[:50], mach_image()[:-1]]:
            with self.subTest(length=len(data)), self.assertRaises(ValueError):
                builder.macho(data)
        for offset, value in [(4, 0x1000007), (12, 6), (36, 0), (40, 2), (104, 0)]:
            data = bytearray(mach_image())
            struct.pack_into('<I', data, offset, value)
            with self.subTest(offset=offset), self.assertRaises(ValueError):
                builder.macho(data)
        for data in [b'', pe_image()[:90], pe_image(False)]:
            with self.subTest(length=len(data)), self.assertRaises(ValueError):
                builder.pe(data, gui=True)
        data = bytearray(pe_image())
        struct.pack_into('<H', data, 68, 0xaa64)
        with self.assertRaises(ValueError):
            builder.pe(data)

    def test_dll_hash_and_architecture_are_both_required(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'wintun.dll'
            path.write_bytes(pe_image())
            with self.assertRaises(ValueError):
                builder.wintun(path)
            with patch.object(builder, 'WINTUN_SHA256', builder.sha256(path.read_bytes())):
                self.assertEqual(builder.wintun(path), pe_image())
            path.write_bytes(b'not a PE')
            with patch.object(builder, 'WINTUN_SHA256', builder.sha256(path.read_bytes())), self.assertRaises(ValueError):
                builder.wintun(path)

    def test_required_components_and_gui_failure_do_not_publish(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            stage = Path(temporary)
            for platform, binary in [('darwin', mach_image()), ('windows', pe_image())]:
                with self.assertRaises(ValueError):
                    builder.package(stage, platform, 'operator', '1.2.3', 'abc', False, binary, None, None)
            with self.assertRaises(ValueError):
                builder.package(stage, 'windows', 'client', '1.2.3', 'abc', False, pe_image(False), None, None)
            self.assertEqual(list(stage.iterdir()), [])

    def test_build_flags_are_role_specific_and_offline(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / 'binary'
            destination.write_bytes(b'fixture')
            for platform in ['windows', 'darwin']:
                for role in ['client', 'operator']:
                    with patch.object(builder.subprocess, 'run') as command:
                        builder.build(platform, role, destination, '1.2.3')
                    args = command.call_args.args[0]
                    flags = args[args.index('-ldflags') + 1]
                    self.assertIn(f'packageIdentifier=dev.remote-enet.{role}', flags)
                    self.assertEqual('-H=windowsgui' in flags, platform == 'windows')
                    self.assertIn('mygo_nowebview', args)
                    env = command.call_args.kwargs['env']
                    self.assertEqual(env['CGO_ENABLED'], '0')
                    self.assertEqual(env['GOPROXY'], 'off')
                    self.assertEqual(env['GOTOOLCHAIN'], 'local')
            with patch.object(builder.subprocess, 'run') as command:
                builder.build('darwin', 'operator', destination, '1.2.3', helper=True)
            self.assertEqual(command.call_args.args[0][-1], './cmd/renet-net-helper')

    def test_failed_build_preserves_existing_output(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            sentinel = output / 'old.zip'
            sentinel.write_bytes(b'existing')
            with patch('sys.argv', ['build-desktop.py', '--platform', 'darwin', '--role', 'client', '--output', str(output)]), patch.object(builder, 'run', side_effect=['abcdef0', '']), patch.object(builder, 'build', side_effect=ValueError('failed')):
                with self.assertRaisesRegex(ValueError, 'failed'):
                    builder.main()
            self.assertEqual(list(output.iterdir()), [sentinel])
            self.assertEqual(sentinel.read_bytes(), b'existing')

    def test_missing_dll_fails_before_build(self) -> None:
        with patch('sys.argv', ['build-desktop.py', '--platform', 'windows', '--role', 'operator']), patch.object(builder, 'run', side_effect=['abcdef0', '']), patch.object(builder, 'build') as build, patch('sys.stderr'):
            with self.assertRaises(SystemExit) as error:
                builder.main()
            self.assertEqual(error.exception.code, 2)
            build.assert_not_called()

    def test_later_role_failure_publishes_no_earlier_package(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            dll = output / 'input.dll'
            dll.write_bytes(pe_image())
            with patch('sys.argv', ['build-desktop.py', '--platform', 'windows', '--role', 'all', '--output', str(output), '--wintun-dll', str(dll)]), patch.object(builder, 'run', side_effect=['abcdef0', '']), patch.object(builder, 'WINTUN_SHA256', builder.sha256(pe_image())), patch.object(builder, 'build', side_effect=[pe_image(), ValueError('second role failed')]):
                with self.assertRaisesRegex(ValueError, 'second role failed'):
                    builder.main()
            self.assertEqual(list(output.iterdir()), [dll])

    def test_cli_publishes_only_complete_artifacts(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            with patch('sys.argv', ['build-desktop.py', '--platform', 'darwin', '--role', 'client', '--output', str(output), '--version', '0.1.0-preview.wave10.0']), patch.object(builder, 'run', side_effect=['abcdef0', 'modified']), patch.object(builder, 'build', return_value=mach_image()), patch('builtins.print'):
                builder.main()
            self.assertEqual(len(list(output.iterdir())), 3)
            manifest = json.loads(next(output.glob('*.json')).read_text())
            self.assertEqual(manifest['version'], '0.1.0-preview.wave10.0-worktree')
            self.assertEqual(manifest['revision'], 'abcdef0')


if __name__ == '__main__':
    unittest.main()
