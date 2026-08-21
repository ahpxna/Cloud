import Foundation
import UniformTypeIdentifiers

enum AppGroupQueueError: LocalizedError {
    case appGroupUnavailable
    case unsupportedPayload

    var errorDescription: String? {
        switch self {
        case .appGroupUnavailable:
            return "The shared app storage is unavailable. Check the App Group entitlement."
        case .unsupportedPayload:
            return "The shared item is not an image or video file."
        }
    }
}

/// A file-per-item queue avoids a shared mutable index between the extension
/// and main application. The extension creates one new item; the main app owns
/// state transitions for that item afterwards.
enum AppGroupQueue {
    static let appGroupIdentifier = "group.dev.phanan.familyphotocloud"
    private static let backgroundProtection = FileProtectionType.completeUntilFirstUserAuthentication

    static func enqueue(ephemeralSource: URL, type: UTType) throws -> QueuedUpload {
        guard type.conforms(to: .image) || type.conforms(to: .movie) else {
            throw AppGroupQueueError.unsupportedPayload
        }
        let root = try rootURL()
        let payloads = root.appending(path: "payloads", directoryHint: .isDirectory)
        let records = root.appending(path: "records", directoryHint: .isDirectory)
        try createProtectedDirectory(payloads)
        try createProtectedDirectory(records)

        let id = UUID()
        let extensionName = ephemeralSource.pathExtension.isEmpty
            ? (type.preferredFilenameExtension ?? "bin")
            : ephemeralSource.pathExtension
        let filename = "\(id.uuidString).\(extensionName.lowercased())"
        let payload = payloads.appending(path: filename)
        let staging = payloads.appending(path: ".\(id.uuidString).importing")

        do {
            try FileManager.default.copyItem(at: ephemeralSource, to: staging)
            try FileManager.default.moveItem(at: staging, to: payload)
            try FileManager.default.setAttributes([.protectionKey: backgroundProtection], ofItemAtPath: payload.path())
            let item = QueuedUpload(
                id: id,
                payloadFilename: filename,
                originalFilename: ephemeralSource.lastPathComponent,
                typeIdentifier: type.identifier,
                createdAt: .now,
                state: .queued,
                byteCount: nil,
                sha256: nil,
                serverSessionID: nil,
                tusUploadID: nil,
                lastError: nil
            )
            try save(item, in: records)
            return item
        } catch {
            try? FileManager.default.removeItem(at: staging)
            try? FileManager.default.removeItem(at: payload)
            throw error
        }
    }

    static func all() throws -> [QueuedUpload] {
        let records = try rootURL().appending(path: "records", directoryHint: .isDirectory)
        guard FileManager.default.fileExists(atPath: records.path()) else { return [] }
        return try FileManager.default.contentsOfDirectory(
            at: records,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )
        .filter { $0.pathExtension == "json" }
        .map { try JSONDecoder.photoCloud.decode(QueuedUpload.self, from: Data(contentsOf: $0)) }
        .sorted { $0.createdAt < $1.createdAt }
    }

    static func save(_ item: QueuedUpload) throws {
        let records = try rootURL().appending(path: "records", directoryHint: .isDirectory)
        try createProtectedDirectory(records)
        try save(item, in: records)
    }

    static func payloadURL(for item: QueuedUpload) throws -> URL {
        guard item.payloadFilename.range(of: #"^[0-9A-Fa-f-]+\.[A-Za-z0-9]+$"#, options: .regularExpression) != nil else {
            throw AppGroupQueueError.unsupportedPayload
        }
        return try rootURL()
            .appending(path: "payloads", directoryHint: .isDirectory)
            .appending(path: item.payloadFilename)
    }

    static func tusStorageDirectory() throws -> URL {
        let directory = try rootURL().appending(path: "tus", directoryHint: .isDirectory)
        try createProtectedDirectory(directory)
        return directory
    }

    private static func rootURL() throws -> URL {
        guard let container = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroupIdentifier) else {
            throw AppGroupQueueError.appGroupUnavailable
        }
        return container.appending(path: "UploadQueue", directoryHint: .isDirectory)
    }

    private static func save(_ item: QueuedUpload, in records: URL) throws {
        let destination = records.appending(path: "\(item.id.uuidString).json")
        try JSONEncoder.photoCloud.encode(item).write(to: destination, options: [.atomic])
        try FileManager.default.setAttributes([.protectionKey: backgroundProtection], ofItemAtPath: destination.path())
    }

    private static func createProtectedDirectory(_ directory: URL) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        try FileManager.default.setAttributes([.protectionKey: backgroundProtection], ofItemAtPath: directory.path())
    }
}

private extension JSONEncoder {
    static var photoCloud: JSONEncoder {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }
}

private extension JSONDecoder {
    static var photoCloud: JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }
}
