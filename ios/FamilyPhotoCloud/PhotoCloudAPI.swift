import Foundation

struct Credential: Codable, Sendable {
    var accessToken: String
    var accessExpiresAt: Date
    var refreshToken: String
    var refreshExpiresAt: Date
    var userID: String
    // Persisted before a refresh request is sent so a lost HTTP response or app
    // restart retries the same rotation request ID instead of looking like token theft.
    var pendingRefreshRequestID: String? = nil
}

struct UploadSession: Codable, Sendable {
    let id: String
    let state: String
    let expectedSize: Int64
    let receivedSize: Int64
    let uploadEndpoint: String
    let sessionIDMetadata: String
    let recommendedChunkBytes: Int64
    let uploadToken: String?

    enum CodingKeys: String, CodingKey {
        case id, state
        case expectedSize = "expected_size"
        case receivedSize = "received_size"
        case uploadEndpoint = "upload_endpoint"
        case sessionIDMetadata = "session_id_metadata"
        case recommendedChunkBytes = "recommended_chunk_bytes"
        case uploadToken = "upload_token"
    }
}

struct LibraryAsset: Codable, Identifiable, Sendable {
    let id: String
    let originalFilename: String
    let mediaType: String
    let byteSize: Int64
    let contentSHA256: String
    let createdAt: Date
    let originalURL: String

    enum CodingKeys: String, CodingKey {
        case id
        case originalFilename = "original_filename"
        case mediaType = "media_type"
        case byteSize = "byte_size"
        case contentSHA256 = "content_sha256"
        case createdAt = "created_at"
        case originalURL = "original_url"
    }
}

struct LibraryPage: Codable, Sendable {
    let assets: [LibraryAsset]
    let nextCursor: String?

    enum CodingKeys: String, CodingKey {
        case assets
        case nextCursor = "next_cursor"
    }
}

enum LoginResult: Sendable {
    case authenticated(Credential)
    case mfaRequired(challenge: String, expiresIn: Int)
}

struct MFAEnrollment: Codable, Sendable {
    let secret: String
    let otpauthURI: String

    enum CodingKeys: String, CodingKey {
        case secret
        case otpauthURI = "otpauth_uri"
    }
}

struct MFARecoveryCodes: Codable, Sendable {
    let recoveryCodes: [String]

    enum CodingKeys: String, CodingKey {
        case recoveryCodes = "recovery_codes"
    }
}

struct DeviceSession: Codable, Identifiable, Sendable {
    let id: String
    let deviceName: String
    let createdAt: Date
    let lastUsedAt: Date
    let expiresAt: Date
    let current: Bool

    enum CodingKeys: String, CodingKey {
        case id
        case deviceName = "device_name"
        case createdAt = "created_at"
        case lastUsedAt = "last_used_at"
        case expiresAt = "expires_at"
        case current
    }
}

private struct DeviceSessionsResponse: Codable, Sendable {
    let sessions: [DeviceSession]
}

struct APIProblem: Codable, Error, LocalizedError, Sendable {
    let status: Int
    let code: String
    let detail: String
    var errorDescription: String? { detail }
}

struct PhotoCloudAPI: Sendable {
    let baseURL: URL
    private let session: URLSession

    init(baseURL: URL, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
    }

