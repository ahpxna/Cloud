# App Store readiness — 2026-09-07, second debug pass

**Decision: not ready to submit. Approval cannot be predicted or guaranteed.**
The app has useful account-based photo upload and verification functionality, but
there is still no signed device build or executed iOS test evidence.

## Changes in this pass

- Added PhotosPicker import inside Uploads (up to 20 selected photos/videos), using file transfers into the durable queue without requesting whole-library access.
- Added Help & privacy: setup-independent instructions, backup semantics, optional diagnostics guidance, and configurable HTTPS privacy/support links.
- Share Extension supports up to 20 images/videos, guards duplicate submission, and shows an import receipt explaining that queueing is not completed backup.
- Fixed import publication race: copying an old photo preserves its old modification time, allowing a concurrent queue reader to mistake it for an abandoned payload. Refresh staging-file modification time before publication; regression test scans exactly between payload and record publication.
- Bound newly started uploads to their initial account before hashing. Account changes stop the old queue drain; old bound records are hidden from other accounts. Legacy server-linked records bind only after a successful owner-authenticated session lookup. Share imports remain unbound until first started by a signed-in account.
- Error export now keeps type and numeric code, not free-form descriptions, nested URLs, filenames or arbitrary server text.
- Restricted media URLs to the configured origin including port and disallowed embedded credentials. Reject API base paths/query strings that made login and other endpoints disagree.
- Added required-reason file-metadata manifests to both targets and explicit resource packaging. These declare C617.1 for container metadata; they are not a completed App Store Connect data collection disclosure.
- Added explicit app Info.plist with API, privacy, support build settings.
- Added a structural device-archive checker and six Python tests; CI runs these tests. iOS test script requires Xcode 26+ and discovers an available iPhone simulator instead of hard-coding iPhone 16.

## Verification

- PASS: six Python release-check tests, including incomplete bundle, SDK, URL, icon-reference and extension-manifest fixtures.
- PASS: Swift syntax/XcodeGen YAML parse, plist parsing, shell syntax and diff whitespace checks.
- Added eight XCTest regression cases in this pass: import race (1), log privacy (2), URL constraints (2), queue ownership/persistence (3).
- NOT RUN: the new XCTest cases and prior 13 cases, full Swift type-check, dependency build, device SDK archive, signing, simulator/device tests. Full Xcode is absent on this machine; the old Command Line Tools SDK is also incomplete.
- The archive checker validates structure, not Mach-O validity, signatures, actual icon content, network reachability or review acceptance.
- Previous Go race/integration/vet evidence remains the backend baseline; this pass does not modify backend code.

## Submission blockers still requiring real values or decisions

1. Install/select Xcode 26+, generate project, resolve TUSKit and require XCTest + device archive to pass. Since April 28, 2026, Apple requires Xcode 26+ with iOS 26+ SDK for uploads. Deployment target can remain iOS 17.
2. Replace `PHOTO_CLOUD_API_BASE_URL` with the reachable production HTTPS origin. Both signing teams and registered App Group identifiers must be configured consistently.
3. Supply and publish real privacy-policy and support pages; set `PHOTO_CLOUD_PRIVACY_POLICY_URL` and `PHOTO_CLOUD_SUPPORT_URL`. Help intentionally shows missing configuration in development; the archive check rejects missing/placeholder URLs.
4. Supply a final app icon/asset catalog. None is invented or represented as completed by this pass.
5. Complete privacy labels from actual data flow: photos/videos (including embedded location/audio metadata), account email/user identity, device session information, security logs, server and Cloudflare processing, support submissions and backup retention. Do not mark “Data Not Collected”. Review the generated Xcode privacy report and TUSKit's resolved archive contents.
6. Provide App Review with an active demo account and a reachable backend, disposable example content, and precise review instructions. Do not supply real family photos/credentials.
7. Decide distribution: existing design is invite-only family accounts. Consider Apple's unlisted distribution for limited audiences; it still requires App Review and is not a beta bypass. Use TestFlight for testing.
8. Account deletion: repository currently creates accounts with an administrator CLI; app has no signup or deletion UI/endpoint. Apple's rule applies when an app supports account creation. If signup/invite activation is added, include in-app account deletion before submission. Do not label logout/revocation or a contact-support link as deletion. Even invite-only operation needs a real retention/deletion policy; originals, audit records and backups make this a backend lifecycle feature, not a UI-only button.
9. Complete actual screenshots, age-rating questions, category/description, privacy/support URLs, review notes, export-compliance responses, and applicable regional account declarations in App Store Connect. No export-compliance exemption is assumed by code.
10. Test fresh install, Photos import/share, low storage, iCloud-only selected media, slow upload, airplane mode, background/force-quit/reopen, digest mismatch, sign-out/account switch, MFA and device revocation. Verify SHA-256 and originals retained until available.

## Commands after configuring Xcode and signing

```bash
make ios-test
# Optional: IOS_TEST_DESTINATION='platform=iOS Simulator,id=...' make ios-test
# Build an archive in Xcode, then inspect the actual device .app:
python3 scripts/ios-release-check.py /path/to/FamilyPhotoCloud.xcarchive/Products/Applications/FamilyPhotoCloud.app
```

The checked-in configuration is deliberately not submission-ready: the archive
check must fail until a genuine hostname, published URLs, icon and device build exist.

## Apple sources checked

- [App Review Guidelines](https://developer.apple.com/app-store/review/guidelines/): completeness, functional review access, privacy policy, account-based features and deletion when account creation is supported.
- [SDK minimum requirements](https://developer.apple.com/news/upcoming-requirements/): Xcode 26/iOS 26 upload requirement effective April 28, 2026.
- [Required-reason API categories](https://developer.apple.com/documentation/bundleresources/app-privacy-configuration/nsprivacyaccessedapitypes/nsprivacyaccessedapitype): file metadata/container reason C617.1.
- [Unlisted distribution](https://developer.apple.com/support/unlisted-app-distribution): limited-audience link distribution, final app and review requirements.
- [Offering account deletion](https://developer.apple.com/support/offering-account-deletion-in-your-app): deletion includes associated data, not just disabling access.

This is an engineering readiness assessment of this checkout, not a decision by Apple.
