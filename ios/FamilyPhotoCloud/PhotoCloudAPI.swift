import Foundation

struct Credential: Codable, Sendable {
    var accessToken: String
    var accessExpiresAt: Date
    var refreshToken: String
    var refreshExpiresAt: Date
    var userID: String
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

    func login(email: String, password: String, deviceName: String) async throws -> Credential {
        struct Request: Encodable { let email, password: String; let deviceName: String
            enum CodingKeys: String, CodingKey { case email, password; case deviceName = "device_name" }
        }
        return try await request(
            path: "/v1/auth/login",
            method: "POST",
            body: Request(email: email, password: password, deviceName: deviceName),
            bearer: nil,
            response: CredentialResponse.self
        ).credential
    }

    func refresh(_ refreshToken: String) async throws -> Credential {
        struct Request: Encodable { let refreshToken: String
            enum CodingKeys: String, CodingKey { case refreshToken = "refresh_token" }
        }
        return try await request(path: "/v1/auth/refresh", method: "POST", body: Request(refreshToken: refreshToken), bearer: nil, response: CredentialResponse.self).credential
    }

    func createUploadSession(_ request: CreateUploadRequest, accessToken: String) async throws -> UploadSession {
        try await request(path: "/v1/upload-sessions", method: "POST", body: request, bearer: accessToken, response: UploadSession.self)
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

    func absoluteURL(for relativePath: String) throws -> URL {
        guard let url = URL(string: relativePath, relativeTo: baseURL), url.host == baseURL.host, url.scheme == baseURL.scheme else {
            throw URLError(.badURL)
        }
        return url
    }

    private func request<Body: Encodable, Response: Decodable>(path: String, method: String, queryItems: [URLQueryItem] = [], body: Body?, bearer: String?, response: Response.Type) async throws -> Response {
        var components = URLComponents(url: baseURL.appending(path: path), resolvingAgainstBaseURL: false)
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
        decoder.dateDecodingStrategy = .iso8601
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

actor AuthenticationStore {
    private var credential: Credential?
    private var refreshTask: Task<Credential, Error>?

    func login(email: String, password: String, deviceName: String, api: PhotoCloudAPI) async throws {
        let newCredential = try await api.login(email: email, password: password, deviceName: deviceName)
        try KeychainStore.save(newCredential)
        credential = newCredential
    }

    func accessToken(api: PhotoCloudAPI) async throws -> String {
        var current = try credential ?? KeychainStore.loadCredential()
        guard var current else { throw APIProblem(status: 401, code: "not_signed_in", detail: "Sign in before uploading.") }
        if current.accessExpiresAt > Date.now.addingTimeInterval(60) { return current.accessToken }
        guard current.refreshExpiresAt > .now else {
            try KeychainStore.deleteCredential()
            credential = nil
            throw APIProblem(status: 401, code: "session_expired", detail: "Sign in again to continue uploads.")
        }
        if let refreshTask {
            current = try await refreshTask.value
        } else {
            let task = Task { try await api.refresh(current.refreshToken) }
            refreshTask = task
            do {
                current = try await task.value
            } catch {
                refreshTask = nil
                throw error
            }
            refreshTask = nil
        }
        try KeychainStore.save(current)
        credential = current
        return current.accessToken
    }
}
