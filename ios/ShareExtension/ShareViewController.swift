import Social
import UniformTypeIdentifiers

final class ShareViewController: SLComposeServiceViewController {
    override func isContentValid() -> Bool { true }

    override func didSelectPost() {
        let providers: [(NSItemProvider, UTType)] = (extensionContext?.inputItems ?? [])
            .compactMap { $0 as? NSExtensionItem }
            .flatMap { $0.attachments ?? [] }
            .compactMap { provider -> (NSItemProvider, UTType)? in
                if provider.hasItemConformingToTypeIdentifier(UTType.image.identifier) { return (provider, .image) }
                if provider.hasItemConformingToTypeIdentifier(UTType.movie.identifier) { return (provider, .movie) }
                return nil
            }
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
            if failures.isEmpty {
                self.extensionContext?.completeRequest(returningItems: nil)
            } else if succeeded > 0 {
                // Queue writes are already durable, so do not discard the
                // successful originals. However, completion would falsely
                // imply every selected item was backed up; return an explicit
                // receipt the share sheet can show instead of silently omitting
                // failed providers.
                self.cancel(with: "\(succeeded) item(s) queued; \(failures.count) could not be imported. Keep the originals and try sharing the failed item(s) again.")
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
