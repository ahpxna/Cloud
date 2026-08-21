import Social
import UniformTypeIdentifiers

final class ShareViewController: SLComposeServiceViewController {
    override func isContentValid() -> Bool { true }

    override func didSelectPost() {
        guard let provider = extensionContext?.inputItems
            .compactMap({ $0 as? NSExtensionItem })
            .flatMap(\.attachments ?? [])
            .first(where: { $0.hasItemConformingToTypeIdentifier(UTType.image.identifier) || $0.hasItemConformingToTypeIdentifier(UTType.movie.identifier) })
        else {
            cancel(with: "Choose an image or video to add to the queue.")
            return
        }

        let type: UTType = provider.hasItemConformingToTypeIdentifier(UTType.image.identifier) ? .image : .movie
        provider.loadFileRepresentation(forTypeIdentifier: type.identifier) { [weak self] url, error in
            guard let self else { return }
            do {
                if let error { throw error }
                guard let url else { throw AppGroupQueueError.unsupportedPayload }
                _ = try AppGroupQueue.enqueue(ephemeralSource: url, type: type)
                self.extensionContext?.completeRequest(returningItems: nil)
            } catch {
                self.cancel(with: error.localizedDescription)
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
