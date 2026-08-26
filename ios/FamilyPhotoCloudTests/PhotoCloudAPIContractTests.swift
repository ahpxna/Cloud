import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class PhotoCloudAPIContractTests: XCTestCase {
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
            let body = try XCTUnwrap(request.httpBody)
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
    nonisolated(unsafe) static var handler: ((URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
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
