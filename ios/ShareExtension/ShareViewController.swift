import Social
import UniformTypeIdentifiers

final class ShareViewController: SLComposeServiceViewController {
    override func isContentValid() -> Bool { true }

    override func didSelectPost() {
        let providers = extensionContext?.inputItems
            .compactMap({ $0 as? NSExtensionItem })
            .flatMap(\.attachments ?? [])
            .compactMap { provider -> (NSItemProvider, UTType)? in
                if provider.hasItemConformingToTypeIdentifier(UTType.image.identifier) { return (provider, .image) }
                if provider.hasItemConformingToTypeIdentifier(UTType.movie.identifier) { return (provider, .movie) }
                return nil
            } ?? []
        guard !providers.isEmpty else {
            cancel(with: "Choose an image or video to add to the queue.")
            return
        }

        Task { @MainActor [weak self] in
            var succeeded = 0
            var failures: [String] = []
            for (provider, type) in providers {
                do {
                    try await Self.enqueue(provider: provider, type: type)
                    succeeded += 1
                } catch {
                    failures.append(error.localizedDescription)
                }
            }
            guard let self else { return }
            if succeeded > 0 {
                // Every selected provider was attempted. A partial failure does
                // not cancel successfully queued originals; the host app shows
                // retry state for each record it owns.
                self.extensionContext?.completeRequest(returningItems: nil)
            } else {
                self.cancel(with: failures.first ?? "Could not add the selected items to the queue.")
            }
        }
    }

    private static func enqueue(provider: NSItemProvider, type: UTType) async throws {
        try await withCheckedThrowingContinuation { continuation in
            provider.loadFileRepresentation(forTypeIdentifier: type.identifier) { url, error in
                do {
                    if let error { throw error }
                    guard let url else { throw AppGroupQueueError.unsupportedPayload }
                    _ = try AppGroupQueue.enqueue(ephemeralSource: url, type: type)
                    continuation.resume(returning: ())
                } catch {
                    continuation.resume(throwing: error)
                }
            }
        }
    }

    override func configurationItems() -> [Any]! { [] }

    private func cancel(with description: String) {
        extensionContext?.cancelRequest(withError: NSError(
            domain: "FamilyPhotoCloudShare",
            code: 1,
            userInfo: [NSLocalizedDescriptionKey: description]
        ))
    }
}
