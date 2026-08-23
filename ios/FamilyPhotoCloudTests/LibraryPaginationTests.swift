import XCTest
@testable import FamilyPhotoCloud

final class LibraryPaginationTests: XCTestCase {
    func testAppendKeepsEveryPageAndRemovesBoundaryDuplicate() {
        let first = (0..<50).map { asset(id: "asset-\($0)") }
        var pagination = LibraryPagination()
        pagination.replace(with: LibraryPage(assets: first, nextCursor: "page-2"))

        pagination.append(
            LibraryPage(
                assets: [asset(id: "asset-49"), asset(id: "asset-50"), asset(id: "asset-51")],
                nextCursor: nil
            ),
            requestedCursor: "page-2"
        )

        XCTAssertEqual(pagination.assets.count, 52)
        XCTAssertEqual(Set(pagination.assets.map(\.id)).count, 52)
        XCTAssertFalse(pagination.hasMore)
    }

    func testRepeatedCursorStopsPaginationLoop() {
        var pagination = LibraryPagination()
        pagination.replace(with: LibraryPage(assets: [asset(id: "asset-1")], nextCursor: "repeat"))

        pagination.append(
            LibraryPage(assets: [asset(id: "asset-2")], nextCursor: "repeat"),
            requestedCursor: "repeat"
        )

        XCTAssertEqual(pagination.assets.map(\.id), ["asset-1", "asset-2"])
        XCTAssertFalse(pagination.hasMore)
    }

    private func asset(id: String) -> LibraryAsset {
        LibraryAsset(
            id: id,
            originalFilename: "\(id).heic",
            mediaType: "image/heic",
            byteSize: 1,
            contentSHA256: String(repeating: "0", count: 64),
            createdAt: Date(timeIntervalSince1970: 1),
            originalURL: "/v1/assets/\(id)/original"
        )
    }
}
