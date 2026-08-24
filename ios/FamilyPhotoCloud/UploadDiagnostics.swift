import Foundation
import UIKit

@MainActor
final class UploadDiagnostics {
    static let shared = UploadDiagnostics()
    private static let maxLogBytes: UInt64 = 5 * 1024 * 1024

    private struct Event: Encodable {
        let timestamp: Date
        let event: String
        let queueID: String?
        let sessionID: String?
        let tusUploadID: String?
        let bytesUploaded: Int?
        let totalBytes: Int?
        let appState: String
        let error: String?

        enum CodingKeys: String, CodingKey {
            case timestamp, event, error
            case queueID = "queue_id"
            case sessionID = "session_id"
            case tusUploadID = "tus_upload_id"
            case bytesUploaded = "bytes_uploaded"
            case totalBytes = "total_bytes"
            case appState = "app_state"
        }
    }

    private let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }()

    private init() {}

    func record(
        _ event: String,
        context: [String: String]? = nil,
        tusUploadID: UUID? = nil,
        bytesUploaded: Int? = nil,
        totalBytes: Int? = nil,
        error: Error? = nil
    ) {
        let record = Event(
            timestamp: .now,
            event: event,
            queueID: context?["queue_id"],
            sessionID: context?["session_id"],
            tusUploadID: tusUploadID?.uuidString,
            bytesUploaded: bytesUploaded,
            totalBytes: totalBytes,
            appState: Self.appStateName(UIApplication.shared.applicationState),
            error: Self.safeErrorDescription(error)
        )
        do {
            let directory = try AppGroupQueue.diagnosticsDirectory()
            let url = directory.appending(path: "upload-events.jsonl")
            var data = try encoder.encode(record)
            data.append(0x0A)
            try Self.rotateIfNeeded(url, incomingBytes: UInt64(data.count))
            if !FileManager.default.fileExists(atPath: url.path()) {
                try data.write(to: url, options: [.atomic])
            } else {
                let handle = try FileHandle(forWritingTo: url)
                defer { try? handle.close() }
                try handle.seekToEnd()
                try handle.write(contentsOf: data)
                try handle.synchronize()
            }
        } catch {
            // Diagnostics must never break or stall the upload state machine.
        }
    }

    func exportURL() throws -> URL {
        let directory = try AppGroupQueue.diagnosticsDirectory()
        let source = directory.appending(path: "upload-events.jsonl")
        if !FileManager.default.fileExists(atPath: source.path()) {
            try Data().write(to: source, options: [.atomic])
        }
        let export = FileManager.default.temporaryDirectory
            .appending(path: "family-photo-cloud-upload-diagnostics-\(UUID().uuidString).jsonl")
        FileManager.default.createFile(atPath: export.path(), contents: nil)
        let output = try FileHandle(forWritingTo: export)
        defer { try? output.close() }
        let rotated = source.appendingPathExtension("1")
        for candidate in [rotated, source] where FileManager.default.fileExists(atPath: candidate.path()) {
            let input = try FileHandle(forReadingFrom: candidate)
            defer { try? input.close() }
            while let chunk = try input.read(upToCount: 256 * 1024), !chunk.isEmpty {
                try output.write(contentsOf: chunk)
            }
        }
        try output.synchronize()
        return export
    }

    private static func rotateIfNeeded(_ url: URL, incomingBytes: UInt64) throws {
        guard FileManager.default.fileExists(atPath: url.path()) else { return }
        let attributes = try FileManager.default.attributesOfItem(atPath: url.path())
        let current = (attributes[.size] as? NSNumber)?.uint64Value ?? 0
        guard current + incomingBytes > maxLogBytes else { return }
        let rotated = url.appendingPathExtension("1")
        if FileManager.default.fileExists(atPath: rotated.path()) {
            try FileManager.default.removeItem(at: rotated)
        }
        try FileManager.default.moveItem(at: url, to: rotated)
    }

    private static func safeErrorDescription(_ error: Error?) -> String? {
        guard let error else { return nil }
        var value = error.localizedDescription
        let patterns = [
            #"(?i)Bearer\s+[A-Za-z0-9._~+/=-]+"#,
            #"eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+"#
        ]
        for pattern in patterns {
            guard let regex = try? NSRegularExpression(pattern: pattern) else { continue }
            let range = NSRange(value.startIndex..<value.endIndex, in: value)
            value = regex.stringByReplacingMatches(in: value, range: range, withTemplate: "[REDACTED]")
        }
        if value.count > 512 {
            value = String(value.prefix(512)) + "…"
        }
        return value
    }

    private static func appStateName(_ state: UIApplication.State) -> String {
        switch state {
        case .active: return "active"
        case .inactive: return "inactive"
        case .background: return "background"
        @unknown default: return "unknown"
        }
    }
}
