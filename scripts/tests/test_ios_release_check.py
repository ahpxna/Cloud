import importlib.util
import plistlib
import tempfile
import unittest
from pathlib import Path

SPEC = importlib.util.spec_from_file_location("release_check", Path(__file__).parents[1] / "ios-release-check.py")
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class ReleaseCheckTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.app = Path(self.temp.name) / "FamilyPhotoCloud.app"
        self.extension = self.app / "PlugIns" / "Share.appex"
        self.extension.mkdir(parents=True)
        self.info = {
            "CFBundleIdentifier": "dev.family.cloud", "CFBundleVersion": "2",
            "CFBundleShortVersionString": "1.0", "CFBundleExecutable": "FamilyPhotoCloud",
            "PhotoCloudAPIBaseURL": "https://photos.familycloud.dev",
            "PhotoCloudPrivacyPolicyURL": "https://familycloud.dev/privacy",
            "PhotoCloudSupportURL": "https://familycloud.dev/support",
            "DTSDKName": "iphoneos26.0",
            "CFBundleIcons": {"CFBundlePrimaryIcon": {"CFBundleIconName": "AppIcon"}},
        }
        self.write(self.app / "Info.plist", self.info)
        self.write(self.extension / "Info.plist", {
            "CFBundleVersion": "2", "CFBundleShortVersionString": "1.0",
            "NSExtension": {"NSExtensionPointIdentifier": "com.apple.share-services"},
        })
        # Fixtures check validation logic only, not actual binary or SDK behavior.
        (self.app / "FamilyPhotoCloud").write_bytes(b"fixture")
        (self.app / "Assets.car").write_bytes(b"fixture")
        for bundle in [self.app, self.extension]:
            self.write(bundle / "PrivacyInfo.xcprivacy", {"NSPrivacyAccessedAPITypes": [{
                "NSPrivacyAccessedAPIType": "NSPrivacyAccessedAPICategoryFileTimestamp",
                "NSPrivacyAccessedAPITypeReasons": ["C617.1"],
            }]})

    def write(self, path, value):
        path.write_bytes(plistlib.dumps(value))

    def test_complete_fixture_passes_structural_check(self):
        self.assertEqual(CHECK.check_app(self.app), [])

    def test_missing_bundle_reports_errors_without_crashing(self):
        self.assertTrue(CHECK.check_app(self.app / "missing.app"))

    def test_blocks_placeholder_credentials_and_nonpublic_urls(self):
        for url in ["https://photos.example.com", "https://family.local", "http://familycloud.dev",
                    "https://user:secret@familycloud.dev", "$(PRIVACY_URL)", "https://x.test", "https://x.dev:bad", "https://127.0.0.1", "https://10.0.0.1"]:
            with self.subTest(url=url):
                self.assertFalse(CHECK.public_https(url))
        self.assertFalse(CHECK.public_https("https://familycloud.dev/api", origin=True))
        self.assertTrue(CHECK.public_https("https://familycloud.dev/privacy"))

    def test_blocks_simulator_and_old_sdk_archives(self):
        for sdk in ["iphonesimulator26.0", "iphoneos18.5", ""]:
            self.info["DTSDKName"] = sdk
            self.write(self.app / "Info.plist", self.info)
            self.assertTrue(any("DTSDKName" in e for e in CHECK.check_app(self.app)))

    def test_detects_missing_extension_manifest_and_mismatched_build(self):
        (self.extension / "PrivacyInfo.xcprivacy").unlink()
        self.write(self.extension / "Info.plist", {"CFBundleVersion": "1"})
        errors = CHECK.check_app(self.app)
        self.assertTrue(any("PrivacyInfo" in e for e in errors))
        self.assertTrue(any("build version" in e for e in errors))

    def test_detects_missing_icon_and_unexpanded_url(self):
        (self.app / "Assets.car").unlink()
        self.info["PhotoCloudAPIBaseURL"] = "$(PHOTO_CLOUD_API_BASE_URL)"
        self.write(self.app / "Info.plist", self.info)
        errors = CHECK.check_app(self.app)
        self.assertTrue(any("App icon" in e for e in errors))
        self.assertTrue(any("PhotoCloudAPIBaseURL" in e for e in errors))


if __name__ == "__main__":
    unittest.main()
