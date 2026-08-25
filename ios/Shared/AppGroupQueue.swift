import Foundation
import UniformTypeIdentifiers


struct QuarantinedQueueRecord: Identifiable, Hashable, Sendable {
    let id: String
    let filename: String
    let modifiedAt: Date?
}


struct QueueRecoveryHooks {
    let afterPayloadMove: () throws -> Void
    let afterRecordWrite: () throws -> Void
    let beforeSourceRecordRemoval: () throws -> Void

    init(
        afterPayloadMove: @escaping () throws -> Void = {},
        afterRecordWrite: @escaping () throws -> Void = {},
        beforeSourceRecordRemoval: @escaping () throws -> Void = {}
    ) {
        self.afterPayloadMove = afterPayloadMove
        self.afterRecordWrite = afterRecordWrite
        self.beforeSourceRecordRemoval = beforeSourceRecordRemoval
    }
}

enum AppGroupQueueError: LocalizedError {
    case appGroupUnavailable
    case unsupportedPayload
    case cleanupRequiresAvailable

    var errorDescription: String? {
        switch self {
        case .appGroupUnavailable:
            return "The shared app storage is unavailable. Check the App Group entitlement."
        case .unsupportedPayload:
            return "The shared item is not an image or video file."
        case .cleanupRequiresAvailable:
            return "An upload can only be removed after server verification is complete."
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
        try all(in: rootURL())
    }

    static func all(in root: URL) throws -> [QueuedUpload] {
        let records = root.appending(path: "records", directoryHint: .isDirectory)
        guard FileManager.default.fileExists(atPath: records.path()) else { return [] }
        let files = try FileManager.default.contentsOfDirectory(
            at: records,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )
        .filter { $0.pathExtension == "json" }
        var items: [QueuedUpload] = []
        for file in files {
            do {
                items.append(try JSONDecoder.photoCloud.decode(QueuedUpload.self, from: Data(contentsOf: file)))
            } catch {
                // Preserve the original payload; isolate only malformed queue
                // metadata so one record cannot stop every healthy upload.
                let quarantine = records.appending(path: "quarantine", directoryHint: .isDirectory)
                try? createProtectedDirectory(quarantine)
                try? FileManager.default.moveItem(at: file, to: quarantine.appending(path: file.lastPathComponent + ".corrupt"))
            }
        }
        return items.sorted { $0.createdAt < $1.createdAt }
    }

    static func save(_ item: QueuedUpload) throws {
        let records = try rootURL().appending(path: "records", directoryHint: .isDirectory)
        try createProtectedDirectory(records)
        try save(item, in: records)
    }

    static func payloadURL(for item: QueuedUpload) throws -> URL {
        try payloadURL(for: item, in: rootURL())
    }

    static func payloadURL(for item: QueuedUpload, in root: URL) throws -> URL {
        guard item.payloadFilename.range(of: #"^[0-9A-Fa-f-]+\.[A-Za-z0-9]+$"#, options: .regularExpression) != nil else {
            throw AppGroupQueueError.unsupportedPayload
        }
        return root
            .appending(path: "payloads", directoryHint: .isDirectory)
            .appending(path: item.payloadFilename)
    }

    static func cleanupCompleted() throws {
        try cleanupCompleted(in: rootURL())
    }

    static func cleanupCompleted(in root: URL) throws {
        for item in try all(in: root) where item.state == .available {
            try removeCompleted(item, in: root)
        }
    }

    static func removeCompleted(_ item: QueuedUpload) throws {
        try removeCompleted(item, in: rootURL())
    }

    static func removeCompleted(_ item: QueuedUpload, in root: URL) throws {
        guard item.state == .available else {
            throw AppGroupQueueError.cleanupRequiresAvailable
        }
        let payload = try payloadURL(for: item, in: root)
        try removeIfPresent(payload)
        try removeIfPresent(recordURL(for: item.id, in: root))
    }

    static func quarantinedRecords() throws -> [QuarantinedQueueRecord] {
        try quarantinedRecords(in: rootURL())
    }

    static func quarantinedRecords(in root: URL) throws -> [QuarantinedQueueRecord] {
        let quarantine = root
            .appending(path: "records", directoryHint: .isDirectory)
            .appending(path: "quarantine", directoryHint: .isDirectory)
        guard FileManager.default.fileExists(atPath: quarantine.path()) else { return [] }
        return try FileManager.default.contentsOfDirectory(
            at: quarantine,
            includingPropertiesForKeys: [.contentModificationDateKey],
            options: [.skipsHiddenFiles]
        )
        .filter { $0.lastPathComponent.hasSuffix(".json.corrupt") }
        .map { url in
            let values = try? url.resourceValues(forKeys: [.contentModificationDateKey])
            return QuarantinedQueueRecord(
                id: url.lastPathComponent,
                filename: url.lastPathComponent,
                modifiedAt: values?.contentModificationDate
            )
        }
        .sorted { ($0.modifiedAt ?? .distantPast) < ($1.modifiedAt ?? .distantPast) }
    }

    /// Rebuilds a corrupt queue record as a brand-new upload identity while
    /// preserving the payload bytes. A fresh UUID avoids colliding with any
    /// server session whose immutable metadata was lost with the corrupt JSON.
    static func recoverQuarantinedRecord(_ record: QuarantinedQueueRecord) throws -> QueuedUpload {
        try recoverQuarantinedRecord(record, in: rootURL())
    }

    static func recoverQuarantinedRecord(_ record: QuarantinedQueueRecord, in root: URL) throws -> QueuedUpload {
        try recoverQuarantinedRecord(record, in: root, hooks: QueueRecoveryHooks())
    }

    static func recoverQuarantinedRecord(
        _ record: QuarantinedQueueRecord,
        in root: URL,
        hooks: QueueRecoveryHooks
    ) throws -> QueuedUpload {
        let quarantine = root
            .appending(path: "records", directoryHint: .isDirectory)
            .appending(path: "quarantine", directoryHint: .isDirectory)
        let sourceRecord = quarantine.appending(path: record.filename)
        guard FileManager.default.fileExists(atPath: sourceRecord.path()) else {
            throw CocoaError(.fileNoSuchFile)
        }

        let originalIDText = record.filename.replacingOccurrences(of: ".json.corrupt", with: "")
        guard let originalID = UUID(uuidString: originalIDText) else {
            throw AppGroupQueueError.unsupportedPayload
        }
        let payloads = root.appending(path: "payloads", directoryHint: .isDirectory)
        let candidates = try FileManager.default.contentsOfDirectory(
            at: payloads,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        ).filter { $0.deletingPathExtension().lastPathComponent.caseInsensitiveCompare(originalID.uuidString) == .orderedSame }
        guard candidates.count == 1 else {
            throw AppGroupQueueError.unsupportedPayload
        }
        let oldPayload = candidates[0]
        let type = UTType(filenameExtension: oldPayload.pathExtension) ?? .data
        guard type.conforms(to: .image) || type.conforms(to: .movie) else {
            throw AppGroupQueueError.unsupportedPayload
        }

        let newID = UUID()
        let newPayload = payloads.appending(path: "\(newID.uuidString).\(oldPayload.pathExtension.lowercased())")
        let newRecord = recordURL(for: newID, in: root)
        try FileManager.default.moveItem(at: oldPayload, to: newPayload)
        do {
            try hooks.afterPayloadMove()
            let item = QueuedUpload(
                id: newID,
                payloadFilename: newPayload.lastPathComponent,
                originalFilename: "Recovered-\(oldPayload.lastPathComponent)",
                typeIdentifier: type.identifier,
                createdAt: .now,
                state: .queued,
                byteCount: nil,
                sha256: nil,
                serverSessionID: nil,
                tusUploadID: nil,
                lastError: "Recovered from isolated corrupt queue metadata; upload will restart from byte zero."
            )
            let records = root.appending(path: "records", directoryHint: .isDirectory)
            try save(item, in: records, afterWrite: hooks.afterRecordWrite)
            try hooks.beforeSourceRecordRemoval()
            try FileManager.default.removeItem(at: sourceRecord)
            return item
        } catch {
            // save() writes JSON atomically before applying file protection, so
            // it may throw after the new record already exists. Always remove
            // the destination record rather than relying on a "save returned"
            // flag, then restore the payload only when doing so cannot overwrite
            // another file.
            try? removeIfPresent(newRecord)
            if FileManager.default.fileExists(atPath: newPayload.path())
                && !FileManager.default.fileExists(atPath: oldPayload.path()) {
                try? FileManager.default.moveItem(at: newPayload, to: oldPayload)
            }
            throw error
        }
    }

    static func diagnosticsDirectory() throws -> URL {
        let directory = try rootURL().appending(path: "diagnostics", directoryHint: .isDirectory)
        try createProtectedDirectory(directory)
        return directory
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

    private static func save(
        _ item: QueuedUpload,
        in records: URL,
        afterWrite: () throws -> Void = {}
    ) throws {
        let destination = records.appending(path: "\(item.id.uuidString).json")
        try JSONEncoder.photoCloud.encode(item).write(to: destination, options: [.atomic])
        try afterWrite()
        try FileManager.default.setAttributes([.protectionKey: backgroundProtection], ofItemAtPath: destination.path())
    }

    private static func recordURL(for id: UUID, in root: URL) -> URL {
        root
            .appending(path: "records", directoryHint: .isDirectory)
            .appending(path: "\(id.uuidString).json")
    }

    private static func removeIfPresent(_ url: URL) throws {
        guard FileManager.default.fileExists(atPath: url.path()) else { return }
        try FileManager.default.removeItem(at: url)
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
