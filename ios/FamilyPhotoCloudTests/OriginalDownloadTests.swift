import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class OriginalDownloadTests: XCTestCase {
    override func tearDown() {
        RangeURLProtocolStub.handler = nil
        super.tearDown()
    }

    func testOriginalDownloadResumesRangeSegmentAfterTransientFailure() async throws {
        let original = Data((0..<10).map { UInt8($0) })
        let log = RequestLog()
        RangeURLProtocolStub.handler = { request in
            let range = try XCTUnwrap(request.value(forHTTPHeaderField: "Range"))
            XCTAssertEqual(request.value(forHTTPHeaderField: "If-Range"), "\"sha256-abc123\"")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer token-\(log.count + 1)")
            // Request 2 is the first attempt at the second segment; it loses
            // the connection and must be retried with the same range.
            if log.record(range) == 2 {
                throw URLError(.networkConnectionLost)
            }
            let bounds = range.dropFirst("bytes=".count).split(separator: "-").compactMap { Int($0) }
            XCTAssertEqual(bounds.count, 2)
            let response = try XCTUnwrap(HTTPURLResponse(
                url: request.url!, statusCode: 206, httpVersion: "HTTP/1.1",
                headerFields: ["Content-Range": "bytes \(bounds[0])-\(bounds[1])/\(original.count)"]
            ))
            return (response, original.subdata(in: bounds[0]..<(bounds[1] + 1)))
        }
        let tokens = TokenSequence()
        let url = try await makeAPI().downloadOriginal(
            asset(byteSize: original.count),
            accessToken: { tokens.next() },
            segmentBytes: 4,
            retryDelay: .zero
        )
        defer { try? FileManager.default.removeItem(at: url) }

        XCTAssertEqual(try Data(contentsOf: url), original)
        XCTAssertEqual(log.ranges, ["bytes=0-3", "bytes=4-7", "bytes=4-7", "bytes=8-9"])
    }

    func testOriginalDownloadRejectsTruncatedFullBody() async throws {
        RangeURLProtocolStub.handler = { request in
            let response = try XCTUnwrap(HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: "HTTP/1.1", headerFields: nil))
            return (response, Data(count: 4))
        }
        do {
            _ = try await makeAPI().downloadOriginal(
                asset(byteSize: 10), accessToken: { "token" }, segmentBytes: 4, retryDelay: .zero
            )
            XCTFail("a short 200 response must not be accepted as the whole original")
        } catch {
            // Expected: a 200 body shorter than the asset is never accepted.
        }
    }

    private func makeAPI() -> PhotoCloudAPI {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [RangeURLProtocolStub.self]
        let session = URLSession(configuration: configuration)
        addTeardownBlock { session.invalidateAndCancel() }
        return PhotoCloudAPI(baseURL: URL(string: "https://photos.example.test")!, session: session)
    }

    private func asset(byteSize: Int) -> LibraryAsset {
        LibraryAsset(
            id: "asset-1",
            originalFilename: "clip.mov",
            mediaType: "video/quicktime",
            byteSize: Int64(byteSize),
            contentSHA256: "ABC123",
            createdAt: .now,
            originalURL: "/v1/assets/asset-1/original"
        )
    }
}

private final class RequestLog: @unchecked Sendable {
    private let lock = NSLock()
    private var values: [String] = []

    var ranges: [String] {
        lock.lock()
        defer { lock.unlock() }
        return values
    }

    var count: Int { ranges.count }

    func record(_ range: String) -> Int {
        lock.lock()
        defer { lock.unlock() }
        values.append(range)
        return values.count
    }
}

private final class TokenSequence: @unchecked Sendable {
    private let lock = NSLock()
    private var issued = 0

    func next() -> String {
        lock.lock()
        defer { lock.unlock() }
        issued += 1
        return "token-\(issued)"
    }
}

private final class RangeURLProtocolStub: URLProtocol {
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
