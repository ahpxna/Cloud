import CryptoKit
import Foundation
import Network
import UIKit

enum AvailableUploadFinalization {
    case cleaned(QueuedUpload)
    case persistencePending(QueuedUpload, Error)
    case cleanupPending(QueuedUpload, Error)
}

enum AvailableUploadFinalizer {
    static func finalize(
        _ original: QueuedUpload,
        persist: (QueuedUpload) throws -> Void,
        cleanup: (QueuedUpload) throws -> Void
    ) -> AvailableUploadFinalization {
        var item = original
        item.state = .available
        item.lastError = nil

        do {
            try persist(item)
        } catch {
            return .persistencePending(item, error)
        }

        do {
            try cleanup(item)
        } catch {
            return .cleanupPending(item, error)
        }

        return .cleaned(item)
    }
}

enum HashWorker {
    static func sha256(of url: URL) async throws -> String {
        let worker = Task.detached(priority: .utility) { () throws -> String in
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }
            var hasher = SHA256()
            while let data = try handle.read(upToCount: 1 << 20), !data.isEmpty {
                try Task.checkCancellation()
                hasher.update(data: data)
            }
            try Task.checkCancellation()
            return hasher.finalize().map { String(format: "%02x", $0) }.joined()
        }
        return try await withTaskCancellationHandler {
            try await worker.value
        } onCancel: {
            worker.cancel()
        }
    }
}

@MainActor
final class UploadCoordinator: ObservableObject {
    @Published private(set) var uploads: [QueuedUpload] = []
    @Published private(set) var quarantinedRecords: [QuarantinedQueueRecord] = []
    @Published private(set) var diagnosticsExportURL: URL?
    @Published private(set) var mfaChallenge: String?
    @Published private(set) var mfaExpiresAt: Date?
    @Published private(set) var mfaEnrollment: MFAEnrollment?
    @Published private(set) var mfaRecoveryCodes: [String] = []
    @Published private(set) var deviceSessions: [DeviceSession] = []
    @Published private(set) var lastError: String?

    private let api: PhotoCloudAPI
    private let auth: AuthenticationStore
    private let transport: TUSUploadTransport
    private let pathMonitor = NWPathMonitor()
    private let connectivityQueue = DispatchQueue(label: "dev.phanan.FamilyPhotoCloud.connectivity")
    private var verificationRetryTasks: [UUID: Task<Void, Never>] = [:]
    private var verificationRetryCounts: [UUID: Int] = [:]
    private var verificationReconcileInFlight: Set<UUID> = []

    init(apiBaseURL: URL) throws {
        api = PhotoCloudAPI(baseURL: apiBaseURL)
        auth = AuthenticationStore()
        transport = try TUSUploadTransport(api: api) { [api, auth] sessionID in
            let accessToken = try await auth.accessToken(api: api)
            let session = try await api.uploadSession(id: sessionID, accessToken: accessToken)
            return try Self.requireUploadToken(session)
        }
        transport.uploadFinished = { [weak self] _, context in
            Task { @MainActor in await self?.tusFinished(context: context) }
        }
        transport.uploadFailed = { [weak self] _, context, error in
            Task { @MainActor in await self?.tusFailed(context: context, error: error) }
        }
        pathMonitor.pathUpdateHandler = { [weak self] path in
            guard path.status == .satisfied else { return }
            Task { @MainActor [weak self] in
                await self?.reconcileVerifyingUploads(trigger: "network_available")
            }
        }
        pathMonitor.start(queue: connectivityQueue)
    }

    func reload() {
        do { try AppGroupQueue.cleanupCompleted() }
        catch { lastError = error.localizedDescription }
        do { uploads = try AppGroupQueue.all() }
        catch { lastError = error.localizedDescription }
        do { quarantinedRecords = try AppGroupQueue.quarantinedRecords() }
        catch { lastError = error.localizedDescription }
    }

    func login(email: String, password: String) async {
        do {
            let result = try await auth.login(email: email, password: password, deviceName: UIDevice.current.name, api: api)
            switch result {
            case .authenticated:
                mfaChallenge = nil
                mfaExpiresAt = nil
                await startQueuedUploads()
            case .mfaRequired(let challenge, let expiresIn):
                mfaChallenge = challenge
                mfaExpiresAt = Date.now.addingTimeInterval(TimeInterval(expiresIn))
                lastError = nil
            }
        } catch { lastError = error.localizedDescription }
    }

