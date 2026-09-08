import Foundation
import XCTest
@testable import FamilyPhotoCloud

final class DiagnosticErrorSummaryTests: XCTestCase {
    func testExportNeverIncludesNSErrorDescriptionsURLsOrNestedSecrets() throws {
        let error = NSError(domain: "https://private.example/?key=secret", code: 17, userInfo: [
            NSLocalizedDescriptionKey: "IMG_private.heic belonging to parent@example.com",
            NSURLErrorFailingURLStringErrorKey: "https://private.example/?token=secret",
            NSUnderlyingErrorKey: NSError(domain: "secret", code: 9)
        ])
        let summary = try XCTUnwrap(DiagnosticErrorSummary.describe(error))
        XCTAssertTrue(summary.contains("17"))
        for sensitive in ["private", "secret", "IMG_", "parent@", "https://"] {
            XCTAssertFalse(summary.contains(sensitive))
        }
    }

    func testServerProblemDetailIsNotExported() throws {
        let error = APIProblem(status: 500, code: "internal_error", detail: "private filename and Bearer secret")
        let summary = try XCTUnwrap(DiagnosticErrorSummary.describe(error))
        XCTAssertFalse(summary.contains("private"))
        XCTAssertFalse(summary.contains("Bearer"))
        XCTAssertNil(DiagnosticErrorSummary.describe(nil))
    }
}
