import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class LibraryAuthenticationTests: XCTestCase {
    @MainActor
    func testOldListResponseCannotRepopulateLibraryAfterSignOut() async {
        var store: LibraryStore!
        store = LibraryStore(pageLoader: { _, _ in
            // Authentication changes while this request owns the old revision.
            store.clearForAuthenticationChange()
            return LibraryPage(assets: [self.asset()], nextCursor: "old-cursor")
        }, originalDownloader: { _ in throw CancellationError() })
        await store.reload()
        XCTAssertTrue(store.assets.isEmpty)
        XCTAssertFalse(store.hasMore)
        XCTAssertFalse(store.isLoading)
    }

    @MainActor
    func testOldPaginationResponseCannotAppendAfterAccountChange() async {
        var store: LibraryStore!
        store = LibraryStore(pageLoader: { cursor, _ in
            if cursor != nil { store.clearForAuthenticationChange() }
            return LibraryPage(assets: [self.asset()], nextCursor: "old-cursor")
        }, originalDownloader: { _ in throw CancellationError() })
        await store.reload()
        XCTAssertEqual(store.assets.count, 1)
        await store.loadMore()
        XCTAssertTrue(store.assets.isEmpty)
        XCTAssertFalse(store.hasMore)
        XCTAssertFalse(store.isLoadingMore)
    }

    @MainActor
    func testOldDetailCannotDownloadAfterAccountChange() async {
        let original = asset()
        var downloaded = false
        let store = LibraryStore(pageLoader: { _, _ in
            LibraryPage(assets: [original], nextCursor: nil)
        }, originalDownloader: { _ in
            downloaded = true
            throw URLError(.badServerResponse)
        })
        await store.reload()
        let oldRevision = store.revision
        store.clearForAuthenticationChange()
        XCTAssertNotEqual(store.revision, oldRevision)
        do {
            _ = try await store.localURL(for: original)
            XCTFail("A detail from the old account must be invalidated")
        } catch is CancellationError { }
        catch { XCTFail("Unexpected error: \(error)") }
        XCTAssertFalse(downloaded)
    }

    private func asset() -> LibraryAsset {
        LibraryAsset(id: "old-asset", originalFilename: "IMG.heic", mediaType: "image/heic",
                     byteSize: 1, contentSHA256: "abc", createdAt: .now, originalURL: "/v1/assets/old-asset/original")
    }
}
