import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class AvailableUploadFinalizerTests: XCTestCase {
    func testPersistenceFailureKeepsVerifiedStateAndSkipsCleanup() {
        var persistedStates: [UploadState] = []
        var cleanupCalled = false

        let result = AvailableUploadFinalizer.finalize(
            queuedUpload(state: .verifying),
            persist: { item in
                persistedStates.append(item.state)
                throw TestFailure.expected
            },
            cleanup: { _ in cleanupCalled = true }
        )

        guard case .persistencePending(let item, _) = result else {
            return XCTFail("Expected local persistence to remain pending")
        }
        XCTAssertEqual(item.state, .available)
        XCTAssertNil(item.lastError)
        XCTAssertEqual(persistedStates, [.available])
        XCTAssertFalse(cleanupCalled)
    }

    func testCleanupFailureKeepsVerifiedState() {
        var persistedStates: [UploadState] = []

        let result = AvailableUploadFinalizer.finalize(
            queuedUpload(state: .verifying),
            persist: { persistedStates.append($0.state) },
            cleanup: { _ in throw TestFailure.expected }
        )

        guard case .cleanupPending(let item, _) = result else {
            return XCTFail("Expected local cleanup to remain pending")
        }
        XCTAssertEqual(item.state, .available)
        XCTAssertNil(item.lastError)
        XCTAssertEqual(persistedStates, [.available])
    }

    private func queuedUpload(state: UploadState) -> QueuedUpload {
        let id = UUID()
        return QueuedUpload(
            id: id,
            payloadFilename: "\(id.uuidString).heic",
            originalFilename: "IMG.heic",
            typeIdentifier: "public.heic",
            createdAt: Date(timeIntervalSince1970: 1),
            state: state,
            byteCount: 1,
            sha256: String(repeating: "0", count: 64),
            serverSessionID: UUID().uuidString,
            tusUploadID: UUID(),
            lastError: "stale error"
        )
    }

    private enum TestFailure: Error {
        case expected
    }
}
