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
                }
                Section("Upload queue") {
                    Button("Resume and check status") {
                        Task {
                            await coordinator.startQueuedUploads()
                            await library.reload()
                        }
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
