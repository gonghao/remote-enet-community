#!/usr/bin/env python3
"""Build desktop preview archives without starting any packaged executable."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import stat
import struct
import subprocess
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parent.parent
VERSION_PATTERN = re.compile(r'v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-(alpha|beta|rc)\.(0|[1-9][0-9]{0,8})|-preview\.(wave[0-9]{1,3}(\.[0-9]{1,3})?|[0-9a-f]{7,40})(-worktree)?)?')
WINTUN_SHA256 = 'e5da8447dc2c320edc0fc52fa01885c103de8c118481f683643cacc3220dafce'


def build_version(version: str, dirty: bool) -> str:
    if dirty and not version.endswith('-worktree'):
        version += '-worktree'
    if len(version.encode('utf-8')) > 64 or (version != 'dev' and VERSION_PATTERN.fullmatch(version) is None):
        raise ValueError('Version must match the diagnostics grammar and fit 64 ASCII bytes; dirty builds require a preview version.')
    return version


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def run(args: list[str], **kwargs: object) -> str:
    return subprocess.check_output(args, cwd=ROOT, text=True, **kwargs).strip()


def pe(data: bytes, gui: bool = False) -> None:
    if len(data) < 64 or data[:2] != b'MZ':
        raise ValueError('Expected a PE executable.')
    offset = struct.unpack_from('<I', data, 60)[0]
    if offset + 94 > len(data) or data[offset:offset + 4] != b'PE\0\0':
        raise ValueError('Invalid PE header.')
    if struct.unpack_from('<H', data, offset + 4)[0] != 0x8664 or struct.unpack_from('<H', data, offset + 24)[0] != 0x20b:
        raise ValueError('Expected an amd64 PE32+ image.')
    if gui and struct.unpack_from('<H', data, offset + 92)[0] != 2:
        raise ValueError('Expected the Windows GUI subsystem.')


def wintun(path: Path) -> bytes:
    data = path.read_bytes()
    if sha256(data) != WINTUN_SHA256:
        raise ValueError('Wintun DLL does not match the pinned official SHA256.')
    pe(data)
    return data


def macho(data: bytes) -> dict[str, object]:
    if len(data) < 32 or struct.unpack_from('<III', data) != (0xfeedfacf, 0x100000c, 0):
        raise ValueError('Expected an arm64 Mach-O executable.')
    if struct.unpack_from('<I', data, 12)[0] != 2:
        raise ValueError('Expected Mach-O MH_EXECUTE.')
    count, size = struct.unpack_from('<II', data, 16)
    end = 32 + size
    if end > len(data):
        raise ValueError('Truncated Mach-O commands.')
    position, minimum, signed = 32, None, False
    for _ in range(count):
        if position + 8 > end:
            raise ValueError('Truncated Mach-O command.')
        command, length = struct.unpack_from('<II', data, position)
        if length < 8 or position + length > end:
            raise ValueError('Invalid Mach-O command length.')
        if command == 0x32:
            if length < 24:
                raise ValueError('Truncated build version.')
            platform, packed = struct.unpack_from('<II', data, position + 8)
            if platform != 1:
                raise ValueError('Expected the macOS platform.')
            minimum = (packed >> 16, (packed >> 8) & 255, packed & 255)
        if command == 0x1d:
            if length < 16:
                raise ValueError('Truncated code signature command.')
            offset, size = struct.unpack_from('<II', data, position + 8)
            if size < 12 or offset + size > len(data):
                raise ValueError('Invalid code signature bounds.')
            magic, total, slots = struct.unpack_from('>III', data, offset)
            if magic != 0xfade0cc0 or total > size or total < 12 + 8 * slots:
                raise ValueError('Invalid code signature superblob.')
            for slot in range(slots):
                kind, relative = struct.unpack_from('>II', data, offset + 12 + slot * 8)
                if relative + 16 > total:
                    raise ValueError('Invalid code signature slot.')
                if kind == 0:
                    cmagic, length_cd, _, flags = struct.unpack_from('>IIII', data, offset + relative)
                    signed = cmagic == 0xfade0c02 and 16 <= length_cd <= total - relative and bool(flags & 2)
        position += length
    if position != end or minimum is None or not signed:
        raise ValueError('Expected macOS minimum version and an ad hoc CodeDirectory marker.')
    return {'minimum_macos': '.'.join(map(str, minimum)), 'ad_hoc_marker': True}


def build(platform: str, role: str, destination: Path, version: str, helper: bool = False) -> bytes:
    name = 'renet-net-helper' if helper else f'renet-{role}'
    flags = '-s -w'
    if not helper:
        flags += f' -X "github.com/egoist/mygo.packageName=Remote ENET {role.title()}" -X main.version={version} -X github.com/egoist/mygo.production=1 -X github.com/egoist/mygo.packageIdentifier=dev.remote-enet.{role} -X github.com/egoist/mygo.packageVersion={version}'
        if platform == 'windows':
            flags += ' -H=windowsgui'
    env = dict(os.environ, CGO_ENABLED='0', GOOS=platform, GOARCH='amd64' if platform == 'windows' else 'arm64', GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local')
    subprocess.run(['go', 'build', '-tags', 'mygo_nowebview', '-trimpath', '-ldflags', flags, '-o', str(destination), f'./cmd/{name}'], cwd=ROOT, env=env, check=True)
    return destination.read_bytes()


def third_party_notices() -> dict[str, bytes]:
    manifest_path = ROOT / 'third_party' / 'manifest.json'
    encoded = manifest_path.read_bytes()
    manifest = json.loads(encoded)
    if manifest.get('schemaVersion') != 1 or manifest.get('goModSHA256') != sha256((ROOT / 'go.mod').read_bytes()):
        raise ValueError('Third-party notice inventory must be reviewed after go.mod changes.')
    project_license = (ROOT / 'LICENSE').read_bytes()
    if manifest.get('projectLicense') != 'AGPL-3.0-only' or manifest.get('projectLicenseSHA256') != sha256(project_license):
        raise ValueError('Project license does not match its reviewed inventory.')
    entries = {
        'LICENSE': project_license,
        'THIRD_PARTY_NOTICES.md': (ROOT / 'THIRD_PARTY_NOTICES.md').read_bytes(),
        'third_party/manifest.json': encoded,
    }
    for component in manifest['components']:
        for item in component['files']:
            relative = Path(item['path'])
            if relative.is_absolute() or '..' in relative.parts or relative.parts[:1] != ('licenses',):
                raise ValueError('Invalid third-party license inventory path.')
            source = ROOT / 'third_party' / relative
            if source.is_symlink():
                raise ValueError('Third-party license snapshots must be regular files.')
            data = source.read_bytes()
            if sha256(data) != item['sha256']:
                raise ValueError(f'Third-party license snapshot does not match its reviewed digest: {relative.as_posix()}')
            entries['third_party/' + relative.as_posix()] = data
    return entries


def package(stage: Path, platform: str, role: str, version: str, revision: str, dirty: bool, binary: bytes, helper: bytes | None, dll: bytes | None) -> dict[str, object]:
    architecture = 'amd64' if platform == 'windows' else 'arm64'
    title = f'Remote ENET {role.title()}'
    executable = f'renet-{role}'
    entries: dict[str, tuple[bytes, int]] = {}
    signatures: dict[str, object] = {'developer_id': False, 'notarized': False, 'authenticode_verified': False}
    minimum = None
    if platform == 'darwin':
        metadata = macho(binary)
        if role == 'operator':
            if helper is None:
                raise ValueError('macOS Operator requires the real network helper.')
            helper_meta = macho(helper)
            metadata['minimum_macos'] = max(str(metadata['minimum_macos']), str(helper_meta['minimum_macos']), key=lambda v: tuple(map(int, v.split('.'))))
            signatures['helper'] = helper_meta
        signatures['executable'] = metadata
        minimum = metadata['minimum_macos']
        prefix = f'{title}.app/Contents/'
        base = re.match(r'^v?(\d+)\.(\d+)\.(\d+)(?:[.-]|$)', version)
        if base is None and version != 'dev':
            raise ValueError('Version must start with major.minor.patch or be dev.')
        plist = {
            'CFBundleName': title, 'CFBundleDisplayName': title,
            'CFBundleIdentifier': f'dev.remote-enet.{role}', 'CFBundleExecutable': executable,
            'CFBundlePackageType': 'APPL', 'CFBundleInfoDictionaryVersion': '6.0',
            'CFBundleIconFile': 'AppIcon.icns',
            'CFBundleShortVersionString': '.'.join(str(int(n)) for n in base.groups()) if base else '0.0.0',
            'CFBundleVersion': datetime.now(timezone.utc).strftime('%Y.%m.%d'),
            'RemoteENETPreviewVersion': version, 'RemoteENETBuildRevision': revision,
            'RemoteENETBuildDirty': dirty, 'CFBundleDevelopmentRegion': 'en',
            'CFBundleLocalizations': ['en', 'zh-Hans', 'zh-Hant'],
            'LSMinimumSystemVersion': minimum, 'LSArchitecturePriority': ['arm64'],
            'LSApplicationCategoryType': 'public.app-category.utilities', 'NSPrincipalClass': 'NSApplication',
            'NSHighResolutionCapable': True, 'NSSupportsAutomaticGraphicsSwitching': True,
            'NSLocalNetworkUsageDescription': 'Find ENET gateways and relay diagnostic traffic on the selected network.',
        }
        entries[prefix + 'Info.plist'] = (plistlib.dumps(plist), 0o644)
        entries[prefix + 'Resources/AppIcon.icns'] = ((ROOT / 'internal' / 'brand' / 'assets' / 'app-icon.icns').read_bytes(), 0o644)
        entries[prefix + 'Resources/licenses/PixelCode-OFL.txt'] = ((ROOT / 'internal' / 'ui' / 'assets' / 'fonts' / 'pixel-code' / 'OFL.txt').read_bytes(), 0o644)
        for language in ('en', 'zh-Hans', 'zh-Hant'):
            messages = json.loads((ROOT / 'internal' / 'locales' / f'{language}.json').read_text(encoding='utf-8'))
            localized = {
                'CFBundleDisplayName': messages[title],
                'NSLocalNetworkUsageDescription': messages[plist['NSLocalNetworkUsageDescription']],
            }
            strings = '\n'.join(f'{json.dumps(key)} = {json.dumps(value, ensure_ascii=False)};' for key, value in localized.items()) + '\n'
            entries[prefix + f'Resources/{language}.lproj/InfoPlist.strings'] = (strings.encode('utf-8'), 0o644)
        entries[prefix + 'PkgInfo'] = (b'APPL????', 0o644)
        entries[prefix + 'MacOS/' + executable] = (binary, 0o755)
        if helper is not None:
            entries[prefix + 'MacOS/renet-net-helper'] = (helper, 0o755)
    else:
        pe(binary, gui=True)
        entries[executable + '.exe'] = (binary, 0o755)
        entries['RemoteENET.ico'] = ((ROOT / 'internal' / 'brand' / 'assets' / 'app-icon.ico').read_bytes(), 0o644)
        entries['licenses/PixelCode-OFL.txt'] = ((ROOT / 'internal' / 'ui' / 'assets' / 'fonts' / 'pixel-code' / 'OFL.txt').read_bytes(), 0o644)
        if role == 'operator':
            if dll is None or sha256(dll) != WINTUN_SHA256:
                raise ValueError('Windows Operator requires the pinned Wintun DLL.')
            pe(dll)
            entries['wintun.dll'] = (dll, 0o644)
    readme = f'{title} 预览版\n版本：{version}\n源码提交：{revision}\n工作区有改动：{dirty}\n平台：{platform}/{architecture}\n最低 macOS：{minimum or "不适用"}\n\n项目源码许可：AGPL-3.0-only，完整文本见包内 LICENSE；第三方组件条款见 THIRD_PARTY_NOTICES.md。macOS 的许可文件位于应用 Contents/Resources。\n请完整解压后再打开应用。\n本预览包尚未进行 Developer ID 签名、Apple 公证或应用程序 Authenticode 签名。\nGo ad hoc 签名标记不代表 Developer ID 签名，也不代表已通过密码学签名验证。\n跨编译通过不代表原生运行或实车验收通过。\n'
    if role == 'operator':
        if platform == 'darwin':
            readme += 'GUI 和身份文件保持普通用户权限，仅网络助手在明确请求直连后按次获得系统授权；不安装常驻服务。\n'
        else:
            readme += '本轮检测权限和驱动能力并给出提示；用户可按需通过 Run as administrator 运行 Operator。\n随包官方 amd64 Wintun DLL 已校验固定摘要；脚本不安装驱动，也不自动提权。\n'
    entries['README.txt'] = (readme.encode(), 0o644)
    notice_prefix = prefix + 'Resources/' if platform == 'darwin' else ''
    for name, data in third_party_notices().items():
        entries[notice_prefix + name] = (data, 0o644)
    manifest: dict[str, object] = {
        'schema_version': 1, 'role': role, 'platform': platform, 'architecture': architecture,
        'name': title, 'identifier': f'dev.remote-enet.{role}', 'version': version,
        'revision': revision, 'dirty': dirty, 'minimum_macos': minimum,
        'helper_sha256': sha256(helper) if helper is not None else None,
        'wintun_sha256': sha256(dll) if dll is not None else None,
        'signatures': signatures,
        'files': {name: {'sha256': sha256(data), 'mode': oct(mode)} for name, (data, mode) in entries.items()},
    }
    encoded = (json.dumps(manifest, indent=2, sort_keys=True) + '\n').encode()
    entries['manifest.json'] = (encoded, 0o644)
    label = 'macos' if platform == 'darwin' else 'windows'
    archive = stage / f'remote-enet-{role}-{label}-{architecture}-{version}.zip'
    with zipfile.ZipFile(archive, 'w', compression=zipfile.ZIP_DEFLATED) as bundle:
        for name, (data, mode) in entries.items():
            info = zipfile.ZipInfo(name)
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | mode) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            bundle.writestr(info, data)
    with zipfile.ZipFile(archive) as bundle:
        if bundle.testzip() is not None:
            raise ValueError('ZIP integrity check failed.')
        for name, (data, mode) in entries.items():
            if bundle.read(name) != data or stat.S_IMODE(bundle.getinfo(name).external_attr >> 16) != mode:
                raise ValueError('ZIP contents or permissions mismatch.')
    digest = sha256(archive.read_bytes())
    archive.with_suffix('.zip.sha256').write_text(f'{digest}  {archive.name}\n', encoding='utf-8')
    archive.with_suffix('.manifest.json').write_bytes(encoded)
    return {'archive': archive.name, 'sha256': digest}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', choices=['windows', 'darwin', 'all'], default='all')
    parser.add_argument('--role', choices=['client', 'operator', 'all'], default='all')
    parser.add_argument('--output', type=Path, default=ROOT / 'dist')
    parser.add_argument('--version')
    parser.add_argument('--wintun-dll', type=Path)
    args = parser.parse_args()
    revision = run(['git', 'rev-parse', 'HEAD'])
    dirty = bool(run(['git', 'status', '--porcelain', '--untracked-files=normal']))
    try:
        version = build_version(args.version or f'0.1.0-preview.{revision[:7]}', dirty)
    except ValueError as error:
        parser.error(str(error))
    platforms = ['windows', 'darwin'] if args.platform == 'all' else [args.platform]
    roles = ['client', 'operator'] if args.role == 'all' else [args.role]
    dll = None
    if 'windows' in platforms and 'operator' in roles:
        if args.wintun_dll is None:
            parser.error('Windows Operator requires --wintun-dll with the pinned official amd64 DLL.')
        dll = wintun(args.wintun_dll)
    if 'darwin' in platforms and 'operator' in roles and not (ROOT / 'cmd/renet-net-helper/main.go').is_file():
        parser.error('macOS Operator requires cmd/renet-net-helper from work package A.')
    args.output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.renet-build-', dir=args.output) as temporary:
        stage = Path(temporary)
        results = []
        for platform in platforms:
            for role in roles:
                binary = build(platform, role, stage / 'application', version)
                helper = build(platform, role, stage / 'helper', version, helper=True) if platform == 'darwin' and role == 'operator' else None
                results.append(package(stage, platform, role, version, revision, dirty, binary, helper, dll if platform == 'windows' and role == 'operator' else None))
        # All builds and validation complete before exposing any artifact. Each file replacement is atomic.
        for artifact in sorted(stage.iterdir(), key=lambda p: p.suffix == '.zip'):
            if artifact.suffix in {'.zip', '.sha256', '.json'}:
                os.replace(artifact, args.output / artifact.name)
        print(json.dumps(results, indent=2))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        raise SystemExit(str(error)) from error
