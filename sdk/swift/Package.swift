// swift-tools-version: 6.0

import PackageDescription

let package = Package(
    name: "NaatreSDK",
    platforms: [
        .iOS(.v17),
        .macOS(.v14),
        .tvOS(.v17),
        .watchOS(.v10),
        .visionOS(.v1),
    ],
    products: [
        .library(name: "NaatreCore", targets: ["NaatreCore"]),
        .library(name: "NaatreGenerated", targets: ["NaatreGenerated"]),
        .library(name: "NaatreApple", targets: ["NaatreApple"]),
        .executable(name: "naatre-swift-conformance", targets: ["NaatreConformance"]),
        .executable(name: "naatre-swift-apple-conformance", targets: ["NaatreAppleConformance"]),
    ],
    targets: [
        .target(name: "NaatreCore"),
        .target(name: "NaatreGenerated", dependencies: ["NaatreCore"], exclude: ["operations.json"]),
        .target(name: "NaatreApple", dependencies: ["NaatreCore"]),
        .executableTarget(name: "NaatreConformance", dependencies: ["NaatreCore", "NaatreGenerated"]),
        .executableTarget(name: "NaatreAppleConformance", dependencies: ["NaatreApple", "NaatreGenerated"]),
    ]
)
