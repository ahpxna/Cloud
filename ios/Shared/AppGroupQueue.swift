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
    let beforeRollbackRecordRemoval: () throws -> Void

    init(
        afterPayloadMove: @escaping () throws -> Void = {},
        afterRecordWrite: @escaping () throws -> Void = {},
        beforeSourceRecordRemoval: @escaping () throws -> Void = {},
        beforeRollbackRecordRemoval: @escaping () throws -> Void = {}
    ) {
        self.afterPayloadMove = afterPayloadMove
        self.afterRecordWrite = afterRecordWrite
        self.beforeSourceRecordRemoval = beforeSourceRecordRemoval
        self.beforeRollbackRecordRemoval = beforeRollbackRecordRemoval
    }
}

enum AppGroupQueueError: LocalizedError {
    case appGroupUnavailable
    case unsupportedPayload
    case cleanupRequiresAvailable
    case queueIdentityCollision

    var errorDescription: String? {
        switch self {
        case .appGroupUnavailable:
            return "The shared app storage is unavailable. Check the App Group entitlement."
        case .unsupportedPayload:
            return "The shared item is not an image or video file."
        case .cleanupRequiresAvailable:
            return "An upload can only be removed after server verification is complete."
        case .queueIdentityCollision:
            return "Queue metadata conflicts with an existing payload identity. The original bytes were left untouched."
        }
    }
}

/// A file-per-item queue avoids a shared mutable index between the extension
/// and main application. The extension creates one new item; the main app owns
/// state transitions for that item afterwards.
enum AppGroupQueue {
    static let appGroupIdentifier = "group.dev.phanan.familyphotocloud"
    private static let backgroundProtection = FileProtectionType.completeUntilFirstUserAuthentication
    private static let staleTemporaryAge: TimeInterval = 24 * 60 * 60
    // The Share Extension publishes payload bytes before its queue record. Do
    // not treat a just-created payload as a crash orphan while that extension
    // is still committing the record in another process.
    private static let orphanRecoveryMinimumAge: TimeInterval = 5 * 60

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
        try createProtectedDirectory(records)
        try removeStaleTemporaryFiles(in: root, records: records)
        let files = try FileManager.default.contentsOfDirectory(
            at: records,
            includingPropertiesForKeys: nil,
            options: [.skipsHiddenFiles]
        )
        .filter { $0.pathExtension == "json" }
        var items: [QueuedUpload] = []
        for file in files {
            do {
                let decoded = try JSONDecoder.photoCloud.decode(QueuedUpload.self, from: Data(contentsOf: file))
                let item = try normalizeLegacyIdentity(decoded, recordURL: file, records: records)
                if !items.contains(where: { $0.id == item.id }) {
                    items.append(item)
                }
            } catch {
                // Preserve the original payload; isolate only malformed queue
                // metadata so one record cannot stop every healthy upload.
                let quarantine = records.appending(path: "quarantine", directoryHint: .isDirectory)
                try? createProtectedDirectory(quarantine)
                let destination = quarantine.appending(path: file.lastPathComponent + ".corrupt")
                if !FileManager.default.fileExists(atPath: destination.path()) {
                    try? FileManager.default.moveItem(at: file, to: destination)
                }
            }
        }
        try recoverOrphanedPayloads(in: root, records: records, existing: &items)
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

    /// Rebuilds corrupt queue metadata without renaming the durable payload.
    /// The local queue UUID stays canonical to the payload basename while a
    /// fresh clientAssetID prevents collision with any lost server session.
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

        let records = root.appending(path: "records", directoryHint: .isDirectory)
        if let existing = try existingRecordClaiming(payloadFilename: oldPayload.lastPathComponent, in: records) {
            // A previous recovery committed the valid record but was killed
            // before it could remove the corrupt source. Do not mint a second
            // queue identity for the same bytes.
            try? FileManager.default.removeItem(at: sourceRecord)
            return existing
        }

