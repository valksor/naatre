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
        .executable(name: "naatre-swift-conformance", targets: ["NaatreConformance"]),
    ],
    targets: [
        .target(name: "NaatreCore"),
        .target(name: "NaatreGenerated", dependencies: ["NaatreCore"], exclude: ["operations.json"]),
        .executableTarget(name: "NaatreConformance", dependencies: ["NaatreCore", "NaatreGenerated"]),
    ]
)
