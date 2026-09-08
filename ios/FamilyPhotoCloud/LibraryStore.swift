import AVKit
import Combine
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

    private let pageLoader: (String?, Int) async throws -> LibraryPage
    private let originalDownloader: (LibraryAsset) async throws -> URL
    private var authenticationObserver: AnyCancellable?
    @Published private(set) var revision = UUID()
    private var cachedURLs: [String: URL] = [:]
    private var pagination = LibraryPagination()
    private let pageSize = 50

    init(
        pageLoader: @escaping (String?, Int) async throws -> LibraryPage,
        originalDownloader: @escaping (LibraryAsset) async throws -> URL
    ) {
        self.pageLoader = pageLoader
        self.originalDownloader = originalDownloader
    }

    convenience init(coordinator: UploadCoordinator) {
        self.init(
            pageLoader: { try await coordinator.libraryPage(cursor: $0, limit: $1) },
            originalDownloader: { try await coordinator.downloadOriginal($0) }
        )
        authenticationObserver = coordinator.$authenticationRevision.sink { [weak self] _ in
            self?.clearForAuthenticationChange()
        }
    }

    func clearForAuthenticationChange() {
        revision = UUID()
        for url in cachedURLs.values { try? FileManager.default.removeItem(at: url) }
        cachedURLs = [:]
        pagination = LibraryPagination()
        applyPaginationState()
        isLoading = false
        isLoadingMore = false
        error = nil
    }

    func reload() async {
        guard !isLoading, !isLoadingMore else { return }
        let requestRevision = revision
        isLoading = true
        defer { if revision == requestRevision { isLoading = false } }
        do {
            let page = try await pageLoader(nil, pageSize)
            guard revision == requestRevision else { return }
            pagination.replace(with: page)
            applyPaginationState()
            error = nil
        } catch {
            guard revision == requestRevision else { return }
            pagination = LibraryPagination()
            applyPaginationState()
            self.error = error.localizedDescription
        }
    }

    func loadMoreIfNeeded(current asset: LibraryAsset) async {
        guard asset.id == assets.last?.id else { return }
        await loadMore()
    }

    func loadMore() async {
        guard !isLoading, !isLoadingMore, let cursor = pagination.nextCursor else { return }
        let requestRevision = revision
        isLoadingMore = true
        defer { if revision == requestRevision { isLoadingMore = false } }
        do {
            let page = try await pageLoader(cursor, pageSize)
            guard revision == requestRevision else { return }
            pagination.append(page, requestedCursor: cursor)
            applyPaginationState()
            error = nil
        } catch {
            guard revision == requestRevision else { return }
            self.error = error.localizedDescription
        }
    }

    private func applyPaginationState() {
        assets = pagination.assets
        hasMore = pagination.hasMore
    }

    func localURL(for asset: LibraryAsset) async throws -> URL {
        let requestRevision = revision
        guard assets.contains(where: { $0.id == asset.id }) else { throw CancellationError() }
        if let cached = cachedURLs[asset.id], FileManager.default.fileExists(atPath: cached.path()) {
            return cached
        }
        let temporaryURL = try await originalDownloader(asset)
        defer { try? FileManager.default.removeItem(at: temporaryURL) }
        // Verification can read multi-gigabyte videos. Reuse the detached
        // uploader hash worker rather than monopolising MainActor.
        let digest = try await HashWorker.sha256(of: temporaryURL)
        guard revision == requestRevision else { throw CancellationError() }
        guard digest.caseInsensitiveCompare(asset.contentSHA256) == .orderedSame else {
            throw APIProblem(status: 409, code: "download_integrity_mismatch", detail: "Downloaded original does not match the server's verified SHA-256.")
        }
        // Another request may have completed while this request downloaded or
        // hashed. Reuse its file instead of deleting a URL already in use.
        if let cached = cachedURLs[asset.id], FileManager.default.fileExists(atPath: cached.path()) {
            return cached
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
        .id(store.revision)
    }
}

private struct AssetDetailView: View {
    let asset: LibraryAsset
    @ObservedObject var store: LibraryStore
    @State private var error: String?
    @State private var image: UIImage?
    @State private var player: AVPlayer?

    var body: some View {
        Group {
            if let image {
                Image(uiImage: image)
                    .resizable()
                    .scaledToFit()
                    .accessibilityLabel(asset.originalFilename)
            } else if let player {
                VideoPlayer(player: player)
            } else if let error {
                ContentUnavailableView("Could not open original", systemImage: "exclamationmark.triangle", description: Text(error))
            } else {
                ProgressView("Loading verified original…")
            }
        }
        .navigationTitle(asset.originalFilename)
        .navigationBarTitleDisplayMode(.inline)
        .task {
            do {
                let url = try await store.localURL(for: asset)
                try Task.checkCancellation()
                if asset.mediaType.hasPrefix("image/") {
                    image = UIImage(contentsOfFile: url.path())
                    if image == nil { error = "This image format could not be displayed." }
                } else if asset.mediaType.hasPrefix("video/") {
                    player = AVPlayer(url: url)
                } else {
                    error = "This media format could not be displayed."
                }
            }
            catch { self.error = error.localizedDescription }
        }
        .onDisappear { player?.pause() }
    }
}
