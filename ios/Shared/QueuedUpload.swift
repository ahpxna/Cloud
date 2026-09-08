import Foundation
import UniformTypeIdentifiers

enum UploadState: String, Codable, Sendable {
    case queued
    case transferring
    case verifying
    case available
    case quarantined
    case failed
}

/// Contains no bearer credential. Every field is safe to persist in the App
/// Group because the Share Extension can read the same directory.
struct QueuedUpload: Codable, Identifiable, Sendable {
    /// Stable local identity. For canonical records this UUID is also the
    /// basename of payloadFilename and the record filename.
    let id: UUID
    /// Independent server idempotency identity. Legacy records decode this as
    /// `id`; recovery can mint a new server identity without renaming bytes.
    let clientAssetID: UUID
    let payloadFilename: String
    let originalFilename: String
    let typeIdentifier: String
    let createdAt: Date
    var ownerUserID: String?
    var state: UploadState
    var byteCount: Int64?
    var sha256: String?
    var serverSessionID: String?
    var tusUploadID: UUID?
    var lastError: String?

    private enum CodingKeys: String, CodingKey {
        case id, clientAssetID, payloadFilename, originalFilename, typeIdentifier, createdAt
        case ownerUserID, state, byteCount, sha256, serverSessionID, tusUploadID, lastError
    }

    init(
        id: UUID,
        clientAssetID: UUID? = nil,
        payloadFilename: String,
        originalFilename: String,
        typeIdentifier: String,
        createdAt: Date,
        state: UploadState,
        byteCount: Int64?,
        sha256: String?,
        serverSessionID: String?,
        tusUploadID: UUID?,
        lastError: String?,
        ownerUserID: String? = nil
    ) {
        self.id = id
        self.clientAssetID = clientAssetID ?? id
        self.payloadFilename = payloadFilename
        self.originalFilename = originalFilename
        self.typeIdentifier = typeIdentifier
        self.createdAt = createdAt
        self.ownerUserID = ownerUserID
        self.state = state
        self.byteCount = byteCount
        self.sha256 = sha256
        self.serverSessionID = serverSessionID
        self.tusUploadID = tusUploadID
        self.lastError = lastError
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(UUID.self, forKey: .id)
        clientAssetID = try container.decodeIfPresent(UUID.self, forKey: .clientAssetID) ?? id
        payloadFilename = try container.decode(String.self, forKey: .payloadFilename)
        originalFilename = try container.decode(String.self, forKey: .originalFilename)
        typeIdentifier = try container.decode(String.self, forKey: .typeIdentifier)
        createdAt = try container.decode(Date.self, forKey: .createdAt)
        ownerUserID = try container.decodeIfPresent(String.self, forKey: .ownerUserID)
        state = try container.decode(UploadState.self, forKey: .state)
        byteCount = try container.decodeIfPresent(Int64.self, forKey: .byteCount)
        sha256 = try container.decodeIfPresent(String.self, forKey: .sha256)
        serverSessionID = try container.decodeIfPresent(String.self, forKey: .serverSessionID)
        tusUploadID = try container.decodeIfPresent(UUID.self, forKey: .tusUploadID)
        lastError = try container.decodeIfPresent(String.self, forKey: .lastError)
    }

    mutating func claim(for userID: String) throws {
        guard !userID.isEmpty, ownerUserID == nil || ownerUserID == userID else {
            throw QueueOwnershipError.differentAccount
        }
        ownerUserID = userID
    }

    func acceptsTransferCallback(id: UUID, sessionID: String?) -> Bool {
        guard state != .available, state != .quarantined,
              let serverSessionID, let sessionID else { return false }
        return tusUploadID == id && serverSessionID == sessionID
    }

    var contentType: UTType {
        UTType(typeIdentifier) ?? .data
    }
}

enum QueueOwnershipError: LocalizedError {
    case differentAccount
    var errorDescription: String? { "Sign in with the account that first started this upload." }
}
