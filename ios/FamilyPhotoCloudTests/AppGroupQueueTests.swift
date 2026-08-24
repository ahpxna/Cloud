import Foundation
import UniformTypeIdentifiers
import XCTest
@testable import FamilyPhotoCloud

final class AppGroupQueueTests: XCTestCase {
    func testCleanupRemovesOnlyServerAvailableItems() throws {
        let root = temporaryQueueRoot()
        let available = queuedUpload(id: UUID(), state: .available)
        let quarantined = queuedUpload(id: UUID(), state: .quarantined)
        try write(available, in: root)
        try write(quarantined, in: root)

        try AppGroupQueue.cleanupCompleted(in: root)

        XCTAssertFalse(FileManager.default.fileExists(atPath: payloadURL(for: available, in: root).path()))
        XCTAssertFalse(FileManager.default.fileExists(atPath: recordURL(for: available, in: root).path()))
        XCTAssertTrue(FileManager.default.fileExists(atPath: payloadURL(for: quarantined, in: root).path()))
        XCTAssertTrue(FileManager.default.fileExists(atPath: recordURL(for: quarantined, in: root).path()))
    }

    func testCleanupRefusesUnverifiedPayload() throws {
        let root = temporaryQueueRoot()
        let failed = queuedUpload(id: UUID(), state: .failed)
        try write(failed, in: root)

        XCTAssertThrowsError(try AppGroupQueue.removeCompleted(failed, in: root))
        XCTAssertTrue(FileManager.default.fileExists(atPath: payloadURL(for: failed, in: root).path()))
        XCTAssertTrue(FileManager.default.fileExists(atPath: recordURL(for: failed, in: root).path()))
    }

    func testCleanupIsIdempotentAfterInterruptedDelete() throws {
        let root = temporaryQueueRoot()
        let available = queuedUpload(id: UUID(), state: .available)
        try write(available, in: root)
        try FileManager.default.removeItem(at: payloadURL(for: available, in: root))

        try AppGroupQueue.cleanupCompleted(in: root)
        try AppGroupQueue.cleanupCompleted(in: root)

        XCTAssertFalse(FileManager.default.fileExists(atPath: recordURL(for: available, in: root).path()))
    }


    func testCorruptRecordIsIsolatedAndCanBeRecoveredAsFreshIdentity() throws {
        let root = temporaryQueueRoot()
        let original = queuedUpload(id: UUID(), state: .queued)
        try write(original, in: root)
        try Data("{not-json".utf8).write(to: recordURL(for: original, in: root), options: .atomic)

        XCTAssertTrue(try AppGroupQueue.all(in: root).isEmpty)
        let quarantined = try AppGroupQueue.quarantinedRecords(in: root)
        XCTAssertEqual(quarantined.count, 1)
        XCTAssertTrue(FileManager.default.fileExists(atPath: payloadURL(for: original, in: root).path()))

        let recovered = try AppGroupQueue.recoverQuarantinedRecord(quarantined[0], in: root)

        XCTAssertNotEqual(recovered.id, original.id)
        XCTAssertNil(recovered.serverSessionID)
        XCTAssertNil(recovered.tusUploadID)
        XCTAssertEqual(recovered.state, .queued)
        XCTAssertFalse(FileManager.default.fileExists(atPath: payloadURL(for: original, in: root).path()))
        XCTAssertTrue(FileManager.default.fileExists(atPath: payloadURL(for: recovered, in: root).path()))
        XCTAssertTrue(try AppGroupQueue.quarantinedRecords(in: root).isEmpty)
        XCTAssertEqual(try AppGroupQueue.all(in: root).map(\.id), [recovered.id])
    }

    func testRecoverQuarantinedRecordRefusesAmbiguousPayloadsWithoutDeletingBytes() throws {
        let root = temporaryQueueRoot()
        let original = queuedUpload(id: UUID(), state: .queued)
        try write(original, in: root)
        let records = root.appending(path: "records", directoryHint: .isDirectory)
        let quarantine = records.appending(path: "quarantine", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: quarantine, withIntermediateDirectories: true)
        let corrupt = quarantine.appending(path: "\(original.id.uuidString).json.corrupt")
        try Data("broken".utf8).write(to: corrupt)
        try FileManager.default.removeItem(at: recordURL(for: original, in: root))
        let duplicate = root.appending(path: "payloads", directoryHint: .isDirectory)
            .appending(path: "\(original.id.uuidString).jpg")
        try Data("duplicate".utf8).write(to: duplicate)
        let record = QuarantinedQueueRecord(id: corrupt.lastPathComponent, filename: corrupt.lastPathComponent, modifiedAt: nil)

        XCTAssertThrowsError(try AppGroupQueue.recoverQuarantinedRecord(record, in: root))
        XCTAssertTrue(FileManager.default.fileExists(atPath: payloadURL(for: original, in: root).path()))
        XCTAssertTrue(FileManager.default.fileExists(atPath: duplicate.path()))
        XCTAssertTrue(FileManager.default.fileExists(atPath: corrupt.path()))
    }

    private func temporaryQueueRoot() -> URL {
        let root = FileManager.default.temporaryDirectory
            .appending(path: "FamilyPhotoCloudTests-\(UUID().uuidString)", directoryHint: .isDirectory)
        addTeardownBlock { try? FileManager.default.removeItem(at: root) }
        return root
    }

    private func queuedUpload(id: UUID, state: UploadState) -> QueuedUpload {
        QueuedUpload(
            id: id,
            payloadFilename: "\(id.uuidString).heic",
            originalFilename: "IMG.heic",
            typeIdentifier: UTType.heic.identifier,
            createdAt: Date(timeIntervalSince1970: 1),
            state: state,
            byteCount: 1,
            sha256: String(repeating: "0", count: 64),
            serverSessionID: UUID().uuidString,
            tusUploadID: UUID(),
            lastError: nil
        )
    }

    private func write(_ item: QueuedUpload, in root: URL) throws {
        let records = root.appending(path: "records", directoryHint: .isDirectory)
        let payloads = root.appending(path: "payloads", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: records, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: payloads, withIntermediateDirectories: true)
        try Data("payload".utf8).write(to: payloadURL(for: item, in: root))
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        try encoder.encode(item).write(to: recordURL(for: item, in: root))
    }

    private func payloadURL(for item: QueuedUpload, in root: URL) -> URL {
        root.appending(path: "payloads", directoryHint: .isDirectory)
            .appending(path: item.payloadFilename)
    }

    private func recordURL(for item: QueuedUpload, in root: URL) -> URL {
        root.appending(path: "records", directoryHint: .isDirectory)
            .appending(path: "\(item.id.uuidString).json")
    }
}
