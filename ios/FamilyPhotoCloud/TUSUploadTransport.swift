import Foundation
import TUSKit

/// TUSKit retains custom headers in its own persistence. Its header is therefore
/// a narrowly scoped upload capability, never the general access token.
final class TUSUploadTransport: NSObject, TUSClientDelegate {
    static let chunkSizeBytes = 32 * 1024 * 1024
    private let headerProvider: ScopedHeaderProvider
    private let client: TUSClient
    var uploadFinished: ((UUID, [String: String]?) -> Void)?
    var uploadFailed: ((UUID, [String: String]?, Error) -> Void)?

    init(api: PhotoCloudAPI, capability: @escaping @Sendable (String) async throws -> String) throws {
        let directory = try AppGroupQueue.tusStorageDirectory()
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)

        headerProvider = ScopedHeaderProvider(capability: capability)
        let configuration = URLSessionConfiguration.background(withIdentifier: "dev.phanan.familyphotocloud.tus")
        configuration.sessionSendsLaunchEvents = true
        configuration.waitsForConnectivity = true
        configuration.isDiscretionary = false
        client = try TUSClient(
            server: try api.absoluteURL(for: "/v1/uploads/"),
            sessionIdentifier: "dev.phanan.familyphotocloud.tus",
            sessionConfiguration: configuration,
            storageDirectory: directory,
            chunkSize: Self.chunkSizeBytes,
            generateHeaders: headerProvider.resolve
        )
        super.init()
        client.delegate = self
        restoreStoredContexts()
    }

    func enqueue(item: QueuedUpload, endpoint: URL, uploadToken: String) throws -> UUID {
        guard let sessionID = item.serverSessionID else { throw URLError(.badURL) }
        let id = try client.uploadFileAt(
            filePath: try AppGroupQueue.payloadURL(for: item),
            uploadURL: endpoint,
            customHeaders: ["Authorization": "Bearer \(uploadToken)"],
            context: ["queue_id": item.id.uuidString, "session_id": sessionID]
        )
        headerProvider.register(id: id, sessionID: sessionID)
        return id
    }

    func resumeStoredUploads() {
        restoreStoredContexts()
        for upload in (try? client.getStoredUploads()) ?? [] {
            try? client.resume(id: upload.id)
        }
    }

    func storedUploadID(forSessionID sessionID: String) -> UUID? {
        (try? client.getStoredUploads())?.first(where: { $0.context?["session_id"] == sessionID })?.id
    }

    func resumeStoredUpload(id: UUID) throws {
        try client.resume(id: id)
    }

    func validateChunkSize(_ serverRecommendation: Int64) throws {
        guard serverRecommendation == Int64(Self.chunkSizeBytes) else {
            throw APIProblem(
                status: 409,
                code: "upload_chunk_size_mismatch",
                detail: "The server upload chunk size does not match this app version. Update the app or server before uploading."
            )
        }
    }

    func registerBackgroundHandler(_ completion: @escaping () -> Void, identifier: String) {
        Task { @MainActor in UploadDiagnostics.shared.record("background_handler_registered") }
        client.registerBackgroundHandler(completion, forSession: identifier)
    }

    func didStartUpload(id: UUID, context: [String: String]?, client: TUSClient) {
        Task { @MainActor in UploadDiagnostics.shared.record("tus_started", context: context, tusUploadID: id) }
    }

    func fileError(error: TUSClientError, client: TUSClient) {
        Task { @MainActor in UploadDiagnostics.shared.record("tus_file_error", error: error) }
    }

    func totalProgress(bytesUploaded: Int, totalBytes: Int, client: TUSClient) {}

    func progressFor(id: UUID, context: [String: String]?, bytesUploaded: Int, totalBytes: Int, client: TUSClient) {
        Task { @MainActor in
            UploadDiagnostics.shared.record(
                "tus_progress",
                context: context,
                tusUploadID: id,
                bytesUploaded: bytesUploaded,
                totalBytes: totalBytes
            )
        }
    }

    func didFinishUpload(id: UUID, url: URL, context: [String: String]?, client: TUSClient) {
        Task { @MainActor in UploadDiagnostics.shared.record("tus_finished", context: context, tusUploadID: id) }
        uploadFinished?(id, context)
    }

    func uploadFailed(id: UUID, error: Error, context: [String: String]?, client: TUSClient) {
        Task { @MainActor in UploadDiagnostics.shared.record("tus_failed", context: context, tusUploadID: id, error: error) }
        uploadFailed?(id, context, error)
    }

    private func restoreStoredContexts() {
        for upload in (try? client.getStoredUploads()) ?? [] {
            if let sessionID = upload.context?["session_id"] {
                headerProvider.register(id: upload.id, sessionID: sessionID)
            }
        }
    }
}

private final class ScopedHeaderProvider: @unchecked Sendable {
    private let lock = NSLock()
    private var sessionIDs: [UUID: String] = [:]
    private let capability: @Sendable (String) async throws -> String

    init(capability: @escaping @Sendable (String) async throws -> String) {
        self.capability = capability
    }

    func register(id: UUID, sessionID: String) {
        lock.lock()
        sessionIDs[id] = sessionID
        lock.unlock()
    }

    func resolve(requestID: UUID, headers: [String: String], completion: @escaping ([String: String]) -> Void) {
        lock.lock()
        let sessionID = sessionIDs[requestID]
        lock.unlock()
        guard let sessionID else { completion(headers); return }
        Task {
            do {
                let token = try await capability(sessionID)
                var fresh = headers
                fresh["Authorization"] = "Bearer \(token)"
                completion(fresh)
            } catch {
                // Keep the last scoped capability; TUSKit will retry after a
                // transient auth/network failure. A general access token never
                // reaches this persisted header path.
                completion(headers)
            }
        }
    }
}
