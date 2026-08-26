import AVKit
import SwiftUI
import UIKit

struct LibraryPagination {
    private(set) var assets: [LibraryAsset] = []
    private(set) var nextCursor: String?

    var hasMore: Bool { nextCursor != nil }

    mutating func replace(with page: LibraryPage) {
        assets = page.assets
        nextCursor = page.nextCursor
    }

    mutating func append(_ page: LibraryPage, requestedCursor: String) {
        let existingIDs = Set(assets.map(\.id))
        assets.append(contentsOf: page.assets.filter { !existingIDs.contains($0.id) })
        nextCursor = page.nextCursor == requestedCursor ? nil : page.nextCursor
    }
}

@MainActor
final class LibraryStore: ObservableObject {
    @Published private(set) var assets: [LibraryAsset] = []
    @Published private(set) var error: String?
    @Published private(set) var isLoading = false
    @Published private(set) var isLoadingMore = false
    @Published private(set) var hasMore = false

    private let coordinator: UploadCoordinator
    private var cachedURLs: [String: URL] = [:]
    private var pagination = LibraryPagination()
    private let pageSize = 50

    init(coordinator: UploadCoordinator) {
        self.coordinator = coordinator
    }

    func reload() async {
        guard !isLoading, !isLoadingMore else { return }
        isLoading = true
        defer { isLoading = false }
        do {
            let page = try await coordinator.libraryPage(cursor: nil, limit: pageSize)
            pagination.replace(with: page)
            applyPaginationState()
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    func loadMoreIfNeeded(current asset: LibraryAsset) async {
        guard asset.id == assets.last?.id else { return }
        await loadMore()
    }

    func loadMore() async {
        guard !isLoading, !isLoadingMore, let cursor = pagination.nextCursor else { return }
        isLoadingMore = true
        defer { isLoadingMore = false }
        do {
            let page = try await coordinator.libraryPage(cursor: cursor, limit: pageSize)
            pagination.append(page, requestedCursor: cursor)
            applyPaginationState()
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func applyPaginationState() {
        assets = pagination.assets
        hasMore = pagination.hasMore
    }

    func localURL(for asset: LibraryAsset) async throws -> URL {
        if let cached = cachedURLs[asset.id], FileManager.default.fileExists(atPath: cached.path()) {
            return cached
        }
        let temporaryURL = try await coordinator.downloadOriginal(asset)
        do {
            // Verification can read multi-gigabyte videos. Reuse the detached
            // uploader hash worker rather than monopolising MainActor.
            let digest = try await HashWorker.sha256(of: temporaryURL)
            guard digest.caseInsensitiveCompare(asset.contentSHA256) == .orderedSame else {
                throw APIProblem(status: 409, code: "download_integrity_mismatch", detail: "Downloaded original does not match the server's verified SHA-256.")
            }
        } catch {
            try? FileManager.default.removeItem(at: temporaryURL)
            throw error
        }
        let caches = try FileManager.default.url(
            for: .cachesDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        ).appending(path: "VerifiedOriginals", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: caches, withIntermediateDirectories: true)
        let pathExtension = URL(fileURLWithPath: asset.originalFilename).pathExtension
        let cacheFilename = pathExtension.isEmpty ? asset.id : "\(asset.id).\(pathExtension)"
        let target = caches.appending(path: cacheFilename)
        try? FileManager.default.removeItem(at: target)
        try FileManager.default.moveItem(at: temporaryURL, to: target)
        cachedURLs[asset.id] = target
        return target
    }

}

struct LibraryView: View {
    @ObservedObject var store: LibraryStore

    var body: some View {
        NavigationStack {
            Group {
                if store.assets.isEmpty && !store.isLoading {
                    ContentUnavailableView("No verified photos yet", systemImage: "photo.on.rectangle", description: Text(store.error ?? "Add a photo from the Share Sheet, then wait for server verification."))
                } else {
                    List {
                        ForEach(store.assets) { asset in
                            NavigationLink(asset.originalFilename) {
                                AssetDetailView(asset: asset, store: store)
                            }
                            .task { await store.loadMoreIfNeeded(current: asset) }
                        }
                        if store.isLoadingMore {
                            ProgressView("Loading more…")
                                .frame(maxWidth: .infinity)
                        } else if store.hasMore {
                            Button("Load more") { Task { await store.loadMore() } }
                                .frame(maxWidth: .infinity)
                        }
                    }
                    .refreshable { await store.reload() }
                }
            }
            .overlay { if store.isLoading { ProgressView() } }
            .navigationTitle("Library")
            .toolbar { Button("Refresh") { Task { await store.reload() } } }
            .task { await store.reload() }
        }
    }
}

private struct AssetDetailView: View {
    let asset: LibraryAsset
    @ObservedObject var store: LibraryStore
    @State private var localURL: URL?
    @State private var error: String?

    var body: some View {
        Group {
            if let localURL, asset.mediaType.hasPrefix("image/"), let image = UIImage(contentsOfFile: localURL.path()) {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFit()
                    .accessibilityLabel(asset.originalFilename)
            } else if let localURL, asset.mediaType.hasPrefix("video/") {
                VideoPlayer(player: AVPlayer(url: localURL))
            } else if let error {
                ContentUnavailableView("Could not open original", systemImage: "exclamationmark.triangle", description: Text(error))
            } else {
                ProgressView("Loading verified original…")
            }
        }
        .navigationTitle(asset.originalFilename)
        .navigationBarTitleDisplayMode(.inline)
        .task {
            do { localURL = try await store.localURL(for: asset) }
            catch { self.error = error.localizedDescription }
        }
    }
}
