import CryptoKit
import Foundation
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
        try await Task.detached(priority: .utility) {
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }
            var hasher = SHA256()
            while let data = try handle.read(upToCount: 1 << 20), !data.isEmpty {
                hasher.update(data: data)
            }
            return hasher.finalize().map { String(format: "%02x", $0) }.joined()
        }.value
    }
}

@MainActor
final class UploadCoordinator: ObservableObject {
    @Published private(set) var uploads: [QueuedUpload] = []
    @Published private(set) var lastError: String?

    private let api: PhotoCloudAPI
    private let auth: AuthenticationStore
    private let transport: TUSUploadTransport

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
    }

    func reload() {
        do { try AppGroupQueue.cleanupCompleted() }
        catch { lastError = error.localizedDescription }
        do { uploads = try AppGroupQueue.all() }
        catch { lastError = error.localizedDescription }
    }

    func login(email: String, password: String) async {
        do {
            try await auth.login(email: email, password: password, deviceName: UIDevice.current.name, api: api)
            await startQueuedUploads()
        } catch { lastError = error.localizedDescription }
    }

    func startQueuedUploads() async {
        reload()
        transport.resumeStoredUploads()
        for item in uploads where item.state != .available && item.state != .quarantined {
            if item.serverSessionID == nil {
                await begin(item)
            } else if item.tusUploadID == nil {
                await recoverTransfer(item)
            } else {
                await reconcile(item)
            }
        }
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
                    clientAssetID: original.id.uuidString,
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
            switch session.state {
            case "created":
                try await enqueueTransfer(&item, session: session, accessToken: accessToken)
            case "uploading":
                guard let storedID = transport.storedUploadID(forSessionID: sessionID) else {
                    // TUSKit persists its context before returning from enqueue;
                    // if the process died before then, preserve the record and
                    // surface an explicit recovery error instead of pretending
                    // the upload has resumed.
                    throw APIProblem(status: 409, code: "missing_local_tus_context", detail: "The upload was created on the server before the phone saved its resume state. Reopen the app to retry recovery; keep the original file.")
                }
                item.tusUploadID = storedID
                item.state = .transferring
                item.lastError = nil
                try AppGroupQueue.save(item)
                try transport.resumeStoredUpload(id: storedID)
            default:
                await reconcile(item)
                return
            }
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
        var item = original
        do {
            for attempt in 0..<10 {
                let accessToken = try await auth.accessToken(api: api)
                let status = try await api.uploadSession(id: sessionID, accessToken: accessToken)
                switch status.state {
                case "available":
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
                    reload()
                    return
                case "quarantined":
                    item.state = .quarantined
                    item.lastError = "The server rejected this file because its final SHA-256 or byte count did not match. Keep the source copy."
                    try AppGroupQueue.save(item)
                    reload()
                    return
                case "created":
                    item.tusUploadID = nil
                    item.state = .queued
                    try AppGroupQueue.save(item)
                    await recoverTransfer(item)
                    return
                case "uploading":
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
                    item.state = .failed
                    item.lastError = "The server marked this upload as failed. Create a new upload from the original file."
                    try AppGroupQueue.save(item)
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
        } catch {
            item.state = .failed
            item.lastError = error.localizedDescription
            try? AppGroupQueue.save(item)
        }
        reload()
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
