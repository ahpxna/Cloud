import SwiftUI

struct HelpView: View {
    private var privacyURL: URL? { configuredURL("PhotoCloudPrivacyPolicyURL") }
    private var supportURL: URL? { configuredURL("PhotoCloudSupportURL") }

    var body: some View {
        NavigationStack {
            List {
                Section("Back up your photos") {
                    Label("Sign in with the account provided by your cloud administrator.", systemImage: "person.crop.circle")
                    Label("Open Uploads and choose photos, or share them from Photos to Family Photo Cloud.", systemImage: "photo.badge.plus")
                    Label("Return to this app to start uploading. After an interruption, use Resume and check status.", systemImage: "arrow.clockwise")
                    Label("Wait for photos to appear in Library. The server checks the entire file before making it available.", systemImage: "checkmark.shield")
                }
                Section("Your originals") {
                    Text("Adding an item to the upload queue is not a completed backup. Keep the copy in Photos until server verification finishes.")
                    Text("Only selected photos and videos are imported. This version does not automatically back up your entire library. Live Photos, edits, albums and other Photos metadata may not be preserved as a complete Photos-library backup.")
                    Text("Original files may contain location and camera metadata. Uploaded originals are stored on the configured cloud server. Network encryption does not mean end-to-end encryption from the server operator.")
                }
                Section("Privacy and support") {
                    if let privacyURL {
                        Link("Privacy policy", destination: privacyURL)
                    } else {
                        Text("Privacy policy is not configured in this build.")
                    }
                    if let supportURL {
                        Link("Contact support", destination: supportURL)
                    } else {
                        Text("Ask your cloud administrator for account and storage support.")
                    }
                    Text("For an upload problem, open Uploads → Diagnostics. Export is optional; review the log before choosing who receives it.")
                }
                Section("About") {
                    LabeledContent("Version", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "—")
                    LabeledContent("Build", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "—")
                }
            }
            .navigationTitle("Help & privacy")
        }
    }

    private func configuredURL(_ key: String) -> URL? {
        guard let raw = Bundle.main.object(forInfoDictionaryKey: key) as? String,
              let url = URL(string: raw), url.scheme == "https",
              let host = url.host, !host.isEmpty, url.user == nil, url.password == nil else { return nil }
        return url
    }
}
