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
