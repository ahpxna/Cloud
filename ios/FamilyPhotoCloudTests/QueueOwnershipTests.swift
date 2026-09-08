import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class QueueOwnershipTests: XCTestCase {
    func testInterruptedUploadCannotBeReassignedToAnotherLogin() throws {
        var item = upload()
        try item.claim(for: "parent-a")
        XCTAssertThrowsError(try item.claim(for: "parent-b"))
        XCTAssertEqual(item.ownerUserID, "parent-a")
        XCTAssertNoThrow(try item.claim(for: "parent-a"))
    }

    func testOwnerSurvivesPersistenceAndExpiredSessionRecovery() throws {
        var item = upload()
        try item.claim(for: "parent-a")
        let restored = try JSONDecoder().decode(QueuedUpload.self, from: JSONEncoder().encode(item))
        let reset = try ExpiredUploadRecovery.reset(restored, discardTransfer: { _ in }, persist: { _ in })
        XCTAssertEqual(reset.ownerUserID, "parent-a")
    }

    func testLegacyRecordRemainsUnboundAndCanBeClaimedOnce() throws {
        let original = upload()
        let data = try JSONEncoder().encode(original)
        var legacy = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        legacy.removeValue(forKey: "ownerUserID")
        var decoded = try JSONDecoder().decode(QueuedUpload.self, from: JSONSerialization.data(withJSONObject: legacy))
        XCTAssertNil(decoded.ownerUserID)
        try decoded.claim(for: "parent-a")
        XCTAssertThrowsError(try decoded.claim(for: "parent-b"))
    }

    private func upload() -> QueuedUpload {
        let id = UUID()
        return QueuedUpload(id: id, payloadFilename: "\(id.uuidString).heic", originalFilename: "IMG.heic",
                            typeIdentifier: "public.heic", createdAt: .now, state: .queued,
                            byteCount: nil, sha256: nil, serverSessionID: nil, tusUploadID: nil, lastError: nil)
    }
}
