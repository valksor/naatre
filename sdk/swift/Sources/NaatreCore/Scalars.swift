import Foundation

public protocol CanonicalStringScalar: Codable, Hashable, Sendable {
    var wireValue: String { get }
    init(validating wireValue: String) throws
}

extension CanonicalStringScalar {
    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        try self.init(validating: container.decode(String.self))
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(wireValue)
    }
}

public struct NaatreInt64: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        guard let value = Int64(wireValue), String(value) == normalizeInteger(wireValue) else {
            throw ClientFailure(.scalarInvalid)
        }
        self.wireValue = String(value)
    }

    public init(_ value: Int64) { wireValue = String(value) }
    public var value: Int64 { Int64(wireValue)! }
}

public struct NaatreUInt64: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        guard let value = UInt64(wireValue), String(value) == normalizeUnsignedInteger(wireValue) else {
            throw ClientFailure(.scalarInvalid)
        }
        self.wireValue = String(value)
    }

    public init(_ value: UInt64) { wireValue = String(value) }
    public var value: UInt64 { UInt64(wireValue)! }
}

public struct NaatreBigInt: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        self.wireValue = try canonicalInteger(wireValue)
    }
}

public struct NaatreDecimal: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        self.wireValue = try canonicalDecimal(wireValue)
    }

    public init(_ value: Decimal) throws {
        let locale = Locale(identifier: "en_US_POSIX")
        try self.init(validating: NSDecimalNumber(decimal: value).description(withLocale: locale))
    }

    public var decimal: Decimal? { Decimal(string: wireValue, locale: Locale(identifier: "en_US_POSIX")) }
}

public struct NaatreDuration: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        guard let value = Int64(wireValue), String(value) == normalizeInteger(wireValue) else {
            throw ClientFailure(.scalarInvalid)
        }
        self.wireValue = String(value)
    }

    public init(nanoseconds: Int64) { wireValue = String(nanoseconds) }
    public var nanoseconds: Int64 { Int64(wireValue)! }
}

public struct NaatreUUID: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        let canonical = wireValue.lowercased()
        guard canonical.utf8.count == 36, UUID(uuidString: canonical)?.uuidString.lowercased() == canonical else {
            throw ClientFailure(.scalarInvalid)
        }
        self.wireValue = canonical
    }

    public init(_ value: UUID) { wireValue = value.uuidString.lowercased() }
    public var uuid: UUID { UUID(uuidString: wireValue)! }
}

public struct NaatreBytes: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        guard !wireValue.contains("="), wireValue.utf8.allSatisfy(isBase64URL), wireValue.count % 4 != 1 else {
            throw ClientFailure(.scalarInvalid)
        }
        var standard = wireValue.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        standard += String(repeating: "=", count: (4 - standard.count % 4) % 4)
        guard let decoded = Data(base64Encoded: standard), NaatreBytes(decoded).wireValue == wireValue else {
            throw ClientFailure(.scalarInvalid)
        }
        self.wireValue = wireValue
    }

    public init(_ value: Data) {
        wireValue = value.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }

    public var data: Data {
        var standard = wireValue.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        standard += String(repeating: "=", count: (4 - standard.count % 4) % 4)
        return Data(base64Encoded: standard)!
    }
}

public struct NaatreURL: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        guard let parsed = URL(string: wireValue), parsed.scheme != nil, parsed.absoluteString == wireValue else {
            throw ClientFailure(.scalarInvalid)
        }
        self.wireValue = wireValue
    }

    public init(_ value: URL) { wireValue = value.absoluteString }
    public var url: URL { URL(string: wireValue)! }
}

public struct NaatreTimestamp: CanonicalStringScalar {
    public let wireValue: String

    public init(validating wireValue: String) throws {
        self.wireValue = try canonicalTimestamp(wireValue)
    }

    public init(_ date: Date) throws {
        let seconds = date.timeIntervalSince1970
        guard seconds.isFinite else { throw ClientFailure(.valuePrecision) }
        let whole = floor(seconds)
        let nanos = Int64(((seconds - whole) * 1_000_000_000).rounded())
        var calendar = Calendar(identifier: .gregorian)
        calendar.locale = Locale(identifier: "en_US_POSIX")
        calendar.timeZone = TimeZone(secondsFromGMT: 0)!
        let components = calendar.dateComponents([.year, .month, .day, .hour, .minute, .second], from: Date(timeIntervalSince1970: whole))
        guard let year = components.year, let month = components.month, let day = components.day,
              let hour = components.hour, let minute = components.minute, let second = components.second else {
            throw ClientFailure(.valuePrecision)
        }
        let fraction = nanos == 0 ? "" : "." + String(format: "%09lld", locale: Locale(identifier: "en_US_POSIX"), nanos).trimmingCharacters(in: CharacterSet(charactersIn: "0"))
        try self.init(validating: String(format: "%04d-%02d-%02dT%02d:%02d:%02d%@Z", locale: Locale(identifier: "en_US_POSIX"), year, month, day, hour, minute, second, fraction))
    }

    public var date: Date? {
        guard !wireValue.hasPrefix("0000-") else { return nil }
        return ISO8601DateFormatter().date(from: wireValue)
    }
}

private func normalizeInteger(_ value: String) -> String {
    (try? canonicalInteger(value)) ?? ""
}