    func login(email: String, password: String, deviceName: String) async throws -> LoginResult {
        struct Request: Encodable { let email, password: String; let deviceName: String
            enum CodingKeys: String, CodingKey { case email, password; case deviceName = "device_name" }
        }
        var request = URLRequest(url: try absoluteURL(for: "/v1/auth/login"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(Request(email: email, password: password, deviceName: deviceName))
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw URLError(.badServerResponse) }
        if http.statusCode == 202 {
            let challenge = try JSONDecoder().decode(MFAChallengeResponse.self, from: data)
            return .mfaRequired(challenge: challenge.challenge, expiresIn: challenge.expiresIn)
        }
        guard (200..<300).contains(http.statusCode) else {
            throw (try? JSONDecoder().decode(APIProblem.self, from: data)) ?? URLError(.badServerResponse)
        }
        return .authenticated(try JSONDecoder().decode(CredentialResponse.self, from: data).credential)
    }

    func verifyMFA(challenge: String, totpCode: String?, recoveryCode: String?) async throws -> Credential {
        struct Request: Encodable {
            let challenge: String
            let totpCode: String?
            let recoveryCode: String?
            enum CodingKeys: String, CodingKey {
                case challenge
                case totpCode = "totp_code"
                case recoveryCode = "recovery_code"
            }
        }
        return try await request(
            path: "/v1/auth/mfa/verify",
            method: "POST",
            body: Request(challenge: challenge, totpCode: totpCode, recoveryCode: recoveryCode),
            bearer: nil,
            response: CredentialResponse.self
        ).credential
    }

    func beginMFAEnrollment(password: String, accessToken: String) async throws -> MFAEnrollment {
        struct Request: Encodable { let password: String }
        return try await request(
            path: "/v1/auth/mfa/enroll",
            method: "POST",
            body: Request(password: password),
            bearer: accessToken,
            response: MFAEnrollment.self
        )
    }

    func confirmMFAEnrollment(totpCode: String, accessToken: String) async throws -> MFARecoveryCodes {
        struct Request: Encodable {
            let totpCode: String
            enum CodingKeys: String, CodingKey { case totpCode = "totp_code" }
        }
        return try await request(
            path: "/v1/auth/mfa/confirm",
            method: "POST",
            body: Request(totpCode: totpCode),
            bearer: accessToken,
            response: MFARecoveryCodes.self
        )
    }

    func rotateMFARecoveryCodes(totpCode: String, accessToken: String) async throws -> MFARecoveryCodes {
        struct Request: Encodable {
            let totpCode: String
            enum CodingKeys: String, CodingKey { case totpCode = "totp_code" }
        }
        return try await request(
            path: "/v1/auth/mfa/recovery",
            method: "POST",
            body: Request(totpCode: totpCode),
            bearer: accessToken,
            response: MFARecoveryCodes.self
        )
    }

    func disableMFA(totpCode: String, accessToken: String) async throws {
        struct Request: Encodable {
            let totpCode: String
            enum CodingKeys: String, CodingKey { case totpCode = "totp_code" }
        }
        try await requestNoContent(
            path: "/v1/auth/mfa/disable",
            method: "POST",
            body: Request(totpCode: totpCode),
            bearer: accessToken
        )
    }

    func deviceSessions(accessToken: String) async throws -> [DeviceSession] {
        try await request(
            path: "/v1/auth/sessions",
            method: "GET",
            body: Optional<String>.none,
            bearer: accessToken,
            response: DeviceSessionsResponse.self
        ).sessions
    }

    func revokeDeviceSession(id: String, accessToken: String) async throws {
        try await requestNoContent(
            path: "/v1/auth/sessions/\(id)",
            method: "DELETE",
            body: Optional<String>.none,
            bearer: accessToken
        )
    }

    func logout(refreshToken: String) async throws {
        struct Request: Encodable {
            let refreshToken: String
            enum CodingKeys: String, CodingKey { case refreshToken = "refresh_token" }
        }
        try await requestNoContent(
            path: "/v1/auth/logout",
            method: "POST",
            body: Request(refreshToken: refreshToken),
            bearer: nil
        )
    }

    func refresh(_ refreshToken: String, rotationRequestID: String) async throws -> Credential {
        struct Request: Encodable {
            let refreshToken: String
            let rotationRequestID: String
            enum CodingKeys: String, CodingKey {
                case refreshToken = "refresh_token"
                case rotationRequestID = "rotation_request_id"
            }
        }
        return try await request(
            path: "/v1/auth/refresh",
            method: "POST",
            body: Request(refreshToken: refreshToken, rotationRequestID: rotationRequestID),
            bearer: nil,
            response: CredentialResponse.self
        ).credential
    }

    func createUploadSession(_ uploadRequest: CreateUploadRequest, accessToken: String) async throws -> UploadSession {
        try await request(path: "/v1/upload-sessions", method: "POST", body: uploadRequest, bearer: accessToken, response: UploadSession.self)
    }

    func uploadSession(id: String, accessToken: String) async throws -> UploadSession {
        try await request(path: "/v1/upload-sessions/\(id)", method: "GET", body: Optional<String>.none, bearer: accessToken, response: UploadSession.self)
    }

    func restartUploadSession(id: String, accessToken: String) async throws -> UploadSession {
        try await request(path: "/v1/upload-sessions/\(id)/restart", method: "POST", body: Optional<String>.none, bearer: accessToken, response: UploadSession.self)
    }

    func libraryPage(cursor: String?, limit: Int = 50, accessToken: String) async throws -> LibraryPage {
        guard (1...100).contains(limit) else {
            throw APIProblem(status: 400, code: "invalid_limit", detail: "Library page size must be between 1 and 100.")
        }
        var queryItems = [URLQueryItem(name: "limit", value: String(limit))]
        if let cursor, !cursor.isEmpty {
            queryItems.append(URLQueryItem(name: "cursor", value: cursor))
        }
        return try await request(
            path: "/v1/assets",
            method: "GET",
            queryItems: queryItems,
            body: Optional<String>.none,
            bearer: accessToken,
            response: LibraryPage.self
        )
    }

    func downloadOriginal(_ asset: LibraryAsset, accessToken: String) async throws -> URL {
        var request = URLRequest(url: try absoluteURL(for: asset.originalURL))
        request.setValue("Bearer \(accessToken)", forHTTPHeaderField: "Authorization")
        let (temporaryURL, response) = try await session.download(for: request)
        guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            throw URLError(.badServerResponse)
        }
        return temporaryURL
    }

