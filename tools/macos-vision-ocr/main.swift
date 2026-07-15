import CoreGraphics
import Foundation
import ImageIO
import Vision

struct TextObservation: Codable {
    let text: String
    let confidence: Float
    let x: Double
    let y: Double
    let width: Double
    let height: Double
}

struct OCRResult: Codable {
    let text: String
    let observations: [TextObservation]
    let elapsed_ms: Int
}

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

guard CommandLine.arguments.count >= 2 else {
    fail("usage: macos-vision-ocr <image-path> [language1,language2]")
}

let imageURL = URL(fileURLWithPath: CommandLine.arguments[1])
guard let source = CGImageSourceCreateWithURL(imageURL as CFURL, nil) else {
    fail("cannot decode image: \(imageURL.path)")
}
let thumbnailOptions: [CFString: Any] = [
    kCGImageSourceCreateThumbnailFromImageAlways: true,
    kCGImageSourceCreateThumbnailWithTransform: true,
    kCGImageSourceThumbnailMaxPixelSize: 2400,
]
guard let image = CGImageSourceCreateThumbnailAtIndex(source, 0, thumbnailOptions as CFDictionary) else {
    fail("cannot create OCR image: \(imageURL.path)")
}

let requestedLanguages = CommandLine.arguments.count >= 3
    ? CommandLine.arguments[2].split(separator: ",").map { String($0).trimmingCharacters(in: .whitespaces) }
    : ["zh-Hans", "en-US"]

let startedAt = Date()
let request = VNRecognizeTextRequest()
request.recognitionLevel = .accurate
request.usesLanguageCorrection = true
request.minimumTextHeight = 0.01

do {
    let supported = try request.supportedRecognitionLanguages()
    let selected = requestedLanguages.filter { supported.contains($0) }
    if !selected.isEmpty {
        request.recognitionLanguages = selected
    }
} catch {
    // Vision can still use its default language set when language discovery fails.
}

do {
    try VNImageRequestHandler(cgImage: image, options: [:]).perform([request])
} catch {
    fail("vision request failed: \(error.localizedDescription)")
}

let observations = (request.results ?? []).compactMap { observation -> TextObservation? in
    guard let candidate = observation.topCandidates(1).first else { return nil }
    let box = observation.boundingBox
    return TextObservation(
        text: candidate.string,
        confidence: candidate.confidence,
        x: box.origin.x,
        y: box.origin.y,
        width: box.size.width,
        height: box.size.height
    )
}.sorted { lhs, rhs in
    let lhsTop = lhs.y + lhs.height
    let rhsTop = rhs.y + rhs.height
    if abs(lhsTop - rhsTop) > 0.015 {
        return lhsTop > rhsTop
    }
    return lhs.x < rhs.x
}

let result = OCRResult(
    text: observations.map(\.text).joined(separator: "\n"),
    observations: observations,
    elapsed_ms: Int(Date().timeIntervalSince(startedAt) * 1000)
)

do {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.withoutEscapingSlashes]
    FileHandle.standardOutput.write(try encoder.encode(result))
    FileHandle.standardOutput.write(Data("\n".utf8))
} catch {
    fail("cannot encode result: \(error.localizedDescription)")
}