private func normalizeUnsignedInteger(_ value: String) -> String {
    guard !value.hasPrefix("-") else { return "" }
    return (try? canonicalInteger(value)) ?? ""
}

private func canonicalInteger(_ value: String) throws -> String {
    guard !value.isEmpty else { throw ClientFailure(.scalarInvalid) }
    let negative = value.first == "-"
    let digits = negative ? value.dropFirst() : Substring(value)
    guard !digits.isEmpty, digits.utf8.allSatisfy({ $0 >= 48 && $0 <= 57 }) else {
        throw ClientFailure(.scalarInvalid)
    }
    let stripped = digits.drop { $0 == "0" }
    let magnitude = stripped.isEmpty ? "0" : String(stripped)
    return negative && magnitude != "0" ? "-" + magnitude : magnitude
}

private func canonicalDecimal(_ value: String) throws -> String {
    guard !value.isEmpty else { throw ClientFailure(.scalarInvalid) }
    let negative = value.first == "-"
    let unsigned = negative ? String(value.dropFirst()) : value
    let components = unsigned.split(separator: ".", omittingEmptySubsequences: false)
    guard components.count <= 2, !components[0].isEmpty,
          components.allSatisfy({ !$0.isEmpty && $0.utf8.allSatisfy({ $0 >= 48 && $0 <= 57 }) }) else {
        throw ClientFailure(.scalarInvalid)
    }
    let integerDigits = components[0].drop { $0 == "0" }
    let integer = integerDigits.isEmpty ? "0" : String(integerDigits)
    let fraction = components.count == 2 ? String(String(components[1]).reversed().drop { $0 == "0" }.reversed()) : ""
    let magnitude = fraction.isEmpty ? integer : integer + "." + fraction
    return negative && magnitude != "0" ? "-" + magnitude : magnitude
}

private func isBase64URL(_ byte: UInt8) -> Bool {
    (byte >= 65 && byte <= 90) || (byte >= 97 && byte <= 122) || (byte >= 48 && byte <= 57) || byte == 45 || byte == 95
}

private func canonicalTimestamp(_ input: String) throws -> String {
    let pattern = #"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$"#
    let expression = try NSRegularExpression(pattern: pattern)
    let range = NSRange(input.startIndex..., in: input)
    guard let match = expression.firstMatch(in: input, range: range), match.range == range else {
        throw ClientFailure(.scalarInvalid)
    }
    func number(_ index: Int) -> Int {
        Int((input as NSString).substring(with: match.range(at: index)))!
    }
    var year = number(1), month = number(2), day = number(3)
    var hour = number(4)
    let minute = number(5), second = number(6)
    guard (1...12).contains(month), (1...daysInMonth(year, month)).contains(day),
          (0...23).contains(hour), (0...59).contains(minute), (0...59).contains(second) else {
        throw ClientFailure(.scalarInvalid)
    }
    let zone = (input as NSString).substring(with: match.range(at: 8))
    var offset = 0
    if zone != "Z" {
        let zoneHour = Int(zone.dropFirst().prefix(2))!
        let zoneMinute = Int(zone.suffix(2))!
        guard zoneHour <= 23, zoneMinute <= 59 else { throw ClientFailure(.scalarInvalid) }
        offset = (zone.first == "+" ? 1 : -1) * (zoneHour * 60 + zoneMinute)
    }
    var utcMinutes = hour * 60 + minute - offset
    if utcMinutes < 0 {
        (year, month, day) = previousDay(year, month, day)
        utcMinutes += 1440
    } else if utcMinutes >= 1440 {
        (year, month, day) = nextDay(year, month, day)
        utcMinutes -= 1440
    }
    guard (0...9999).contains(year) else { throw ClientFailure(.scalarInvalid) }
    hour = utcMinutes / 60
    let utcMinute = utcMinutes % 60
    var fraction = ""
    if match.range(at: 7).location != NSNotFound {
        fraction = (input as NSString).substring(with: match.range(at: 7))
        while fraction.last == "0" { fraction.removeLast() }
    }
    return String(format: "%04d-%02d-%02dT%02d:%02d:%02d%@Z", locale: Locale(identifier: "en_US_POSIX"), year, month, day, hour, utcMinute, second, fraction.isEmpty ? "" : "." + fraction)
}

private func daysInMonth(_ year: Int, _ month: Int) -> Int {
    switch month {
    case 2:
        let divisibleBy4 = year.isMultiple(of: 4)
        let ordinaryCentury = year.isMultiple(of: 100) && !year.isMultiple(of: 400)
        return divisibleBy4 && !ordinaryCentury ? 29 : 28
    case 4, 6, 9, 11:
        return 30
    default:
        return 31
    }
}

private func previousDay(_ year: Int, _ month: Int, _ day: Int) -> (Int, Int, Int) {
    if day > 1 { return (year, month, day - 1) }
    if month > 1 { return (year, month - 1, daysInMonth(year, month - 1)) }
    return (year - 1, 12, 31)
}

private func nextDay(_ year: Int, _ month: Int, _ day: Int) -> (Int, Int, Int) {
    if day < daysInMonth(year, month) { return (year, month, day + 1) }
    if month < 12 { return (year, month + 1, 1) }
    return (year + 1, 1, 1)
}
