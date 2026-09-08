import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class PhotoCloudAPIContractTests: XCTestCase {
    func testMediaURLsCannotSendAuthorizationToAnotherOriginPort() throws {
        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!)
        XCTAssertEqual(try api.absoluteURL(for: "/v1/assets/a/original").absoluteString,
                       "https://photos.example.test/v1/assets/a/original")
        XCTAssertNoThrow(try api.absoluteURL(for: "https://photos.example.test:443/v1/assets/a/original"))
        for path in ["https://photos.example.test:8443/original", "https://other.example.test/original",
                     "https://user:password@photos.example.test/original", "http://photos.example.test/original"] {
            XCTAssertThrowsError(try api.absoluteURL(for: path))
        }
    }

    func testConfigurationRejectsPlaceholderAndInconsistentAPIBasePaths() throws {
        XCTAssertNoThrow(try PhotoCloudAPI.configuredBaseURL("https://photos.family.test/"))
        for raw in ["https://photos.example.com", "https://", "http://photos.family.test",
                    "https://photos.family.test/api", "https://photos.family.test/?token=secret",
                    "https://user:secret@photos.family.test", "https://photos.family.test/#fragment"] {
            XCTAssertThrowsError(try PhotoCloudAPI.configuredBaseURL(raw), raw)
        }
    }

    func testLibraryAcceptsGoFractionalTimestampsAndWholeSeconds() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }
        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        for timestamp in ["2026-09-07T12:34:56Z", "2026-09-07T12:34:56.123456Z", "2026-09-07T14:34:56.123456789+02:00"] {
            URLProtocolStub.handler = { request in
                let response = try XCTUnwrap(HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: nil))
                let body = Data("""
                {"assets":[{"id":"asset-1","original_filename":"IMG.heic","media_type":"image/heic","byte_size":1,"content_sha256":"abc","created_at":"\(timestamp)","original_url":"/v1/assets/asset-1/original"}]}
                """.utf8)
                return (response, body)
            }
            let page = try await api.libraryPage(cursor: nil, accessToken: "test")
            XCTAssertEqual(page.assets.count, 1)
            XCTAssertEqual(page.assets[0].createdAt.timeIntervalSince1970,
                           try Date.ISO8601FormatStyle().parse("2026-09-07T12:34:56Z").timeIntervalSince1970,
                           accuracy: 1)
        }
    }

    func testDeviceSessionsAcceptsPostgresMicrosecondTimestamps() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }
        URLProtocolStub.handler = { request in
            let response = try XCTUnwrap(HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: nil))
            return (response, Data(#"{"sessions":[{"id":"device-1","device_name":"iPhone","created_at":"2026-09-07T12:34:56.123456Z","last_used_at":"2026-09-07T12:35:56.654321Z","expires_at":"2026-10-07T12:34:56.123456Z","current":true}]}"#.utf8))
        }
        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        let devices = try await api.deviceSessions(accessToken: "test")
        XCTAssertEqual(devices.count, 1)
        XCTAssertGreaterThan(devices[0].lastUsedAt, devices[0].createdAt)
    }

    func testLibraryRejectsMalformedTimestamp() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }
        URLProtocolStub.handler = { request in
            let response = try XCTUnwrap(HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: nil))
            return (response, Data(#"{"assets":[{"id":"asset-1","original_filename":"IMG.heic","media_type":"image/heic","byte_size":1,"content_sha256":"abc","created_at":"not-a-date","original_url":"/v1/assets/asset-1/original"}]}"#.utf8))
        }
        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        do {
            _ = try await api.libraryPage(cursor: nil, accessToken: "test")
            XCTFail("Malformed timestamps must not be silently accepted")
        } catch is DecodingError {
            // Expected: a malformed date remains a contract error.
        }
    }

    func testLateLoginResponseCannotRestoreInvalidatedCredentials() async throws {
        let memory = MemoryCredentials()
        let auth = AuthenticationStore(persistence: memory.persistence)
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        let gate = DeferredRequest()
        let started = expectation(description: "Login request suspended")
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.deferredHandler = nil
        }
        URLProtocolStub.deferredHandler = { stub in
            gate.store(stub)
            started.fulfill()
        }
        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        let pendingLogin = Task { try await auth.login(email: "test@example.test", password: "test", deviceName: "test", api: api) }
        await fulfillment(of: [started], timeout: 2)
        try await auth.invalidateCredential()
        try gate.complete(status: 200, body: #"{"access_token":"late-access","expires_in":900,"refresh_token":"late-refresh","refresh_expires_in":2592000,"user_id":"old-user"}"#)
        do {
            _ = try await pendingLogin.value
            XCTFail("Late login response must be discarded")
        } catch is CancellationError { }
        XCTAssertNil(memory.load())
    }

    func testSignOutClearsCredentialsBeforeRemoteLogoutCompletes() async throws {
        let memory = MemoryCredentials()
        memory.save(Credential(accessToken: "old-access", accessExpiresAt: .now.addingTimeInterval(900),
                               refreshToken: "old-refresh", refreshExpiresAt: .now.addingTimeInterval(3600), userID: "old-user"))
        let auth = AuthenticationStore(persistence: memory.persistence)
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        let gate = DeferredRequest()
        let started = expectation(description: "Remote logout suspended")
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.deferredHandler = nil
        }
        URLProtocolStub.deferredHandler = { stub in
            XCTAssertEqual(stub.request.url?.path, "/v1/auth/logout")
            gate.store(stub)
            started.fulfill()
        }
        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        let pendingLogout = Task { try await auth.signOut(api: api) }
        await fulfillment(of: [started], timeout: 2)
        XCTAssertNil(memory.load())
        do {
            _ = try await auth.accessToken(api: api)
            XCTFail("A pending logout must not leave a usable access token")
        } catch let problem as APIProblem {
            XCTAssertEqual(problem.code, "not_signed_in")
        }
        try gate.complete(status: 204, body: "")
        try await pendingLogout.value
    }

    func testMFAEnrollmentSendsPasswordJSONAndContentType() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }

        URLProtocolStub.handler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertEqual(request.url?.path, "/v1/auth/mfa/enroll")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer access-token")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")
            let requestBody = try requestBody(from: request)
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: requestBody) as? [String: String])
            XCTAssertEqual(object["password"], "current-password")

            let response = try XCTUnwrap(HTTPURLResponse(
                url: request.url!,
                statusCode: 200,
                httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "application/json"]
            ))
            let body = Data(#"{"secret":"ABC","otpauth_uri":"otpauth://totp/test"}"#.utf8)
            return (response, body)
        }

        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        let enrollment = try await api.beginMFAEnrollment(password: "current-password", accessToken: "access-token")
        XCTAssertEqual(enrollment.secret, "ABC")
        XCTAssertEqual(enrollment.otpauthURI, "otpauth://totp/test")
    }
    func testRefreshSendsStableRotationRequestID() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }

        URLProtocolStub.handler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertEqual(request.url?.path, "/v1/auth/refresh")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")
            let requestBody = try requestBody(from: request)
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: requestBody) as? [String: String])
            XCTAssertEqual(object["refresh_token"], "old-refresh")
            XCTAssertEqual(object["rotation_request_id"], "11111111-2222-4333-8444-555555555555")

            let response = try XCTUnwrap(HTTPURLResponse(
                url: request.url!,
                statusCode: 200,
                httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "application/json"]
            ))
            let body = Data(#"{"access_token":"access","token_type":"Bearer","expires_in":900,"refresh_token":"new-refresh","refresh_expires_in":2592000,"user_id":"10000000-0000-4000-8000-000000000001"}"#.utf8)
            return (response, body)
        }

        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        let credential = try await api.refresh(
            "old-refresh",
            rotationRequestID: "11111111-2222-4333-8444-555555555555"
        )
        XCTAssertEqual(credential.refreshToken, "new-refresh")
    }


    func testDeviceSessionsMarksCurrentSessionFromAPI() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }

        URLProtocolStub.handler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertEqual(request.url?.path, "/v1/auth/sessions")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer access-token")
            let response = try XCTUnwrap(HTTPURLResponse(
                url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1",
                headerFields: ["Content-Type": "application/json"]
            ))
            let body = Data(#"{"sessions":[{"id":"11111111-2222-4333-8444-555555555555","device_name":"iPhone","created_at":"2026-08-26T00:00:00Z","last_used_at":"2026-08-26T00:01:00Z","expires_at":"2026-09-25T00:00:00Z","current":true}]}"#.utf8)
            return (response, body)
        }

        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        let sessions = try await api.deviceSessions(accessToken: "access-token")
        XCTAssertEqual(sessions.count, 1)
        XCTAssertEqual(sessions[0].deviceName, "iPhone")
        XCTAssertTrue(sessions[0].current)
    }

    func testRevokeDeviceSessionUsesAuthenticatedDelete() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }

        URLProtocolStub.handler = { request in
            XCTAssertEqual(request.httpMethod, "DELETE")
            XCTAssertEqual(request.url?.path, "/v1/auth/sessions/11111111-2222-4333-8444-555555555555")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer access-token")
            let response = try XCTUnwrap(HTTPURLResponse(
                url: request.url!, statusCode: 204, httpVersion: "HTTP/1.1", headerFields: nil
            ))
            return (response, Data())
        }

        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        try await api.revokeDeviceSession(
            id: "11111111-2222-4333-8444-555555555555",
            accessToken: "access-token"
        )
    }

    func testLogoutSendsRefreshTokenAndClearsServerSessionEndpoint() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [URLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        defer {
            session.invalidateAndCancel()
            URLProtocolStub.handler = nil
        }

        URLProtocolStub.handler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertEqual(request.url?.path, "/v1/auth/logout")
            XCTAssertNil(request.value(forHTTPHeaderField: "Authorization"))
            XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")
            let body = try requestBody(from: request)
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: body) as? [String: String])
            XCTAssertEqual(object["refresh_token"], "refresh-token")
            let response = try XCTUnwrap(HTTPURLResponse(
                url: request.url!, statusCode: 204, httpVersion: "HTTP/1.1", headerFields: nil
            ))
            return (response, Data())
        }

        let api = PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
        try await api.logout(refreshToken: "refresh-token")
    }
}

