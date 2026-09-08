import Foundation

/// Error descriptions may contain filenames, full URLs, query credentials or
/// an underlying server response. Export only the stable class and numeric code.
enum DiagnosticErrorSummary {
    static func describe(_ error: Error?) -> String? {
        guard let error else { return nil }
        let kind = String(reflecting: type(of: error))
        let code = (error as NSError).code
        return "\(kind) (code \(code))"
    }
}
