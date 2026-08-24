# iOS client scaffold

This is an XcodeGen project specification, not an already-signed app bundle.
It deliberately pins `TUSKit` at 3.7.1 and puts it behind
`TUSUploadTransport`, so the app can replace the library without changing
account, queue, or integrity code.

## Before generating Xcode project

1. Install full Xcode (Command Line Tools alone cannot build iOS targets).
2. Install XcodeGen, then run `xcodegen generate` inside `ios/`.
3. Replace both bundle IDs and the App Group in `project.yml` / entitlements
   with identifiers registered to the Apple Developer team.
4. Set `DEVELOPMENT_TEAM`, choose signing, and replace
   `PhotoCloudAPIBaseURL` with the HTTPS hostname configured in Cloudflare.
5. Add the App Group capability to both targets in the Apple Developer portal.
6. Run `make ios-parse` for a fast local syntax/configuration check, then use
   `make ios-test` from full Xcode for a real SDK/package build and XCTest on
   an iPhone 16 simulator. This command does not sign, archive, upload, or
   require paid Apple Developer membership.

The Share Extension appears in Photos' system Share Sheet because it declares
the `com.apple.share-services` extension point and image/movie activation rule.
MVP attempts every selected image or video independently and only copies the temporary
`NSItemProvider` file into the App Group queue.
It never reads Keychain credentials, logs in, or starts a network transfer.

Queue files use `NSFileProtectionCompleteUntilFirstUserAuthentication`: they
remain inside the App Group but can be read by a background URLSession after the
owner has unlocked the phone once following a reboot. This is required for
reliable resume and should be disclosed in the app's security documentation.

The main app keeps refresh/access credentials in Keychain. A password login for
an MFA-enabled account receives only a five-minute MFA challenge; access and
refresh credentials are persisted only after TOTP or one-time recovery-code
verification succeeds. The Security section can enrol TOTP, show recovery codes
once, rotate them after a current TOTP check, and disable MFA after a current
TOTP check.

The app hashes the copied original before creating an upload session, then gives
TUSKit only a capability JWT scoped to that one server upload session. TUSKit can
persist that narrow capability to resume; it must never receive the general
access JWT.

Queue reconciliation distinguishes `created`/`uploading` from the server's
`received`/`verifying`/`verified`/`committing` states. A local item becomes
`available` only after the API returns server state `available`; `quarantined`
keeps the source copy and surfaces an integrity failure.

For a large original, verification can outlast the initial foreground polling
window. The Uploads tab keeps the item in `verifying`; its “Resume and check
status” action rechecks server state without reuploading bytes.

The Library tab calls the authenticated asset-list endpoint and downloads an
original only when its detail view is opened. Original bytes stay in the app's
private cache; before being cached, each download is streamed through SHA-256
and must match the server's verified asset digest. No stable download URL or
bearer credential is exposed to the Share Extension.

The Library follows the server's opaque `next_cursor` until no page remains, so
libraries larger than 50 assets are not truncated. Infinite scrolling has an
explicit Load More fallback when automatic prefetch does not fire.

The App Group payload and its queue record are removed only after the server
reports `available`, which means verification and durable commit have finished.
Failed, interrupted, and quarantined uploads retain their local payload. Cleanup
is idempotent and retries on the next queue reload after an interrupted delete.
Malformed queue-record JSON is isolated without deleting its payload. The app
can explicitly recover an unambiguous image/video payload as a fresh upload UUID
so lost immutable metadata can never accidentally resume an old server session.

Upload diagnostics persist bounded, token-redacted JSONL events containing queue
and server IDs, offsets, app state, timestamps, and errors. The app can export
the current plus rotated diagnostics log for a device test; diagnostics are not
an excuse to log bearer tokens, filenames, EXIF/GPS, or media bytes.

The generated project still requires a physical-device acceptance test for
Share Sheet import, background URLSession wakeup, app termination, airplane
mode, Wi-Fi/cellular switches, and an integrity mismatch response.
