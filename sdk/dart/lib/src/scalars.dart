import 'dart:convert';
import 'dart:typed_data';

import 'error.dart';
import 'json.dart';

final class ExtendedInteger implements WireValue {
  ExtendedInteger._(this.value);

  factory ExtendedInteger.bigInt(Object value) =>
      ExtendedInteger._(_integer(value));

  factory ExtendedInteger.int64(Object value) {
    final BigInt parsed = _integer(value);
    final BigInt minimum = -(BigInt.one << 63);
    final BigInt maximum = (BigInt.one << 63) - BigInt.one;
    if (parsed < minimum || parsed > maximum) _fail('CLIENT_SCALAR_INVALID');
    return ExtendedInteger._(parsed);
  }

  factory ExtendedInteger.uint64(Object value) {
    final BigInt parsed = _integer(value);
    final BigInt maximum = (BigInt.one << 64) - BigInt.one;
    if (parsed.isNegative || parsed > maximum) _fail('CLIENT_SCALAR_INVALID');
    return ExtendedInteger._(parsed);
  }

  final BigInt value;

  @override
  String toWire() => value.toString();

  @override
  bool operator ==(Object other) =>
      other is ExtendedInteger && other.value == value;

  @override
  int get hashCode => value.hashCode;

  @override
  String toString() => toWire();
}

final class DecimalValue implements WireValue {
  DecimalValue(Object value) : wire = _canonicalDecimal(value);

  final String wire;

  @override
  String toWire() => wire;

  @override
  bool operator ==(Object other) => other is DecimalValue && other.wire == wire;

  @override
  int get hashCode => wire.hashCode;

  @override
  String toString() => wire;
}

final class TimestampValue implements WireValue {
  TimestampValue(String value) : wire = _canonicalTimestamp(value);

  final String wire;

  @override
  String toWire() => wire;

  @override
  bool operator ==(Object other) =>
      other is TimestampValue && other.wire == wire;

  @override
  int get hashCode => wire.hashCode;
}

final class DurationValue implements WireValue {
  DurationValue(Object value) : value = ExtendedInteger.int64(value);

  final ExtendedInteger value;

  @override
  String toWire() => value.toWire();
}

final class UuidValue implements WireValue {
  UuidValue(String value) : wire = value.toLowerCase() {
    if (!_uuid.hasMatch(wire)) _fail('CLIENT_SCALAR_INVALID');
  }

  final String wire;

  @override
  String toWire() => wire;
}

final class BytesValue implements WireValue {
  BytesValue(List<int> value) : bytes = Uint8List.fromList(value);

  factory BytesValue.decode(String wire) {
    if (wire.contains('=') || !_base64Url.hasMatch(wire)) {
      _fail('CLIENT_SCALAR_INVALID');
    }
    try {
      final int padding = (4 - wire.length % 4) % 4;
      return BytesValue(
        base64Url.decode(wire.padRight(wire.length + padding, '=')),
      );
    } on FormatException catch (error) {
      _fail('CLIENT_SCALAR_INVALID', error);
    }
  }

  final Uint8List bytes;

  @override
  String toWire() => base64Url.encode(bytes).replaceAll('=', '');
}

