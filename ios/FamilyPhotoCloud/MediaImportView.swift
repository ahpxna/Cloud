import CoreTransferable
import PhotosUI
import SwiftUI
import UniformTypeIdentifiers

/// File representations are copied before the provider's temporary URL expires.
/// The selected original is queued locally; only server verification confirms a backup.
private struct ImportedMedia: Transferable, Sendable {
    let queueID: UUID

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(importedContentType: .image) { file in
            ImportedMedia(queueID: try AppGroupQueue.enqueue(ephemeralSource: file.file, type: .image).id)
        }
        FileRepresentation(importedContentType: .movie) { file in
            ImportedMedia(queueID: try AppGroupQueue.enqueue(ephemeralSource: file.file, type: .movie).id)
        }
    }
}

struct MediaImportView: View {
    @ObservedObject var coordinator: UploadCoordinator
    @State private var selection: [PhotosPickerItem] = []
    @State private var isImporting = false
    @State private var imported = 0
    @State private var total = 0
    @State private var receipt: String?

    var body: some View {
        PhotosPicker(selection: $selection, maxSelectionCount: 20,
                     matching: .any(of: [.images, .videos]), preferredItemEncoding: .current) {
            Label("Choose photos and videos", systemImage: "photo.badge.plus")
        }
        .disabled(isImporting)
        if isImporting {
            ProgressView("Importing \(imported) of \(total)…", value: Double(imported), total: Double(max(total, 1)))
        }
        if let receipt { Text(receipt).font(.caption).foregroundStyle(.secondary) }
        Group {
            Text("You can also use Photos → Share → Family Photo Cloud. Keep the originals until they appear in Library.")
            .font(.caption).foregroundStyle(.secondary)
            .onChange(of: selection) { _, items in
                guard !items.isEmpty, !isImporting else { return }
                isImporting = true
                imported = 0
                total = items.count
                receipt = nil
                Task { @MainActor in
                    var failures = 0
                    for item in items {
                        do {
                            guard try await item.loadTransferable(type: ImportedMedia.self) != nil else {
                                failures += 1
                                continue
                            }
                            imported += 1
                        } catch { failures += 1 }
                    }
                    receipt = "\(imported) item(s) queued. \(failures) could not be imported. Sign in to upload; Library shows verified backups."
                    selection = []
                    isImporting = false
                    coordinator.reload()
                    await coordinator.startQueuedUploads(retryFailed: true)
                }
            }
        }
    }
}