    static func configuredBaseURL(_ raw: String) throws -> URL {
        guard let url = URL(string: raw), url.scheme == "https",
              let host = url.host, !host.isEmpty,
              host != "example.com", !host.hasSuffix(".example.com"),
              url.user == nil, url.password == nil,
              url.path.isEmpty || url.path == "/",
              url.query == nil, url.fragment == nil else { throw URLError(.badURL) }
        return url
    }

    func absoluteURL(for relativePath: String) throws -> URL {
        guard let url = URL(string: relativePath, relativeTo: baseURL), url.host == baseURL.host, url.scheme == baseURL.scheme,
              (url.port ?? 443) == (baseURL.port ?? 443), url.user == nil, url.password == nil else {
            throw URLError(.badURL)
        }
        return url
    }

    private func requestNoContent<Body: Encodable>(path: String, method: String, body: Body?, bearer: String?) async throws {
        var request = URLRequest(url: try absoluteURL(for: path))
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let bearer { request.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization") }
        if let body {
            request.httpBody = try JSONEncoder().encode(body)
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw URLError(.badServerResponse) }
        guard (200..<300).contains(http.statusCode) else {
            throw (try? JSONDecoder().decode(APIProblem.self, from: data)) ?? URLError(.badServerResponse)
        }
    }

    private func request<Body: Encodable, Response: Decodable>(path: String, method: String, queryItems: [URLQueryItem] = [], body: Body?, bearer: String?, response: Response.Type) async throws -> Response {
        var components = URLComponents(url: try absoluteURL(for: path), resolvingAgainstBaseURL: true)
        components?.queryItems = queryItems.isEmpty ? nil : queryItems
        guard let url = components?.url else { throw URLError(.badURL) }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let bearer { request.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization") }
        if let body {
            request.httpBody = try JSONEncoder().encode(body)
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw URLError(.badServerResponse) }
        guard (200..<300).contains(http.statusCode) else {
            throw (try? JSONDecoder().decode(APIProblem.self, from: data)) ?? URLError(.badServerResponse)
        }
        let decoder = JSONDecoder()
        // Go's time.Time JSON includes fractional seconds for PostgreSQL
        // timestamps. Older supported iOS runtimes reject those with .iso8601.
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let value = try container.decode(String.self)
            if let date = try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(value) {
                return date
            }
            if let date = try? Date.ISO8601FormatStyle().parse(value) {
                return date
            }
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "Invalid RFC 3339 timestamp")
        }
        return try decoder.decode(Response.self, from: data)
    }
}

struct CreateUploadRequest: Encodable, Sendable {
    let clientAssetID: String
    let originalFilename: String
    let mediaType: String
    let expectedSize: Int64
    let clientSHA256: String

    enum CodingKeys: String, CodingKey {
        case clientAssetID = "client_asset_id"
        case originalFilename = "original_filename"
        case mediaType = "media_type"
        case expectedSize = "expected_size"
        case clientSHA256 = "client_sha256"
    }
}

private struct MFAChallengeResponse: Codable {
    let challenge: String
    let expiresIn: Int

    enum CodingKeys: String, CodingKey {
        case challenge
        case expiresIn = "expires_in"
    }
}

private struct CredentialResponse: Codable {
    let accessToken: String
    let expiresIn: Int
    let refreshToken: String
    let refreshExpiresIn: Int
    let userID: String

    enum CodingKeys: String, CodingKey {
        case accessToken = "access_token"
        case expiresIn = "expires_in"
        case refreshToken = "refresh_token"
        case refreshExpiresIn = "refresh_expires_in"
        case userID = "user_id"
    }

    var credential: Credential {
        Credential(
            accessToken: accessToken,
            accessExpiresAt: Date.now.addingTimeInterval(TimeInterval(expiresIn)),
            refreshToken: refreshToken,
            refreshExpiresAt: Date.now.addingTimeInterval(TimeInterval(refreshExpiresIn)),
            userID: userID
        )
    }
}

