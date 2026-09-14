import Foundation

public enum Input<Value: Codable & Sendable>: Codable, Sendable {
    case missing
    case null
    case value(Value)

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        self = container.decodeNil() ? .null : .value(try container.decode(Value.self))
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .missing:
            throw EncodingError.invalidValue(self, .init(codingPath: encoder.codingPath, debugDescription: "missing input requires a keyed container"))
        case .null:
            try container.encodeNil()
        case let .value(value):
            try container.encode(value)
        }
    }

    public static func decode<Key: CodingKey>(
        from container: KeyedDecodingContainer<Key>,
        forKey key: Key,
        allowNull: Bool
    ) throws -> Input<Value> {
        guard container.contains(key) else { return .missing }
        if try container.decodeNil(forKey: key) {
            guard allowNull else { throw ClientFailure(.protocolInvalid) }
            return .null
        }
        return .value(try container.decode(Value.self, forKey: key))
    }
}

public enum Selected<Value: Codable & Sendable>: Sendable {
    case missing
    case null
    case pending
    case value(Value)
    case failed([OperationError])
    case skipped(String)

    public static func decode<Key: CodingKey>(
        from container: KeyedDecodingContainer<Key>,
        forKey key: Key,
        pendingWhenMissing: Bool = false
    ) throws -> Selected<Value> {
        guard container.contains(key) else { return pendingWhenMissing ? .pending : .missing }
        if try container.decodeNil(forKey: key) { return .null }
        return .value(try container.decode(Value.self, forKey: key))
    }

    public func encode<Key: CodingKey>(to container: inout KeyedEncodingContainer<Key>, forKey key: Key) throws {
        switch self {
        case .null: try container.encodeNil(forKey: key)
        case let .value(value): try container.encode(value, forKey: key)
        case .missing, .pending, .failed, .skipped: break
        }
    }
}

extension Selected: Equatable where Value: Equatable {}

public enum JSONValue: Codable, Equatable, Sendable {
    case null
    case boolean(Bool)
    case number(Decimal)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null }
        else if let value = try? container.decode(Bool.self) { self = .boolean(value) }
        else if let value = try? container.decode(Decimal.self) { self = .number(value) }
        else if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode([JSONValue].self) { self = .array(value) }
        else if let value = try? container.decode([String: JSONValue].self) { self = .object(value) }
        else { throw ClientFailure(.protocolInvalid) }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .null: try container.encodeNil()
        case let .boolean(value): try container.encode(value)
        case let .number(value): try container.encode(value)
        case let .string(value): try container.encode(value)
        case let .array(value): try container.encode(value)
        case let .object(value): try container.encode(value)
        }
    }
}

public struct OperationError: Codable, Equatable, Sendable {
    public enum PathComponent: Codable, Equatable, Sendable {
        case field(String)
        case index(Int)

        public init(from decoder: Decoder) throws {
            let container = try decoder.singleValueContainer()
            if let field = try? container.decode(String.self) { self = .field(field) }
            else if let index = try? container.decode(Int.self) { self = .index(index) }
            else { throw ClientFailure(.protocolInvalid) }
        }

        public func encode(to encoder: Encoder) throws {
            var container = encoder.singleValueContainer()
            switch self {
            case let .field(value): try container.encode(value)
            case let .index(value): try container.encode(value)
            }
        }
    }

    public let code: String
    public let message: String?
    public let path: [PathComponent]
    public let extensions: [String: JSONValue]

    enum CodingKeys: String, CodingKey { case code, message, path, extensions }

    public init(code: String, message: String? = nil, path: [PathComponent] = [], extensions: [String: JSONValue] = [:]) {
        self.code = code
        self.message = message
        self.path = path
        self.extensions = extensions
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        code = try container.decode(String.self, forKey: .code)
        message = try container.decodeIfPresent(String.self, forKey: .message)
        path = try container.decodeIfPresent([PathComponent].self, forKey: .path) ?? []
        extensions = try container.decodeIfPresent([String: JSONValue].self, forKey: .extensions) ?? [:]
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(code, forKey: .code)
        try container.encodeIfPresent(message, forKey: .message)
        if !path.isEmpty { try container.encode(path, forKey: .path) }
        if !extensions.isEmpty { try container.encode(extensions, forKey: .extensions) }
    }
}

public struct OperationResult<Value: Codable & Sendable>: Codable, Sendable {
    public let data: Selected<Value>
    public let errors: [OperationError]
    public let complete: Bool

    enum CodingKeys: String, CodingKey, CaseIterable { case data, errors, complete }

    public init(data: Selected<Value>, errors: [OperationError], complete: Bool) {
        self.data = data
        self.errors = errors
        self.complete = complete
    }

    public init(from decoder: Decoder) throws {
        let dynamic = try decoder.container(keyedBy: AnyCodingKey.self)
        let allowed = Set(CodingKeys.allCases.map(\.rawValue))
        guard dynamic.allKeys.allSatisfy({ allowed.contains($0.stringValue) }) else {
            throw ClientFailure(.protocolInvalid)
        }
        let container = try decoder.container(keyedBy: CodingKeys.self)
        guard container.contains(.complete) else { throw ClientFailure(.protocolInvalid) }
        complete = try container.decode(Bool.self, forKey: .complete)
        errors = try container.decodeIfPresent([OperationError].self, forKey: .errors) ?? []
        data = try Selected<Value>.decode(from: container, forKey: .data)
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try data.encode(to: &container, forKey: .data)
        if !errors.isEmpty { try container.encode(errors, forKey: .errors) }
        try container.encode(complete, forKey: .complete)
    }
}

public struct OpenVariant: Codable, Equatable, Sendable {
    public let discriminator: String
    public let value: JSONValue

    enum CodingKeys: String, CodingKey { case discriminator = "$type", value = "$value" }

    public init(discriminator: String, value: JSONValue) {
        self.discriminator = discriminator
        self.value = value
    }
}

private struct AnyCodingKey: CodingKey {
    let stringValue: String
    let intValue: Int?
    init?(stringValue: String) { self.stringValue = stringValue; intValue = nil }
    init?(intValue: Int) { self.intValue = intValue; stringValue = String(intValue) }
}
