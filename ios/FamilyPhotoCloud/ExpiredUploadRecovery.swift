import Foundation

/// Retire the obsolete transfer before publishing a resumable fresh session.
/// A failed cancellation or queue write must leave the caller's record intact.
enum ExpiredUploadRecovery {
    static func reset(
        _ original: QueuedUpload,
        discardTransfer: (QueuedUpload) throws -> Void,
        persist: (QueuedUpload) throws -> Void
    ) throws -> QueuedUpload {
        try discardTransfer(original)
        var item = original
        item.serverSessionID = nil
        item.tusUploadID = nil
        item.state = .queued
        item.lastError = nil
        try persist(item)
        return item
    }
}
