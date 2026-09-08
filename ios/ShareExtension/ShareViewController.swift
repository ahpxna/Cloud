import Social
import UniformTypeIdentifiers

final class ShareViewController: SLComposeServiceViewController {
    private var isImporting = false

    override func viewDidLoad() {
        super.viewDidLoad()
        title = "Add to Family Photo Cloud"
        placeholder = "Images and videos are saved to the upload queue. Open Family Photo Cloud to finish backing them up."
    }

    override func isContentValid() -> Bool { !isImporting }

    override func didSelectPost() {
        guard !isImporting else { return }
        isImporting = true
        validateContent()
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
            let title = failures.isEmpty ? "Added to upload queue" : "Import finished with errors"
            let message = "\(succeeded) item(s) queued. \(failures.count) item(s) could not be imported. " +
                "Open Family Photo Cloud to upload the queued items. Keep your originals until they appear in Library."
            let receipt = UIAlertController(title: title, message: message, preferredStyle: .alert)
            receipt.addAction(UIAlertAction(title: "Done", style: .default) { [weak self] _ in
                self?.extensionContext?.completeRequest(returningItems: nil)
            })
            self.present(receipt, animated: true)
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
