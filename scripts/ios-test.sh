#!/usr/bin/env bash
# Generate the local Xcode project and run the iOS unit tests on a simulator.
# This never signs, archives, uploads, or changes Apple account state.
set -euo pipefail

if ! xcodebuild -version >/dev/null 2>&1; then
  printf 'Full Xcode is required. Install it from the Mac App Store, open it once, then run this command again.\n' >&2
  exit 2
fi

xcode_major="$(xcodebuild -version | awk '/^Xcode / { split($2, version, "."); print version[1] }')"
if [[ ! "$xcode_major" =~ ^[0-9]+$ ]] || (( xcode_major < 26 )); then
  printf 'Xcode 26 or later is required for the App Store SDK baseline. Select it with DEVELOPER_DIR or xcode-select.\n' >&2
  exit 2
fi

if ! command -v xcodegen >/dev/null 2>&1; then
  printf 'XcodeGen is required. Install it with: brew install xcodegen\n' >&2
  exit 2
fi

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../ios" && pwd)"
cd "$project_dir"

destination="${IOS_TEST_DESTINATION:-}"
if [[ -z "$destination" ]]; then
  simulator_id="$(xcrun simctl list devices available --json | python3 -c '
import json, re, sys
catalog = json.load(sys.stdin)["devices"]
choices = []
for runtime, devices in catalog.items():
    version = re.search(r"iOS-(\d+(?:-\d+)*)$", runtime)
    if not version:
        continue
    for device in devices:
        if device.get("isAvailable") and device.get("name", "").startswith("iPhone"):
            choices.append((tuple(map(int, version[1].split("-"))), device["udid"]))
if not choices:
    sys.exit("No available iPhone simulator. Install an iOS runtime in Xcode or set IOS_TEST_DESTINATION.")
print(max(choices)[1])
')"
  destination="platform=iOS Simulator,id=$simulator_id"
fi

xcodegen generate
xcodebuild \
  -project FamilyPhotoCloud.xcodeproj \
  -scheme FamilyPhotoCloud \
  -sdk iphonesimulator \
  -destination "$destination" \
  CODE_SIGNING_ALLOWED=NO \
  test
