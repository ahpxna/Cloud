#!/usr/bin/env bash
# Generate the local Xcode project and run the iOS unit tests on a simulator.
# This never signs, archives, uploads, or changes Apple account state.
set -euo pipefail

if ! xcodebuild -version >/dev/null 2>&1; then
  printf 'Full Xcode is required. Install it from the Mac App Store, open it once, then run this command again.\n' >&2
  exit 2
fi

if ! command -v xcodegen >/dev/null 2>&1; then
  printf 'XcodeGen is required. Install it with: brew install xcodegen\n' >&2
  exit 2
fi

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../ios" && pwd)"
cd "$project_dir"

xcodegen generate
xcodebuild \
  -project FamilyPhotoCloud.xcodeproj \
  -scheme FamilyPhotoCloud \
  -sdk iphonesimulator \
  -destination 'platform=iOS Simulator,name=iPhone 16' \
  CODE_SIGNING_ALLOWED=NO \
  test
