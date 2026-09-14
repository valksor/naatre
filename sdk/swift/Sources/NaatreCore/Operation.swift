import Foundation

public enum OperationKind: String, Codable, Sendable {
    case query
    case mutation
    case subscription
}

public struct PersistedReference: Codable, Equatable, Sendable {
    public let algorithm: String
    public let canonicalVersion: String
    public let digest: String

    public init(algorithm: String = "sha-256", canonicalVersion: String = "c14n-1", digest: String) throws {
        guard algorithm == "sha-256", canonicalVersion == "c14n-1", digest.count == 64,
              digest.utf8.allSatisfy({ ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102) }) else {
            throw ClientFailure(.protocolInvalid)
        }
        self.algorithm = algorithm
        self.canonicalVersion = canonicalVersion
        self.digest = digest
    }
}

public struct Operation<Variables: Encodable & Sendable, Result: Codable & Sendable>: Sendable {
    public let name: String
    public let kind: OperationKind
    public let persisted: PersistedReference
    public let variables: Variables

    public init(name: String, kind: OperationKind, persisted: PersistedReference, variables: Variables) throws {
        guard name.range(of: #"^[A-Za-z_][A-Za-z0-9_]{0,127}$"#, options: .regularExpression) != nil else {
            throw ClientFailure(.protocolInvalid)
        }
        self.name = name
        self.kind = kind
        self.persisted = persisted
        self.variables = variables
    }

    public func canonicalRequest() throws -> Data {
        let request = Request(version: "1", operation: name, persisted: persisted, variables: variables)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return try encoder.encode(request)
    }

    public func decodeResult(_ data: Data) throws -> OperationResult<Result> {
        do { return try JSONDecoder().decode(OperationResult<Result>.self, from: data) }
        catch let failure as ClientFailure { throw failure }
        catch { throw ClientFailure(.resultInvalid) }
    }

    private struct Request: Encodable {
        let version: String
        let operation: String
        let persisted: PersistedReference
        let variables: Variables
    }
}

public struct ManifestOperation: Codable, Equatable, Sendable {
    public let name: String
    public let kind: OperationKind
    public let persisted: PersistedReference

    public init(name: String, kind: OperationKind, persisted: PersistedReference) {
        self.name = name
        self.kind = kind
        self.persisted = persisted
    }
}

public struct Manifest: Codable, Equatable, Sendable {
    public let profile: String
    public let version: String
    public let protocolVersion: String
    public let canonicalVersion: String
    public let operations: [ManifestOperation]

    public init(profile: String = "sdk.swift.core-1", operations: [ManifestOperation]) throws {
        guard !operations.isEmpty, Set(operations.map(\.name)).count == operations.count else {
            throw ClientFailure(.protocolInvalid)
        }
        self.profile = profile
        version = "1"
        protocolVersion = "1"
        canonicalVersion = "c14n-1"
        self.operations = operations.sorted { $0.name < $1.name }
    }
}