struct UploadIdentity: Sendable {
    let accessToken: String
    let userID: String
    fileprivate let generation: UUID
}

struct CredentialPersistence: Sendable {
    let load: @Sendable () throws -> Credential?
    let save: @Sendable (Credential) throws -> Void
    let delete: @Sendable () throws -> Void

    static let keychain = CredentialPersistence(
        load: { try KeychainStore.loadCredential() },
        save: { try KeychainStore.save($0) },
        delete: { try KeychainStore.deleteCredential() }
    )
}

actor AuthenticationStore {
    private let persistence: CredentialPersistence
    private var credential: Credential?
    private var refreshTask: Task<Credential, Error>?
    private var credentialGeneration = UUID()

    init(persistence: CredentialPersistence = .keychain) {
        self.persistence = persistence
    }

    func login(email: String, password: String, deviceName: String, api: PhotoCloudAPI) async throws -> LoginResult {
        try invalidateCredential()
        let generation = credentialGeneration
        let result = try await api.login(email: email, password: password, deviceName: deviceName)
        try requireGeneration(generation)
        if case .authenticated(let newCredential) = result {
            try persistence.save(newCredential)
            credential = newCredential
        }
        return result
    }

    func verifyMFA(challenge: String, totpCode: String?, recoveryCode: String?, api: PhotoCloudAPI) async throws {
        let generation = credentialGeneration
        let newCredential = try await api.verifyMFA(challenge: challenge, totpCode: totpCode, recoveryCode: recoveryCode)
        try requireGeneration(generation)
        try persistence.save(newCredential)
        credential = newCredential
    }

    private func requireGeneration(_ generation: UUID) throws {
        guard generation == credentialGeneration else { throw CancellationError() }
    }

    func invalidateCredential() throws {
        credentialGeneration = UUID()
        refreshTask?.cancel()
        refreshTask = nil
        credential = nil
        try persistence.delete()
    }

    func signOut(api: PhotoCloudAPI) async throws {
        let current = try credential ?? persistence.load()
        // Clear before the network await. A late refresh/login response must
        // never restore a signed-out account or overwrite a newer sign-in.
        try invalidateCredential()
        if let current { try await api.logout(refreshToken: current.refreshToken) }
    }

    func uploadIdentity(api: PhotoCloudAPI) async throws -> UploadIdentity {
        let generation = credentialGeneration
        let token = try await accessToken(api: api)
        try requireGeneration(generation)
        guard let current = try credential ?? persistence.load() else { throw CancellationError() }
        return UploadIdentity(accessToken: token, userID: current.userID, generation: generation)
    }

    func requireCurrent(_ identity: UploadIdentity) throws {
        try requireGeneration(identity.generation)
    }

    func accessToken(api: PhotoCloudAPI) async throws -> String {
        let generation = credentialGeneration
        let storedCredential = try credential ?? persistence.load()
        guard var current = storedCredential else { throw APIProblem(status: 401, code: "not_signed_in", detail: "Sign in before uploading.") }
        if current.accessExpiresAt > Date.now.addingTimeInterval(60) { return current.accessToken }
        guard current.refreshExpiresAt > .now else {
            try persistence.delete()
            credential = nil
            throw APIProblem(status: 401, code: "session_expired", detail: "Sign in again to continue uploads.")
        }
        if let refreshTask {
            current = try await refreshTask.value
        } else {
            // Persist the idempotency key before the network request. If the
            // response is lost (or the app is killed after the server commits),
            // the next attempt reuses the same ID with the same old token.
            let rotationRequestID = current.pendingRefreshRequestID ?? UUID().uuidString
            current.pendingRefreshRequestID = rotationRequestID
            try persistence.save(current)
            credential = current

            let refreshToken = current.refreshToken
            let task = Task { try await api.refresh(refreshToken, rotationRequestID: rotationRequestID) }
            refreshTask = task
            do {
                current = try await task.value
            } catch let problem as APIProblem where problem.status == 401 {
                try requireGeneration(generation)
                refreshTask = nil
                credential = nil
                try? persistence.delete()
                throw APIProblem(
                    status: 401,
                    code: "session_expired",
                    detail: "Your session is no longer valid. Sign in again to continue."
                )
            } catch {
                try requireGeneration(generation)
                refreshTask = nil
                throw error
            }
            try requireGeneration(generation)
            refreshTask = nil
        }
        try requireGeneration(generation)
        current.pendingRefreshRequestID = nil
        try persistence.save(current)
        credential = current
        return current.accessToken
    }
}
