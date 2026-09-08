# Device preparation debug — 2026-09-07

Baseline: `528ec2396e47f0741a29a503d0e0f8c8c787713c` in `/Users/phanan/Cloud_project`.
Changes were prepared in an isolated local copy and then applied to the original checkout.
No deployment, credentials, production data, commits, or pushes are part of this patch.

## Findings and changes

| Priority | Reproduction / previous behavior | Change |
| --- | --- | --- |
| P1 | Sign out while a refresh/login response is pending; an actor resumes after an await and can persist obsolete credentials. Logout also kept credentials until the remote request finished. | Clear credentials before remote logout; fence asynchronous authentication results with a generation ID, including failure paths. |
| P1 | Open a verified original, sign out or change accounts. A failed library reload left the previous list and cached originals accessible; an old in-flight page/download could repopulate the view. | Clear library/cache references and recreate detail navigation on authentication changes; reject obsolete page/download completions. Block new library requests during authentication transitions. |
| P1/P2 | List assets or devices with PostgreSQL microsecond timestamps on older supported iOS/Foundation. The default ISO8601 decoder rejects the fractional form emitted by Go. | Parse both fractional and whole-second RFC3339 dates; still reject malformed dates. |
| P2 | Open app, go to Photos, share another original, return to app. Foreground handling only polled verifying records and did not start the new queued original. A pre-login failed item also stayed failed after login. | Scan/start the queue on foreground; explicitly retry on successful password/MFA login; coalesce overlapping scans. |
| P2 | A locally failed/missing TUS transfer already finished, expired, or became terminal on the server. Retry could bypass server state, recoverTransfer could attach stale metadata before checking terminal state, and reconcile turned HTTP 410 into endless verification polls. | Reconcile server state before explicit retry/reattachment; recover missing metadata; retire expired transfer before saving fresh session state. Keep original bytes and clientAssetID. |
| P2 | Cancellation of an obsolete transfer delivers a late completion/failure after a replacement task starts. | Match both TUS task UUID and server session ID before mutating a queue record; protect available/quarantined states. |

The timestamp compatibility rationale is documented in the upstream Foundation implementation: https://github.com/swiftlang/swift-foundation/blob/main/Sources/FoundationEssentials/Formatting/Date%2BISO8601FormatStyle.swift (fraction parsing before Swift 6.2).

## Validation actually run

- PASS: `go test -race ./...` with Go 1.26.7.
- PASS: `go test -tags=integration -count=1 ./internal/upload ./internal/account` using disposable embedded PostgreSQL; upload 13.854s, account 75.143s.
- PASS: `go vet ./...`.
- PASS: `make ios-parse` (Swift syntax and XcodeGen YAML parsing only).
- PASS: Compose config validation with `.env.example` and all Makefile profiles.
- PASS: shell syntax checks and `git diff --check`.
- Added 13 XCTest regression cases: 3 API-date cases, 2 delayed authentication cases, 5 expired-transfer/callback cases, 3 library account-isolation cases.
- NOT RUN: XCTest/SDK build. `scripts/ios-test.sh` exits 2 because full Xcode is absent. The installed Command Line Tools SDK also fails to import Foundation due to missing `CarbonCore/Folders.h`; a standalone Swift runtime probe could not execute. No Swift type-check, TUSKit package build, signing, simulator run, or device run is claimed.

The successful Go tests establish backend baseline coverage, not validation of the Swift changes. Run the iOS job on full Xcode before calling this device-ready.

## Physical-device acceptance

Use a test account and disposable media. Record app build/commit, iOS version, host version, and export upload diagnostics after each failure.

1. Install full Xcode and XcodeGen, configure the real HTTPS API hostname, signing team, bundle IDs, and shared App Group in `ios/project.yml` and both entitlements. The checked-in hostname remains `photos.example.com`; setup is still required.
2. Run `make ios-test` and require the new XCTest cases to pass. Include an iOS 17/18 runtime or older supported physical device for the timestamp compatibility check.
3. Keep the app installed and previously opened, share a HEIC/JPEG and MOV from Photos, return to app. Both must start without tapping Resume. Repeat while signed out, then sign in: retained originals must start.
4. Upload a video larger than the 32 MiB chunk size, enable airplane mode mid-transfer, restore connectivity, switch Wi-Fi/cellular, lock/unlock. Confirm eventual server `available`, then download and compare SHA-256. Never treat a locally finished TUS task as proof of server verification.
5. Terminate/reopen the app mid-transfer and during server verification. On explicit user force-quit, reopen the app before assessing resume. Confirm a single server asset, no duplicate transfer task, and retained local bytes until `available`.
6. In a disposable backend, expire an incomplete session and reopen/tap Resume. Confirm fresh transfer state instead of infinite `verifying`; force a local cleanup failure and verify the original record/payload survive.
7. Delay login/refresh/logout responses using a test proxy. Sign out/change account while requests are pending. Old responses must not reauthenticate or restore the previous library. Test another account with a different verified photo and revisit Library/detail.
8. Exercise current-device revocation, MFA confirmation/disable, and sign-out while an original download is in progress. Previous-account detail and late download results must disappear.
9. With test data only, produce a deliberate digest mismatch. Confirm quarantine keeps the local payload and never displays the asset as verified.

No physical-device acceptance result is recorded yet. These are executable manual scenarios, not passed tests.
