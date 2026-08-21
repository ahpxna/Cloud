import AVKit
import CryptoKit
import SwiftUI
import UIKit

@MainActor
final class LibraryStore: ObservableObject {
    @Published private(set) var assets: [LibraryAsset] = []
    @Published private(set) var error: String?
    @Published private(set) var isLoading = false

    private let coordinator: UploadCoordinator
    private var cachedURLs: [String: URL] = [:]

    init(coordinator: UploadCoordinator) {
        self.coordinator = coordinator
    }

    func reload() async {
        isLoading = true
        defer { isLoading = false }
        do {
            assets = try await coordinator.libraryAssets()
            error = nil
        } catch {
            error = error.localizedDescription
        }
    }

    func localURL(for asset: LibraryAsset) async throws -> URL {
        if let cached = cachedURLs[asset.id], FileManager.default.fileExists(atPath: cached.path()) {
            return cached
        }
        let temporaryURL = try await coordinator.downloadOriginal(asset)
        do {
            let digest = try sha256(of: temporaryURL)
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
        let target = caches.appending(path: "\(asset.id).\(asset.originalFilename.pathExtension)")
        try? FileManager.default.removeItem(at: target)
        try FileManager.default.moveItem(at: temporaryURL, to: target)
        cachedURLs[asset.id] = target
        return target
    }

    private func sha256(of url: URL) throws -> String {
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        var hasher = SHA256()
        while let data = try handle.read(upToCount: 1 << 20), !data.isEmpty {
            hasher.update(data: data)
        }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
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
                    List(store.assets) { asset in
                        NavigationLink(asset.originalFilename) {
                            AssetDetailView(asset: asset, store: store)
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