Object? encodeScalar(String type, Object? value) {
  if (value == null) return null;
  switch (type) {
    case 'String':
    case 'ID':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return value;
    case 'Boolean':
      if (value is! bool) _fail('CLIENT_SCALAR_INVALID');
      return value;
    case 'Int32':
      if (value is! int || value < -2147483648 || value > 2147483647) {
        _fail('CLIENT_SCALAR_INVALID');
      }
      return value;
    case 'Float64':
      if (value is! num || !value.toDouble().isFinite)
        _fail('CLIENT_SCALAR_INVALID');
      return value.toDouble();
    case 'Int64':
      return ExtendedInteger.int64(value).toWire();
    case 'UInt64':
      return ExtendedInteger.uint64(value).toWire();
    case 'BigInt':
      return ExtendedInteger.bigInt(value).toWire();
    case 'Decimal':
    case 'Money':
      return value is DecimalValue
          ? value.toWire()
          : DecimalValue(value).toWire();
    case 'Timestamp':
      if (value is TimestampValue) return value.toWire();
      _fail('CLIENT_SCALAR_INVALID');
    case 'Duration':
      return value is DurationValue
          ? value.toWire()
          : DurationValue(value).toWire();
    case 'UUID':
      return value is UuidValue
          ? value.toWire()
          : UuidValue(value as String).toWire();
    case 'Bytes':
      return value is BytesValue
          ? value.toWire()
          : BytesValue(value as List<int>).toWire();
    case 'StringList':
      if (value is! List<String>) _fail('CLIENT_SCALAR_INVALID');
      return List<String>.unmodifiable(value);
    case 'StringMap':
      if (value is! Map<String, String>) _fail('CLIENT_SCALAR_INVALID');
      return Map<String, String>.unmodifiable(value);
    case 'JSON':
      canonicalJson(value);
      return value;
    default:
      if (value is WireValue) return value.toWire();
      _fail('CLIENT_SCALAR_INVALID');
  }
}

Object? decodeScalar(String type, Object? value) {
  if (value == null) return null;
  switch (type) {
    case 'String':
    case 'ID':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return value;
    case 'Boolean':
      if (value is! bool) _fail('CLIENT_SCALAR_INVALID');
      return value;
    case 'Int32':
      if (value is! int || value < -2147483648 || value > 2147483647) {
        _fail('CLIENT_SCALAR_INVALID');
      }
      return value;
    case 'Float64':
      if (value is! num || !value.isFinite) _fail('CLIENT_SCALAR_INVALID');
      return value.toDouble();
    case 'Int64':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return ExtendedInteger.int64(value);
    case 'UInt64':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return ExtendedInteger.uint64(value);
    case 'BigInt':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return ExtendedInteger.bigInt(value);
    case 'Decimal':
    case 'Money':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return DecimalValue(value);
    case 'Timestamp':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return TimestampValue(value);
    case 'Duration':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return DurationValue(value);
    case 'UUID':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return UuidValue(value);
    case 'Bytes':
      if (value is! String) _fail('CLIENT_SCALAR_INVALID');
      return BytesValue.decode(value);
    case 'StringList':
      if (value is! List<Object?> ||
          value.any((Object? item) => item is! String)) {
        _fail('CLIENT_SCALAR_INVALID');
      }
      return List<String>.unmodifiable(value.cast<String>());
    case 'StringMap':
      if (value is! Map<String, Object?> ||
          value.values.any((Object? item) => item is! String)) {
        _fail('CLIENT_SCALAR_INVALID');
      }
      return Map<String, String>.unmodifiable(value.cast<String, String>());
    case 'JSON':
      return value;
    default:
      _fail('CLIENT_SCALAR_INVALID');
  }
}

BigInt _integer(Object value) {
  if (value is BigInt) return value;
  if (value is int) {
    if (value < -maximumSafeInteger || value > maximumSafeInteger) {
      _fail('CLIENT_VALUE_PRECISION');
    }
    return BigInt.from(value);
  }
  if (value is! String || !RegExp(r'^-?(?:0|[1-9][0-9]*)$').hasMatch(value)) {
    _fail('CLIENT_SCALAR_INVALID');
  }
  return BigInt.parse(value);
}

String _canonicalDecimal(Object value) {
  final String source = value is DecimalValue ? value.wire : value.toString();
  final RegExpMatch? match = RegExp(
    r'^(-?)([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$',
  ).firstMatch(source);
  if (match == null) _fail('CLIENT_SCALAR_INVALID');
  final String sign = match.group(1)!;
  String digits = '${match.group(2)!}${match.group(3) ?? ''}';
  final int fractionLength = (match.group(3) ?? '').length;
  final int exponent = int.tryParse(match.group(4) ?? '0') ?? 10001;
  if (digits.length > 10000 || exponent.abs() > 10000) {
    _fail('CLIENT_SCALAR_INVALID');
  }
  digits = digits.replaceFirst(RegExp(r'^0+'), '');
  if (digits.isEmpty) return '0';
  final int point = digits.length - fractionLength + exponent;
  String result;
  if (point <= 0) {
    result = '0.${_zeros(-point)}$digits';
  } else if (point >= digits.length) {
    result = '$digits${_zeros(point - digits.length)}';
  } else {
    result = '${digits.substring(0, point)}.${digits.substring(point)}';
  }
  if (result.contains('.')) {
    result = result
        .replaceFirst(RegExp(r'0+$'), '')
        .replaceFirst(RegExp(r'\.$'), '');
  }
  return sign == '-' ? '-$result' : result;
}

