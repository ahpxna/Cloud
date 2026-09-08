#!/usr/bin/env python3
"""Inspect a built device .app before App Store submission; never upload it."""
import argparse
import ipaddress
import plistlib
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit


def public_https(raw, *, origin=False):
    if not isinstance(raw, str) or not raw or raw != raw.strip() or "$" in raw:
        return False
    try:
        url = urlsplit(raw)
        host = (url.hostname or "").lower()
        port = url.port
    except ValueError:
        return False
    try:
        if not ipaddress.ip_address(host).is_global:
            return False
    except ValueError:
        pass
    reserved = ("example.com", "example.org", "example.net", "localhost")
    if (url.scheme != "https" or not host or "." not in host
            or url.username is not None or url.password is not None or url.fragment
            or any(host == domain or host.endswith("." + domain) for domain in reserved)
            or host.endswith((".test", ".invalid", ".localhost", ".local"))):
        return False
    return not origin or (url.path in ("", "/") and not url.query)


def read_plist(path, errors):
    try:
        with path.open("rb") as stream:
            result = plistlib.load(stream)
        if not isinstance(result, dict):
            raise ValueError("expected dictionary")
        return result
    except (OSError, ValueError, plistlib.InvalidFileException) as error:
        errors.append(f"{path.name}: missing or invalid plist ({type(error).__name__})")
        return {}


def check_app(app):
    errors = []
    info = read_plist(app / "Info.plist", errors)
    for key in ("PhotoCloudAPIBaseURL", "PhotoCloudPrivacyPolicyURL", "PhotoCloudSupportURL"):
        if not public_https(info.get(key), origin=key == "PhotoCloudAPIBaseURL"):
            errors.append(f"{key}: set a real public HTTPS URL in the built app")
    for key in ("CFBundleIdentifier", "CFBundleShortVersionString", "CFBundleVersion"):
        if not isinstance(info.get(key), str) or not info[key] or "$" in info[key]:
            errors.append(f"{key}: missing or unresolved build setting")
    executable = info.get("CFBundleExecutable", "")
    if not executable or Path(executable).name != executable or not (app / executable).is_file():
        errors.append("CFBundleExecutable: compiled executable missing")
    sdk = re.fullmatch(r"iphoneos(\d+)(?:\.\d+)*", info.get("DTSDKName", ""))
    if not sdk or int(sdk[1]) < 26:
        errors.append("DTSDKName: archive with the iOS 26+ device SDK (Xcode 26+)")
    icons = info.get("CFBundleIcons", {}).get("CFBundlePrimaryIcon", {})
    if not icons.get("CFBundleIconName") or not (app / "Assets.car").is_file():
        errors.append("App icon: missing compiled primary icon/asset catalog")
    extensions = sorted((app / "PlugIns").glob("*.appex"))
    if not extensions:
        errors.append("Share Extension: no embedded .appex")
    for bundle in [app, *extensions]:
        manifest = read_plist(bundle / "PrivacyInfo.xcprivacy", errors)
        reasons = manifest.get("NSPrivacyAccessedAPITypes", [])
        if not any(entry.get("NSPrivacyAccessedAPIType") == "NSPrivacyAccessedAPICategoryFileTimestamp"
                   and "C617.1" in entry.get("NSPrivacyAccessedAPITypeReasons", []) for entry in reasons):
            errors.append(f"{bundle.name}: declare container file-metadata access (C617.1)")
    for extension in extensions:
        extension_info = read_plist(extension / "Info.plist", errors)
        if extension_info.get("CFBundleVersion") != info.get("CFBundleVersion"):
            errors.append(f"{extension.name}: build version differs from app")
        if extension_info.get("CFBundleShortVersionString") != info.get("CFBundleShortVersionString"):
            errors.append(f"{extension.name}: marketing version differs from app")
        if extension_info.get("NSExtension", {}).get("NSExtensionPointIdentifier") != "com.apple.share-services":
            errors.append(f"{extension.name}: expected Share Extension entry point")
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("app", type=Path, help="Path to the device .app inside an .xcarchive")
    args = parser.parse_args()
    errors = check_app(args.app)
    for error in errors:
        print(f"FAIL: {error}")
    if errors:
        return 1
    print("PASS: inspected bundle configuration, SDK, icon reference and privacy manifests.")
    print("Still require signed archive validation, working public URLs/backend, privacy-label review and device/XCTest results.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
