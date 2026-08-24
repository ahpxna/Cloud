import SwiftUI
import UIKit

@main
struct FamilyPhotoCloudApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var environment = AppEnvironment.shared

    var body: some Scene {
        WindowGroup {
            ContentView(environment: environment)
                .task { await environment.startUploads() }
        }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication, handleEventsForBackgroundURLSession identifier: String, completionHandler: @escaping () -> Void) {
        AppEnvironment.shared.registerBackgroundHandler(completionHandler, identifier: identifier)
    }
}

@MainActor
final class AppEnvironment: ObservableObject {
    static let shared = AppEnvironment()
    let coordinator: UploadCoordinator?
    let library: LibraryStore?
    @Published var configurationError: String?

    private init() {
        guard let rawURL = Bundle.main.object(forInfoDictionaryKey: "PhotoCloudAPIBaseURL") as? String,
              let url = URL(string: rawURL), url.scheme == "https", url.host != "photos.example.com"
        else {
            coordinator = nil
            library = nil
            configurationError = "Set PhotoCloudAPIBaseURL to your HTTPS Cloudflare Tunnel hostname before signing in."
            return
        }
        do {
            let coordinator = try UploadCoordinator(apiBaseURL: url)
            self.coordinator = coordinator
            library = LibraryStore(coordinator: coordinator)
        }
        catch {
            coordinator = nil
            library = nil
            configurationError = error.localizedDescription
        }
    }

    func startUploads() async { await coordinator?.startQueuedUploads() }
    func registerBackgroundHandler(_ completion: @escaping () -> Void, identifier: String) {
        guard let coordinator else { completion(); return }
        coordinator.registerBackgroundHandler(completion, identifier: identifier)
    }
}

struct ContentView: View {
    @ObservedObject var environment: AppEnvironment
    @State private var email = ""
    @State private var password = ""

    var body: some View {
        NavigationStack {
            Group {
                if let error = environment.configurationError {
                    ContentUnavailableView("Setup required", systemImage: "gear.badge", description: Text(error))
                } else if let coordinator = environment.coordinator, let library = environment.library {
                    TabView {
                        LibraryView(store: library)
                            .tabItem { Label("Library", systemImage: "photo.on.rectangle") }
                        UploadQueueView(coordinator: coordinator, library: library, email: $email, password: $password)
                            .tabItem { Label("Uploads", systemImage: "arrow.up.circle") }
                    }
                }
            }
        }
    }
}

private struct UploadQueueView: View {
    @ObservedObject var coordinator: UploadCoordinator
    @ObservedObject var library: LibraryStore
    @Binding var email: String
    @Binding var password: String
    @State private var mfaCode = ""
    @State private var recoveryCode = ""
    @State private var enrollmentCode = ""
    @State private var securityCode = ""
    @State private var showDisableMFAConfirmation = false

