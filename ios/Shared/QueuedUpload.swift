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
    let id: UUID
    let payloadFilename: String
    let originalFilename: String
    let typeIdentifier: String
    let createdAt: Date
    var state: UploadState
    var byteCount: Int64?
    var sha256: String?
    var serverSessionID: String?
    var tusUploadID: UUID?
    var lastError: String?

    var contentType: UTType {
        UTType(typeIdentifier) ?? .data
    }
}