        // The local identity is always the payload basename. A fresh
        // clientAssetID provides a new server idempotency identity without
        // renaming durable bytes or breaking future quarantine recovery.
        let localID = originalID
        let canonicalRecord = recordURL(for: localID, in: root)
        if FileManager.default.fileExists(atPath: canonicalRecord.path()) {
            if let existing = try? JSONDecoder.photoCloud.decode(QueuedUpload.self, from: Data(contentsOf: canonicalRecord)),
               existing.payloadFilename == oldPayload.lastPathComponent {
                try? FileManager.default.removeItem(at: sourceRecord)
                return existing
            }
            throw AppGroupQueueError.queueIdentityCollision
        }
        do {
            try hooks.afterPayloadMove()
            let item = QueuedUpload(
                id: localID,
                clientAssetID: UUID(),
                payloadFilename: oldPayload.lastPathComponent,
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
            try save(item, in: records, afterWrite: hooks.afterRecordWrite)
            try hooks.beforeSourceRecordRemoval()
            try FileManager.default.removeItem(at: sourceRecord)
            return item
        } catch {
            do {
                try hooks.beforeRollbackRecordRemoval()
                try removeIfPresent(canonicalRecord)
            } catch {}
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
        let temporary = records.appending(path: ".\(item.id.uuidString).\(UUID().uuidString).json.tmp")
        do {
            let encoded = try JSONEncoder.photoCloud.encode(item)
            try encoded.write(to: temporary)
            try afterWrite()
            // Apply file protection before publishing the record. A protection
            // failure must not leave a newly-written destination that callers
            // mistake for an unsuccessful save.
            try FileManager.default.setAttributes(
                [.protectionKey: backgroundProtection],
                ofItemAtPath: temporary.path()
            )
            if FileManager.default.fileExists(atPath: destination.path()) {
                _ = try FileManager.default.replaceItemAt(
                    destination,
                    withItemAt: temporary,
                    backupItemName: nil,
                    options: []
                )
            } else {
                try FileManager.default.moveItem(at: temporary, to: destination)
            }
        } catch {
            try? removeIfPresent(temporary)
            throw error
        }
    }

    private static func recordURL(for id: UUID, in root: URL) -> URL {
        root
            .appending(path: "records", directoryHint: .isDirectory)
            .appending(path: "\(id.uuidString).json")
    }

    /// A process can die after publishing bytes but before the record's atomic
    /// rename. Rebuild a fresh record at the next launch without moving the
    /// payload. Quarantined corrupt records reserve their original filename so
    /// the explicit recovery UI remains authoritative for those bytes.
    private static func recoverOrphanedPayloads(
        in root: URL,
        records: URL,
        existing: inout [QueuedUpload]
    ) throws {
        let payloads = root.appending(path: "payloads", directoryHint: .isDirectory)
        guard FileManager.default.fileExists(atPath: payloads.path()) else { return }
        let claimed = Set(existing.map(\.payloadFilename))
        let orphanCutoff = Date().addingTimeInterval(-orphanRecoveryMinimumAge)
        let quarantine = records.appending(path: "quarantine", directoryHint: .isDirectory)
        let quarantinedRecords = (try? FileManager.default.contentsOfDirectory(at: quarantine, includingPropertiesForKeys: nil, options: [.skipsHiddenFiles])) ?? []
        let quarantinedPayloadIDs = Set(quarantinedRecords.compactMap { file -> UUID? in
            let name = file.lastPathComponent.replacingOccurrences(of: ".json.corrupt", with: "")
            return UUID(uuidString: name)
        })
        for payload in try FileManager.default.contentsOfDirectory(
            at: payloads,
            includingPropertiesForKeys: [.contentModificationDateKey],
            options: [.skipsHiddenFiles]
        ) {
            let modifiedAt = try payload.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate ?? .now
            guard !claimed.contains(payload.lastPathComponent),
                  modifiedAt <= orphanCutoff,
                  let oldID = UUID(uuidString: payload.deletingPathExtension().lastPathComponent),
                  !quarantinedPayloadIDs.contains(oldID),
                  let type = UTType(filenameExtension: payload.pathExtension),
                  type.conforms(to: .image) || type.conforms(to: .movie) else { continue }
            try FileManager.default.setAttributes([.protectionKey: backgroundProtection], ofItemAtPath: payload.path())
            let item = QueuedUpload(
                id: oldID,
                clientAssetID: UUID(),
                payloadFilename: payload.lastPathComponent,
                originalFilename: "Recovered-\(payload.lastPathComponent)",
                typeIdentifier: type.identifier,
                createdAt: .now,
                state: .queued,
                byteCount: nil,
                sha256: nil,
                serverSessionID: nil,
                tusUploadID: nil,
                lastError: "Recovered payload bytes after an interrupted queue write; upload will restart from byte zero."
            )
            try save(item, in: records)
            existing.append(item)
        }
    }

    /// V18 recovery could publish B.json pointing at A.heic. Canonicalize that
    /// legacy state on read by publishing A.json first and only then retiring
    /// B.json. The old queue UUID is preserved as clientAssetID so an existing
    /// server session remains idempotent.
    private static func normalizeLegacyIdentity(
        _ item: QueuedUpload,
        recordURL source: URL,
        records: URL
    ) throws -> QueuedUpload {
        guard let payloadID = UUID(uuidString: URL(fileURLWithPath: item.payloadFilename).deletingPathExtension().lastPathComponent) else {
            throw AppGroupQueueError.unsupportedPayload
        }
        guard payloadID != item.id else { return item }

        let normalized = QueuedUpload(
            id: payloadID,
            clientAssetID: item.clientAssetID,
            payloadFilename: item.payloadFilename,
            originalFilename: item.originalFilename,
            typeIdentifier: item.typeIdentifier,
            createdAt: item.createdAt,
            state: item.state,
            byteCount: item.byteCount,
            sha256: item.sha256,
            serverSessionID: item.serverSessionID,
            tusUploadID: item.tusUploadID,
            lastError: item.lastError
        )
        let destination = records.appending(path: "\(payloadID.uuidString).json")
        if FileManager.default.fileExists(atPath: destination.path()) {
            let existing = try JSONDecoder.photoCloud.decode(QueuedUpload.self, from: Data(contentsOf: destination))
            guard existing.payloadFilename == item.payloadFilename else {
                throw AppGroupQueueError.queueIdentityCollision
            }
            if source != destination { try removeIfPresent(source) }
            return existing
        }
        try save(normalized, in: records)
        if source != destination { try removeIfPresent(source) }
        return normalized
    }

    private static func existingRecordClaiming(payloadFilename: String, in records: URL) throws -> QueuedUpload? {
        guard FileManager.default.fileExists(atPath: records.path()) else { return nil }
        for record in try FileManager.default.contentsOfDirectory(at: records, includingPropertiesForKeys: nil, options: [.skipsHiddenFiles]) where record.pathExtension == "json" {
            if let item = try? JSONDecoder.photoCloud.decode(QueuedUpload.self, from: Data(contentsOf: record)), item.payloadFilename == payloadFilename {
                return item
            }
        }
        return nil
    }

    private static func removeStaleTemporaryFiles(in root: URL, records: URL) throws {
        let cutoff = Date().addingTimeInterval(-staleTemporaryAge)
        let payloads = root.appending(path: "payloads", directoryHint: .isDirectory)
        for directory in [payloads, records] where FileManager.default.fileExists(atPath: directory.path()) {
            for file in try FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.contentModificationDateKey], options: []) {
                let name = file.lastPathComponent
                let isImporting = directory == payloads && name.hasPrefix(".") && name.hasSuffix(".importing")
                let isRecordTemp = directory == records && name.hasPrefix(".") && name.hasSuffix(".json.tmp")
                guard isImporting || isRecordTemp else { continue }
                let modified = try file.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate ?? .distantFuture
                if modified < cutoff { try? FileManager.default.removeItem(at: file) }
            }
        }
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