    var body: some View {
        NavigationStack {
            List {
                Section("Sign in") {
                    TextField("Email", text: $email).textInputAutocapitalization(.never).keyboardType(.emailAddress)
                    SecureField("Password", text: $password)
                    Button("Sign in and upload") {
                        Task {
                            await coordinator.login(email: email, password: password)
                            await library.reload()
                        }
                    }

                    if coordinator.mfaChallenge != nil {
                        Text("Multi-factor authentication required")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        TextField("6-digit authenticator code", text: $mfaCode)
                            .keyboardType(.numberPad)
                            .textContentType(.oneTimeCode)
                        TextField("Recovery code (alternative)", text: $recoveryCode)
                            .textInputAutocapitalization(.characters)
                            .autocorrectionDisabled()
                        if let expiry = coordinator.mfaExpiresAt {
                            Text("Challenge expires \(expiry, style: .relative).")
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                        }
                        Button("Verify MFA") {
                            Task {
                                await coordinator.verifyMFA(totpCode: mfaCode, recoveryCode: recoveryCode)
                                if coordinator.mfaChallenge == nil {
                                    mfaCode = ""
                                    recoveryCode = ""
                                    await library.reload()
                                }
                            }
                        }
                    }
                }

                Section("MFA security") {
                    Button("Begin authenticator enrollment") {
                        Task { await coordinator.beginMFAEnrollment() }
                    }

                    if let enrollment = coordinator.mfaEnrollment {
                        Text("Add this secret to an authenticator, then confirm with the current 6-digit code.")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        LabeledContent("Secret") {
                            Text(enrollment.secret)
                                .font(.caption.monospaced())
                                .textSelection(.enabled)
                        }
                        Text(enrollment.otpauthURI)
                            .font(.caption2.monospaced())
                            .textSelection(.enabled)
                        TextField("Enrollment authenticator code", text: $enrollmentCode)
                            .keyboardType(.numberPad)
                            .textContentType(.oneTimeCode)
                        Button("Confirm MFA enrollment") {
                            Task {
                                await coordinator.confirmMFAEnrollment(totpCode: enrollmentCode)
                                if coordinator.mfaEnrollment == nil { enrollmentCode = "" }
                            }
                        }
                    }

                    if !coordinator.mfaRecoveryCodes.isEmpty {
                        Text("Save these one-time recovery codes now. The app does not persist them after this view state is lost.")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        ForEach(coordinator.mfaRecoveryCodes, id: \.self) { code in
                            Text(code).font(.caption.monospaced()).textSelection(.enabled)
                        }
                    }

                    TextField("Current authenticator code", text: $securityCode)
                        .keyboardType(.numberPad)
                        .textContentType(.oneTimeCode)
                    Button("Rotate recovery codes") {
                        Task {
                            await coordinator.rotateMFARecoveryCodes(totpCode: securityCode)
                            securityCode = ""
                        }
                    }
                    Button("Disable MFA", role: .destructive) {
                        showDisableMFAConfirmation = true
                    }
                    .confirmationDialog(
                        "Disable multi-factor authentication?",
                        isPresented: $showDisableMFAConfirmation,
                        titleVisibility: .visible
                    ) {
                        Button("Disable MFA", role: .destructive) {
                            Task {
                                await coordinator.disableMFA(totpCode: securityCode)
                                securityCode = ""
                            }
                        }
                        Button("Cancel", role: .cancel) {}
                    } message: {
                        Text("A valid current authenticator code is required. Future sign-ins will no longer require the second factor.")
                    }
                }

                if !coordinator.quarantinedRecords.isEmpty {
                    Section("Queue recovery") {
                        Text("Corrupt queue metadata is isolated from payload bytes. Recovery creates a fresh upload identity and restarts from byte zero.")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        ForEach(coordinator.quarantinedRecords) { record in
                            VStack(alignment: .leading, spacing: 6) {
                                Text(record.filename).lineLimit(1)
                                if let modifiedAt = record.modifiedAt {
                                    Text(modifiedAt, style: .relative)
                                        .font(.caption2)
                                        .foregroundStyle(.secondary)
                                }
                                Button("Recover as new upload") {
                                    Task {
                                        await coordinator.recoverQuarantinedRecord(record)
                                        await library.reload()
                                    }
                                }
                            }
                        }
                    }
                }

                Section("Diagnostics") {
                    Button("Prepare diagnostics export") {
                        coordinator.prepareDiagnosticsExport()
                    }
                    if let exportURL = coordinator.diagnosticsExportURL {
                        ShareLink(item: exportURL) {
                            Label("Share diagnostics", systemImage: "square.and.arrow.up")
                        }
                    }
                    Text("Diagnostics contain queue/session identifiers, offsets, timestamps, app state and errors; credentials and bearer tokens are excluded.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }

                Section("Upload queue") {
                    Button("Resume and check status") {
                        Task {
                            await coordinator.startQueuedUploads()
                            await library.reload()
                        }
                    }
                    if let error = coordinator.lastError {
                        Text(error).font(.caption).foregroundStyle(.red)
                    }
                    ForEach(coordinator.uploads) { item in
                        VStack(alignment: .leading) {
                            Text(item.originalFilename).lineLimit(1)
                            Text(item.state.rawValue).font(.caption).foregroundStyle(.secondary)
                            if let error = item.lastError { Text(error).font(.caption2).foregroundStyle(.red) }
                        }
                    }
                }
            }
            .navigationTitle("Uploads")
            .task { coordinator.reload() }
        }
    }
}
