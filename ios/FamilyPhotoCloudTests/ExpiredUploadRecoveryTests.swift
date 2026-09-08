import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class ExpiredUploadRecoveryTests: XCTestCase {
    func testRetiresOldTransferBeforeSavingFreshSessionAndPreservesOriginalIdentity() throws {
        let original = queuedUpload()
        var operations: [String] = []
        let result = try ExpiredUploadRecovery.reset(original, discardTransfer: { item in
            XCTAssertEqual(item.serverSessionID, original.serverSessionID)
            XCTAssertEqual(item.tusUploadID, original.tusUploadID)
            operations.append("discard")
        }, persist: { item in
            XCTAssertNil(item.serverSessionID)
            XCTAssertNil(item.tusUploadID)
            operations.append("persist")
        })
        XCTAssertEqual(operations, ["discard", "persist"])
        XCTAssertEqual(result.id, original.id)
        XCTAssertEqual(result.clientAssetID, original.clientAssetID)
        XCTAssertEqual(result.payloadFilename, original.payloadFilename)
        XCTAssertEqual(result.sha256, original.sha256)
        XCTAssertEqual(result.byteCount, original.byteCount)
        XCTAssertEqual(result.state, .queued)
        XCTAssertNil(result.lastError)
    }

    func testCancellationFailureNeverPublishesNewSession() {
        var persisted = false
        XCTAssertThrowsError(try ExpiredUploadRecovery.reset(queuedUpload(), discardTransfer: { _ in
            throw Failure.expected
        }, persist: { _ in persisted = true }))
        XCTAssertFalse(persisted)
    }

    func testPersistenceFailureDoesNotReplaceCallersRecord() {
        let original = queuedUpload()
        var current = original
        XCTAssertThrowsError(current = try ExpiredUploadRecovery.reset(current, discardTransfer: { _ in }, persist: { _ in
            throw Failure.expected
        }))
        XCTAssertEqual(current.serverSessionID, original.serverSessionID)
        XCTAssertEqual(current.tusUploadID, original.tusUploadID)
        XCTAssertEqual(current.state, original.state)
    }

    func testLateCallbackFromExpiredTransferCannotChangeReplacement() throws {
        let original = queuedUpload()
        let oldID = try XCTUnwrap(original.tusUploadID)
        var replacement = try ExpiredUploadRecovery.reset(original, discardTransfer: { _ in }, persist: { _ in })
        replacement.serverSessionID = "replacement-session"
        replacement.tusUploadID = UUID()
        XCTAssertFalse(replacement.acceptsTransferCallback(id: oldID, sessionID: original.serverSessionID))
        XCTAssertFalse(replacement.acceptsTransferCallback(id: oldID, sessionID: replacement.serverSessionID))
        XCTAssertFalse(replacement.acceptsTransferCallback(id: replacement.tusUploadID!, sessionID: original.serverSessionID))
        XCTAssertTrue(replacement.acceptsTransferCallback(id: replacement.tusUploadID!, sessionID: replacement.serverSessionID))
    }

    func testLateFailureCannotDowngradeAvailableOrQuarantinedRecord() throws {
        var item = queuedUpload()
        let id = try XCTUnwrap(item.tusUploadID)
        for state in [UploadState.available, .quarantined] {
            item.state = state
            XCTAssertFalse(item.acceptsTransferCallback(id: id, sessionID: item.serverSessionID))
        }
    }

    private func queuedUpload() -> QueuedUpload {
        let id = UUID()
        return QueuedUpload(id: id, payloadFilename: "\(id.uuidString).heic", originalFilename: "IMG.heic",
                            typeIdentifier: "public.heic", createdAt: .now, state: .verifying,
                            byteCount: 10, sha256: String(repeating: "a", count: 64),
                            serverSessionID: "expired-session", tusUploadID: UUID(), lastError: "expired")
    }

    private enum Failure: Error { case expected }
}