String _canonicalTimestamp(String value) {
  final RegExpMatch? match = RegExp(
    r'^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$',
  ).firstMatch(value);
  if (match == null) _fail('CLIENT_SCALAR_INVALID');
  int year = int.parse(match.group(1)!);
  int month = int.parse(match.group(2)!);
  int day = int.parse(match.group(3)!);
  int hour = int.parse(match.group(4)!);
  final int minute = int.parse(match.group(5)!);
  final int second = int.parse(match.group(6)!);
  final String fraction = (match.group(7) ?? '').replaceFirst(
    RegExp(r'0+$'),
    '',
  );
  final String zone = match.group(8)!;
  if (month < 1 ||
      month > 12 ||
      day < 1 ||
      day > _daysInMonth(year, month) ||
      hour > 23 ||
      minute > 59 ||
      second > 59) {
    _fail('CLIENT_SCALAR_INVALID');
  }
  int offset = 0;
  if (zone != 'Z') {
    final int offsetHour = int.parse(zone.substring(1, 3));
    final int offsetMinute = int.parse(zone.substring(4, 6));
    if (offsetHour > 23 || offsetMinute > 59) {
      _fail('CLIENT_SCALAR_INVALID');
    }
    offset = (offsetHour * 60 + offsetMinute) * (zone[0] == '+' ? 1 : -1);
  }
  int utcMinutes = hour * 60 + minute - offset;
  if (utcMinutes < 0) {
    final ({int year, int month, int day}) previous = _previousDay(
      year,
      month,
      day,
    );
    year = previous.year;
    month = previous.month;
    day = previous.day;
    utcMinutes += 1440;
  } else if (utcMinutes >= 1440) {
    final ({int year, int month, int day}) next = _nextDay(year, month, day);
    year = next.year;
    month = next.month;
    day = next.day;
    utcMinutes -= 1440;
  }
  if (year < 0 || year > 9999) _fail('CLIENT_SCALAR_INVALID');
  hour = utcMinutes ~/ 60;
  final int utcMinute = utcMinutes % 60;
  final String suffix = fraction.isEmpty ? '' : '.$fraction';
  return '${year.toString().padLeft(4, '0')}-'
      '${month.toString().padLeft(2, '0')}-'
      '${day.toString().padLeft(2, '0')}T'
      '${hour.toString().padLeft(2, '0')}:'
      '${utcMinute.toString().padLeft(2, '0')}:'
      '${second.toString().padLeft(2, '0')}${suffix}Z';
}

int _daysInMonth(int year, int month) {
  if (month == 2) {
    return year % 4 == 0 && (year % 100 != 0 || year % 400 == 0) ? 29 : 28;
  }
  return const <int>{4, 6, 9, 11}.contains(month) ? 30 : 31;
}

({int year, int month, int day}) _previousDay(int year, int month, int day) {
  if (day > 1) return (year: year, month: month, day: day - 1);
  if (month > 1) {
    return (year: year, month: month - 1, day: _daysInMonth(year, month - 1));
  }
  return (year: year - 1, month: 12, day: 31);
}

({int year, int month, int day}) _nextDay(int year, int month, int day) {
  if (day < _daysInMonth(year, month)) {
    return (year: year, month: month, day: day + 1);
  }
  if (month < 12) return (year: year, month: month + 1, day: 1);
  return (year: year + 1, month: 1, day: 1);
}

final RegExp _uuid = RegExp(
  r'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$',
);
final RegExp _base64Url = RegExp(r'^[A-Za-z0-9_-]*$');

String _zeros(int count) => List<String>.filled(count, '0').join();

Never _fail(String code, [Object? cause]) => fail(code, cause);