private func requestBody(from request: URLRequest) throws -> Data {
    if let body = request.httpBody {
        return body
    }
    guard let stream = request.httpBodyStream else {
        throw URLError(.badServerResponse)
    }
    stream.open()
    defer { stream.close() }
    var result = Data()
    var buffer = [UInt8](repeating: 0, count: 4_096)
    while stream.hasBytesAvailable {
        let count = stream.read(&buffer, maxLength: buffer.count)
        if count < 0 {
            throw stream.streamError ?? URLError(.cannotDecodeContentData)
        }
        if count == 0 {
            break
        }
        result.append(buffer, count: count)
    }
    return result
}

private final class URLProtocolStub: URLProtocol {
    nonisolated(unsafe) static var deferredHandler: ((URLProtocolStub) -> Void)?
    nonisolated(unsafe) static var handler: ((URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        if let deferredHandler = Self.deferredHandler {
            deferredHandler(self)
            return
        }
        guard let handler = Self.handler else {
            client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
            return
        }
        do {
            let (response, data) = try handler(request)
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

private final class DeferredRequest: @unchecked Sendable {
    private let lock = NSLock()
    private var pending: URLProtocolStub?

    func store(_ request: URLProtocolStub) {
        lock.lock()
        pending = request
        lock.unlock()
    }

    func complete(status: Int, body: String) throws {
        lock.lock()
        let stub = pending
        pending = nil
        lock.unlock()
        let request = try XCTUnwrap(stub)
        let response = try XCTUnwrap(HTTPURLResponse(url: request.request.url!, statusCode: status, httpVersion: nil, headerFields: nil))
        request.client?.urlProtocol(request, didReceive: response, cacheStoragePolicy: .notAllowed)
        request.client?.urlProtocol(request, didLoad: Data(body.utf8))
        request.client?.urlProtocolDidFinishLoading(request)
    }
}

private final class MemoryCredentials: @unchecked Sendable {
    private let lock = NSLock()
    private var credential: Credential?

    var persistence: CredentialPersistence {
        CredentialPersistence(load: { self.load() }, save: { self.save($0) }, delete: { self.save(nil) })
    }

    func load() -> Credential? {
        lock.lock()
        defer { lock.unlock() }
        return credential
    }

    func save(_ value: Credential?) {
        lock.lock()
        credential = value
        lock.unlock()
    }
}