    func verifyMFA(totpCode: String, recoveryCode: String) async {
        guard let challenge = mfaChallenge else { return }
        do {
            let cleanTOTP = totpCode.trimmingCharacters(in: .whitespacesAndNewlines)
            let cleanRecovery = recoveryCode.trimmingCharacters(in: .whitespacesAndNewlines)
            try await auth.verifyMFA(
                challenge: challenge,
                totpCode: cleanRecovery.isEmpty ? cleanTOTP : nil,
                recoveryCode: cleanRecovery.isEmpty ? nil : cleanRecovery,
                api: api
            )
            mfaChallenge = nil
            mfaExpiresAt = nil
            lastError = nil
            await startQueuedUploads()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func beginMFAEnrollment(password: String) async {
        do {
            let accessToken = try await auth.accessToken(api: api)
            mfaEnrollment = try await api.beginMFAEnrollment(
                password: password,
                accessToken: accessToken
            )
            mfaRecoveryCodes = []
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    func confirmMFAEnrollment(totpCode: String) async {
        do {
            let accessToken = try await auth.accessToken(api: api)
            let response = try await api.confirmMFAEnrollment(
                totpCode: totpCode.trimmingCharacters(in: .whitespacesAndNewlines),
                accessToken: accessToken
            )
            mfaEnrollment = nil
            mfaRecoveryCodes = response.recoveryCodes
            // Confirming MFA revokes every pre-MFA refresh family, including
            // this device. Drop cached credentials immediately so the next
            // authentication is guaranteed to pass through the new factor.
            try await auth.invalidateCredential()
            lastError = "MFA enabled. Save the recovery codes, then sign in again."
        } catch {
            lastError = error.localizedDescription
        }
    }

    func rotateMFARecoveryCodes(totpCode: String) async {
        do {
            let accessToken = try await auth.accessToken(api: api)
            let response = try await api.rotateMFARecoveryCodes(
                totpCode: totpCode.trimmingCharacters(in: .whitespacesAndNewlines),
                accessToken: accessToken
            )
            mfaRecoveryCodes = response.recoveryCodes
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    func disableMFA(totpCode: String) async {
        do {
            let accessToken = try await auth.accessToken(api: api)
            try await api.disableMFA(
                totpCode: totpCode.trimmingCharacters(in: .whitespacesAndNewlines),
                accessToken: accessToken
            )
            mfaEnrollment = nil
            mfaRecoveryCodes = []
            // The backend revokes every session when MFA is disabled. Avoid
            // leaving dead access/refresh tokens in memory or the Keychain.
            try await auth.invalidateCredential()
            lastError = "MFA disabled. Sign in again to continue."
        } catch {
            lastError = error.localizedDescription
        }
    }

    func refreshDeviceSessions() async {
        do {
            let accessToken = try await auth.accessToken(api: api)
            deviceSessions = try await api.deviceSessions(accessToken: accessToken)
            lastError = nil
        } catch let problem as APIProblem where problem.status == 401 {
            try? await auth.invalidateCredential()
            deviceSessions = []
            lastError = "Your session expired. Sign in again."
        } catch {
            lastError = error.localizedDescription
        }
    }

    func revokeDeviceSession(_ session: DeviceSession) async {
        do {
            let accessToken = try await auth.accessToken(api: api)
            try await api.revokeDeviceSession(id: session.id, accessToken: accessToken)
            if session.current {
                try await auth.invalidateCredential()
                deviceSessions = []
                lastError = "This device session was revoked. Sign in again to continue."
            } else {
                await refreshDeviceSessions()
            }
        } catch let problem as APIProblem where problem.status == 401 {
            try? await auth.invalidateCredential()
            deviceSessions = []
            lastError = "Your session expired. Sign in again."
        } catch {
            lastError = error.localizedDescription
        }
    }

    func signOut() async {
        do {
            try await auth.signOut(api: api)
            deviceSessions = []
            mfaChallenge = nil
            mfaExpiresAt = nil
            mfaEnrollment = nil
            mfaRecoveryCodes = []
            lastError = nil
        } catch {
            // AuthenticationStore clears Keychain/in-memory credentials even if
            // the remote revoke fails, so the device is still signed out locally.
            deviceSessions = []
            mfaChallenge = nil
            mfaExpiresAt = nil
            mfaEnrollment = nil
            mfaRecoveryCodes = []
            lastError = "Signed out locally, but the server session could not be revoked: \(error.localizedDescription)"
        }
    }

    func recoverQuarantinedRecord(_ record: QuarantinedQueueRecord) async {
        do {
            let recovered = try AppGroupQueue.recoverQuarantinedRecord(record)
            UploadDiagnostics.shared.record("queue_record_recovered", context: ["queue_id": recovered.id.uuidString])
            reload()
            await begin(recovered)
        } catch {
            lastError = error.localizedDescription
            reload()
        }
    }

    func prepareDiagnosticsExport() {
        do { diagnosticsExportURL = try UploadDiagnostics.shared.exportURL() }
        catch { lastError = error.localizedDescription }
    }

    func startQueuedUploads(retryFailed: Bool = false) async {
        reload()
        let failedTUSUploads = transport.resumeStoredUploads()
        for index in uploads.indices {
            guard let tusID = uploads[index].tusUploadID, failedTUSUploads.contains(tusID) else { continue }
            uploads[index].state = .failed
            uploads[index].lastError = "Upload paused after its network retry limit. Tap Resume and check status to retry it."
            try? AppGroupQueue.save(uploads[index])
        }
        for item in uploads where item.state != .available && item.state != .quarantined {
            if item.state == .failed {
                guard retryFailed else { continue }
                if item.serverSessionID == nil {
                    await begin(item)
                    continue
                }
                guard let tusID = item.tusUploadID, failedTUSUploads.contains(tusID) else {
                    await recoverTransfer(item)
                    continue
                }
                do {
                    guard try transport.retryFailedUpload(id: tusID) else {
                        throw URLError(.cannotLoadFromNetwork)
                    }
                    var retrying = item
                    retrying.state = .transferring
                    retrying.lastError = nil
                    try AppGroupQueue.save(retrying)
                } catch {
                    lastError = error.localizedDescription
                }
                continue
            }
            if item.serverSessionID == nil {
                await begin(item)
            } else if item.tusUploadID == nil {
                await recoverTransfer(item)
            } else {
                await reconcile(item)
            }
        }
    }

    func appBecameActive() async {
        await reconcileVerifyingUploads(trigger: "foreground")
    }

    func registerBackgroundHandler(_ completion: @escaping () -> Void, identifier: String) {
        transport.registerBackgroundHandler(completion, identifier: identifier)
    }

    func libraryPage(cursor: String?, limit: Int) async throws -> LibraryPage {
        let accessToken = try await auth.accessToken(api: api)
        return try await api.libraryPage(cursor: cursor, limit: limit, accessToken: accessToken)
    }

    func downloadOriginal(_ asset: LibraryAsset) async throws -> URL {
        let accessToken = try await auth.accessToken(api: api)
        return try await api.downloadOriginal(asset, accessToken: accessToken)
    }

    private func begin(_ original: QueuedUpload) async {
        var item = original
        do {
            let payload = try AppGroupQueue.payloadURL(for: original)
            let digest = try await HashWorker.sha256(of: payload)
            let attributes = try FileManager.default.attributesOfItem(atPath: payload.path())
            guard let size = attributes[.size] as? NSNumber else { throw URLError(.cannotOpenFile) }
            let accessToken = try await auth.accessToken(api: api)
            let session = try await api.createUploadSession(
                CreateUploadRequest(
                    clientAssetID: original.clientAssetID.uuidString,
                    originalFilename: original.originalFilename,
                    mediaType: mediaType(for: original),
                    expectedSize: size.int64Value,
                    clientSHA256: digest
                ),
                accessToken: accessToken
            )
            item.byteCount = size.int64Value
            item.sha256 = digest
            item.serverSessionID = session.id
            UploadDiagnostics.shared.record("session_created", context: [
                "queue_id": item.id.uuidString,
                "session_id": session.id,
            ])
            // Persist the desired transfer before asking TUSKit to create its
            // resource. A crash here is recoverable from the server `created`
            // state rather than becoming a silent stuck upload.
            item.state = .queued
            item.lastError = nil
            try AppGroupQueue.save(item)
            try await enqueueTransfer(&item, session: session, accessToken: accessToken)
            reload()
        } catch {
            // Once a server session exists, retain a queued record and retry
            // reconciliation. Marking it failed would discard resumability.
            item.state = item.serverSessionID == nil ? .failed : .queued
            item.lastError = error.localizedDescription
            try? AppGroupQueue.save(item)
            lastError = error.localizedDescription
            reload()
        }
    }

    private func recoverTransfer(_ original: QueuedUpload) async {
        guard let sessionID = original.serverSessionID else { return }
        var item = original
        do {
            let accessToken = try await auth.accessToken(api: api)
            let session = try await api.uploadSession(id: sessionID, accessToken: accessToken)
            // TUSKit can publish its metadata before this queue record's
            // tusUploadID is atomically saved. Reattach that task after a
            // crash instead of scheduling a second upload for the session.
            if let storedID = transport.storedUploadID(forSessionID: sessionID) {
                item.tusUploadID = storedID
                item.state = transport.isFailedStoredUpload(id: storedID) ? .failed : .transferring
                item.lastError = item.state == .failed
                    ? "Upload paused after its network retry limit. Tap Resume and check status to retry it."
                    : nil
                try AppGroupQueue.save(item)
                reload()
                return
            }
            switch session.state {
            case "created":
                try await enqueueTransfer(&item, session: session, accessToken: accessToken)
            case "uploading":
                // Without a persisted TUSKit context, discard the incomplete
                // server object before rebuilding from byte zero.
                let reset = try await api.restartUploadSession(id: sessionID, accessToken: accessToken)
                item.tusUploadID = nil
                try await enqueueTransfer(&item, session: reset, accessToken: accessToken)
                reload()
                return
            default:
                await reconcile(item)
                return
            }
        } catch let problem as APIProblem where problem.status == 410 && problem.code == "upload_session_expired" {
            // Reusing the queue identity lets the server revive its expired
            // idempotency row. First remove stale local TUS state so a later
            // launch cannot restart the expired transfer in parallel.
            if let tusID = item.tusUploadID ?? transport.storedUploadID(forSessionID: sessionID) {
                try? transport.discardStoredUpload(id: tusID)
            }
            item.serverSessionID = nil
            item.tusUploadID = nil
            item.state = .queued
            item.lastError = nil
            try? AppGroupQueue.save(item)
            await begin(item)
        } catch {
            item.state = .queued
            item.lastError = error.localizedDescription
            try? AppGroupQueue.save(item)
            lastError = error.localizedDescription
        }
        reload()
    }

    private func enqueueTransfer(_ item: inout QueuedUpload, session: UploadSession, accessToken: String) async throws {
        try transport.validateChunkSize(session.recommendedChunkBytes)
        let uploadToken: String
        if let token = session.uploadToken, !token.isEmpty {
            uploadToken = token
        } else {
            let refreshed = try await api.uploadSession(id: session.id, accessToken: accessToken)
            uploadToken = try Self.requireUploadToken(refreshed)
        }
        item.tusUploadID = try transport.enqueue(
            item: item,
            endpoint: try api.absoluteURL(for: session.uploadEndpoint),
            uploadToken: uploadToken
        )
        item.state = .transferring
        item.lastError = nil
        try AppGroupQueue.save(item)
    }

    private func tusFinished(context: [String: String]?) async {
        guard let queueID = context?["queue_id"], let id = UUID(uuidString: queueID), var item = try? AppGroupQueue.all().first(where: { $0.id == id }) else { return }
        item.state = .verifying
        try? AppGroupQueue.save(item)
        reload()
        await reconcile(item)
    }

    private func reconcile(_ original: QueuedUpload) async {
        guard let sessionID = original.serverSessionID else { return }
        guard !verificationReconcileInFlight.contains(original.id) else { return }
        verificationReconcileInFlight.insert(original.id)
        defer { verificationReconcileInFlight.remove(original.id) }
        var item = original
        do {
            for attempt in 0..<10 {
                let accessToken = try await auth.accessToken(api: api)
                let status = try await api.uploadSession(id: sessionID, accessToken: accessToken)
                switch status.state {
                case "available":
                    cancelVerificationRetry(for: item.id)
                    let finalization = AvailableUploadFinalizer.finalize(
                        item,
                        persist: { try AppGroupQueue.save($0) },
                        cleanup: { try AppGroupQueue.removeCompleted($0) }
                    )
                    switch finalization {
                    case .cleaned(let availableItem):
                        item = availableItem
                    case .persistencePending(let availableItem, let error):
                        item = availableItem
                        lastError = "Upload verified by the server, but its local queue state could not be saved; reconciliation will retry: \(error.localizedDescription)"
                    case .cleanupPending(let availableItem, let error):
                        item = availableItem
                        lastError = "Upload verified, but local queue cleanup will retry: \(error.localizedDescription)"
                    }
                    UploadDiagnostics.shared.record("server_available", context: [
                        "queue_id": item.id.uuidString,
                        "session_id": sessionID,
                    ])
                    reload()
                    return
                case "quarantined":
                    cancelVerificationRetry(for: item.id)
                    item.state = .quarantined
                    item.lastError = "The server rejected this file because its final SHA-256 or byte count did not match. Keep the source copy."
                    try AppGroupQueue.save(item)
                    UploadDiagnostics.shared.record("server_quarantined", context: [
                        "queue_id": item.id.uuidString,
                        "session_id": sessionID,
                    ])
                    reload()
                    return
                case "created":
                    cancelVerificationRetry(for: item.id)
                    item.tusUploadID = nil
                    item.state = .queued
                    try AppGroupQueue.save(item)
                    await recoverTransfer(item)
                    return
                case "uploading":
                    cancelVerificationRetry(for: item.id)
                    if item.tusUploadID == nil {
                        await recoverTransfer(item)
                        return
                    }
                    item.state = .transferring
                    item.lastError = nil
                    try AppGroupQueue.save(item)
                    reload()
                    return
                case "failed":
                    cancelVerificationRetry(for: item.id)
                    item.state = .failed
                    item.lastError = "The server marked this upload as failed. Create a new upload from the original file."
                    try AppGroupQueue.save(item)
                    UploadDiagnostics.shared.record("server_failed", context: [
                        "queue_id": item.id.uuidString,
                        "session_id": sessionID,
                    ])
                    reload()
                    return
                default:
                    item.state = .verifying
                    try AppGroupQueue.save(item)
                    if attempt < 9 {
                        try await Task.sleep(for: .seconds(2))
                    }
                }
            }
            item.state = .verifying
            item.lastError = "Server verification is still in progress. The app will check again automatically."
            try AppGroupQueue.save(item)
            scheduleVerificationRetry(item)
        } catch {
            // A failed status poll is not proof that the upload failed. Preserve
            // the durable verifying state and retry on a backoff, foreground,
            // or network-restoration signal. Only an explicit server `failed`
            // state is terminal.
            item.state = .verifying
            item.lastError = "Status check will retry automatically: \(error.localizedDescription)"
            try? AppGroupQueue.save(item)
            scheduleVerificationRetry(item)
        }
        reload()
    }

    private func reconcileVerifyingUploads(trigger: String) async {
        reload()
        let pending = uploads.filter { $0.state == .verifying && $0.serverSessionID != nil }
        guard !pending.isEmpty else { return }
        UploadDiagnostics.shared.record("verification_reconcile_triggered", context: ["trigger": trigger])
        for item in pending {
            cancelVerificationRetry(for: item.id)
            await reconcile(item)
        }
    }

    private func scheduleVerificationRetry(_ item: QueuedUpload) {
        guard verificationRetryTasks[item.id] == nil else { return }
        let previous = verificationRetryCounts[item.id, default: 0]
        let exponent = min(previous, 5)
        let delaySeconds = min(30 * (1 << exponent), 15 * 60)
        verificationRetryCounts[item.id] = previous + 1
        UploadDiagnostics.shared.record("verification_retry_scheduled", context: [
            "queue_id": item.id.uuidString,
            "session_id": item.serverSessionID ?? "",
        ])
        verificationRetryTasks[item.id] = Task { [weak self] in
            try? await Task.sleep(for: .seconds(delaySeconds))
            guard !Task.isCancelled, let self else { return }
            self.verificationRetryTasks[item.id] = nil
            guard let current = try? AppGroupQueue.all().first(where: { $0.id == item.id }),
                  current.state == .verifying else {
                self.verificationRetryCounts[item.id] = nil
                return
            }
            await self.reconcile(current)
        }
    }

    private func cancelVerificationRetry(for id: UUID) {
        verificationRetryTasks[id]?.cancel()
        verificationRetryTasks[id] = nil
        verificationRetryCounts[id] = nil
    }

    private func tusFailed(context: [String: String]?, error: Error) async {
        guard let queueID = context?["queue_id"], let id = UUID(uuidString: queueID), var item = try? AppGroupQueue.all().first(where: { $0.id == id }) else { return }
        item.state = .failed
        item.lastError = error.localizedDescription
        try? AppGroupQueue.save(item)
        reload()
    }

    private func mediaType(for item: QueuedUpload) -> String {
        if let mimeType = item.contentType.preferredMIMEType {
            return mimeType
        }
        return item.contentType.conforms(to: .image) ? "image/*" : "video/*"
    }

    private nonisolated static func requireUploadToken(_ session: UploadSession) throws -> String {
        guard let token = session.uploadToken, !token.isEmpty else {
            throw APIProblem(status: 410, code: "upload_session_expired", detail: "The upload session expired. Create a new upload from the original file.")
        }
        return token
    }
}
