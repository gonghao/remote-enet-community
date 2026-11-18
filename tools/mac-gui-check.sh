#!/bin/sh
set -eu

usage() {
    printf '%s\n' 'Usage: mac-gui-check.sh --run --source LOGIN:/ABS/PACKAGES --version VERSION [--role both|client|operator]'
    printf '%s\n' 'Default: dry run. Uses private loopback settings and validation bundle IDs; no sudo or vehicle requests.'
}

mac_check_run=false
mac_check_source=
mac_check_version=
mac_check_role=both
while [ "$#" -gt 0 ]; do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --run) mac_check_run=true; shift ;;
        --source|--version|--role)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in --source) mac_check_source=$2 ;; --version) mac_check_version=$2 ;; --role) mac_check_role=$2 ;; esac
            shift 2 ;;
        *) usage >&2; exit 2 ;;
    esac
done
if [ "$mac_check_run" = false ]; then
    printf '%s\n' '{"dryRun":true,"scope":"isolated macOS GUI connection checks"}'
    exit 0
fi
case "$mac_check_source" in ''|-*|*[!A-Za-z0-9_./@:-]*) usage >&2; exit 2 ;; esac
case "$mac_check_source" in *:/*) ;; *) usage >&2; exit 2 ;; esac
case "$mac_check_version" in ''|*[!A-Za-z0-9.-]*) usage >&2; exit 2 ;; esac
[ "${#mac_check_version}" -le 64 ] || exit 2
case "$mac_check_role" in both|client|operator) ;; *) usage >&2; exit 2 ;; esac
[ "$(uname -s)" = Darwin ] || { printf '%s\n' 'This run requires macOS.' >&2; exit 1; }
[ "$(uname -m)" = arm64 ] || { printf '%s\n' 'This package requires an Apple silicon Mac.' >&2; exit 1; }
mac_check_owner=$(/usr/bin/id -u)
mac_check_existing=$(/bin/ps -axo uid=,comm= | /usr/bin/awk -v owner="$mac_check_owner" '$1==owner {$1=""; sub(/^ +/, ""); if ($0 ~ /\/renet-(client|operator)$/) count++} END {print count+0}')
[ "$mac_check_existing" = 0 ] || { printf '%s\n' 'Quit existing Client/Operator windows first; this script does not stop applications.' >&2; exit 1; }

umask 077
mac_check_dir=$(mktemp -d /tmp/remote-enet-wave16.XXXXXXXX)
mac_check_dir=$(cd "$mac_check_dir" && pwd -P)
/sbin/ifconfig -l > "$mac_check_dir/interfaces-before.txt"
/usr/sbin/netstat -rn -f inet > "$mac_check_dir/routes-before.txt"
mac_check_roles=$mac_check_role
[ "$mac_check_role" != both ] || mac_check_roles='client operator'
for mac_check_item in $mac_check_roles; do
    mac_check_zip="remote-enet-$mac_check_item-macos-arm64-$mac_check_version.zip"
    /usr/bin/scp "$mac_check_source/$mac_check_zip" "$mac_check_dir/$mac_check_zip"
    /usr/bin/scp "$mac_check_source/$mac_check_zip.sha256" "$mac_check_dir/$mac_check_zip.sha256"
    (cd "$mac_check_dir" && /usr/bin/shasum -a 256 -c "$mac_check_zip.sha256")
    /usr/bin/ditto -x -k "$mac_check_dir/$mac_check_zip" "$mac_check_dir/package-$mac_check_item"
    mac_check_name=Client
    [ "$mac_check_item" != operator ] || mac_check_name=Operator
    mac_check_app="$mac_check_dir/package-$mac_check_item/Remote ENET $mac_check_name.app"
    mac_check_plist="$mac_check_app/Contents/Info.plist"
    mac_check_original_id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$mac_check_plist")
    case "$mac_check_original_id" in ''|*[!A-Za-z0-9.-]*) exit 1 ;; esac
    # NSUserDefaults is scoped by bundle ID on macOS, even with --config.
    # Re-sign only the private copy so language choices cannot alter the real app.
    /usr/libexec/PlistBuddy -c "Set :CFBundleIdentifier $mac_check_original_id.validation.wave16" "$mac_check_plist"
    /usr/bin/codesign --force --deep --sign - "$mac_check_app" > "$mac_check_dir/codesign-$mac_check_item.log" 2>&1
    /usr/bin/codesign --verify --deep --strict "$mac_check_app"
    "$mac_check_app/Contents/MacOS/renet-$mac_check_item" --version > "$mac_check_dir/version-$mac_check_item.txt"
    cat > "$mac_check_dir/$mac_check_item.json" <<EOF
{"version":1,"role":"$mac_check_item","allow_link_test":false,"client":{"nic_name":"lo0","nic_addr":"127.0.0.1","peer":""},"operator":{"listen":"127.0.0.1:58000","endpoint":"127.0.0.1:58000","bind":"127.0.0.1","diagnostic_port":58001,"control_port":58002,"discovery_port":58003,"exclusive":false}}
EOF
    /usr/bin/open -n "$mac_check_app" --args --config "$mac_check_dir/$mac_check_item.json" --state-dir "$mac_check_dir/state-$mac_check_item"
done
printf 'Validation directory: %s\n' "$mac_check_dir"
printf '%s\n' 'Open Settings or Log -> Connection checks -> Run checks.'
printf '%s\n' 'Client: loopback adapter should pass; no pairing/session is expected.'
printf '%s\n' 'Operator: run once idle; then Start listening and run again. Active ports must be skipped.'
printf '%s\n' 'Switch languages, try Cancel, and Copy check results. No MGU authorization is needed.'
printf '%s\n' 'Close both windows to finish. Keep this private evidence directory; do not paste keys or invitations.'
