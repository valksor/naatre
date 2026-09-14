import 'dart:collection';
import 'dart:convert';
import 'dart:typed_data';

import 'error.dart';
import 'sha256.dart';

const int maximumSafeInteger = 9007199254740991;
const int _defaultMaximumDepth = 64;
const int _defaultMaximumTokens = 100000;

abstract interface class WireValue {
  Object? toWire();
}

String canonicalJson(Object? value) {
  return _CanonicalWriter().write(value);
}

Uint8List canonicalJsonBytes(Object? value) {
  return Uint8List.fromList(utf8.encode(canonicalJson(value)));
}

Object? parseStrictJson(
  Object input, {
  int maximumBytes = 16 << 20,
  int maximumDepth = _defaultMaximumDepth,
  int maximumTokens = _defaultMaximumTokens,
}) {
  return _StrictParser(
    _strictJsonSource(input, maximumBytes),
    maximumDepth: maximumDepth,
    maximumTokens: maximumTokens,
  ).parse();
}

String canonicalizeJson(
  Object input, {
  int maximumBytes = 16 << 20,
  int maximumDepth = _defaultMaximumDepth,
  int maximumTokens = _defaultMaximumTokens,
}) {
  return _CanonicalWriter().write(
    _StrictParser(
      _strictJsonSource(input, maximumBytes),
      preserveFloatingNumbers: true,
      maximumDepth: maximumDepth,
      maximumTokens: maximumTokens,
    ).parse(),
  );
}

String _strictJsonSource(Object input, int maximumBytes) {
  late final String source;
  try {
    if (input is String) {
      source = input;
    } else if (input is List<int>) {
      source = utf8.decode(input, allowMalformed: false);
    } else {
      _fail('CLIENT_PROTOCOL_INVALID');
    }
  } on FormatException catch (error) {
    _fail('CLIENT_PROTOCOL_INVALID', error);
  }
  if (utf8.encode(source).length > maximumBytes) {
    _fail('CLIENT_RESPONSE_LIMIT');
  }
  return source;
}

String semanticHash(String purpose, Object? value) {
  final List<int> input = utf8.encode(
    'naatre:$purpose:c14n-1\n${canonicalJson(value)}',
  );
  return sha256Hex(input);
}

final class _CanonicalWriter {
  final Set<Object> _ancestors = HashSet<Object>.identity();

  String write(Object? value) {
    if (value is _ParsedDouble) return _writeDouble(value.value);
    if (value is WireValue) {
      return write(value.toWire());
    }
    if (value == null) return 'null';
    if (value is String) {
      _validateUnicode(value, 'CLIENT_VALUE_INVALID');
      return jsonEncode(value);
    }
    if (value is bool) return value ? 'true' : 'false';
    if (value is num && !value.isFinite) _fail('CLIENT_VALUE_INVALID');
    if (value is int) {
      if (value < -maximumSafeInteger || value > maximumSafeInteger) {
        _fail('CLIENT_VALUE_PRECISION');
      }
      return value.toString();
    }
    if (value is double) return _writeDouble(value);
    if (value is BigInt) _fail('CLIENT_VALUE_PRECISION');
    if (value is List<Object?>) return _writeList(value);
    if (value is Map<Object?, Object?>) return _writeMap(value);
    _fail('CLIENT_VALUE_INVALID');
  }

  String _writeDouble(double value) {
    if (!value.isFinite) _fail('CLIENT_VALUE_INVALID');
    if (value == 0) return '0';
    final String encoded = value.toString();
    return encoded.endsWith('.0')
        ? encoded.substring(0, encoded.length - 2)
        : encoded;
  }

  String _writeList(List<Object?> value) {
    return _composite(value, () {
      return '[${value.map(write).join(',')}]';
    });
  }

  String _writeMap(Map<Object?, Object?> value) {
    return _composite(value, () {
      final List<String> keys = <String>[];
      for (final Object? key in value.keys) {
        if (key is! String) _fail('CLIENT_VALUE_INVALID');
        _validateUnicode(key, 'CLIENT_VALUE_INVALID');
        keys.add(key);
      }
      keys.sort();
      return '{${keys.map((String key) => '${jsonEncode(key)}:${write(value[key])}').join(',')}}';
    });
  }

  String _composite(Object value, String Function() callback) {
    if (!_ancestors.add(value)) _fail('CLIENT_VALUE_INVALID');
    try {
      return callback();
    } finally {
      _ancestors.remove(value);
    }
  }
}

final class _StrictParser {
  _StrictParser(
    this.input, {
    this.preserveFloatingNumbers = false,
    required this.maximumDepth,
    required this.maximumTokens,
  }) {
    if (maximumDepth < 0 || maximumTokens < 1) {
      _fail('CLIENT_RESPONSE_LIMIT');
    }
  }

  final String input;
  final bool preserveFloatingNumbers;
  final int maximumDepth;
  final int maximumTokens;
  int index = 0;
  int tokens = 0;

  Object? parse() {
    _space();
    final Object? result = _value(0);
    _space();
    if (index != input.length) _fail('CLIENT_PROTOCOL_INVALID');
    return result;
  }

  Object? _value(int depth) {
    _space();
    if (depth > maximumDepth || ++tokens > maximumTokens) {
      _fail('CLIENT_RESPONSE_LIMIT');
    }
    if (index >= input.length) _fail('CLIENT_PROTOCOL_INVALID');
    final int token = input.codeUnitAt(index);
    if (token == 0x7b) return _object(depth);
    if (token == 0x5b) return _array(depth);
    if (token == 0x22) return _string();
    if (input.startsWith('true', index)) return _keyword('true', true);
    if (input.startsWith('false', index)) return _keyword('false', false);
    if (input.startsWith('null', index)) return _keyword('null', null);
    return _number();
  }

  Map<String, Object?> _object(int depth) {
    index += 1;
    final Map<String, Object?> result = <String, Object?>{};
    _space();
    if (_take(0x7d)) return result;
    while (true) {
      if (index >= input.length || input.codeUnitAt(index) != 0x22) {
        _fail('CLIENT_PROTOCOL_INVALID');
      }
      final String key = _string();
      if (result.containsKey(key)) _fail('CLIENT_PROTOCOL_INVALID');
      _space();
      if (!_take(0x3a)) _fail('CLIENT_PROTOCOL_INVALID');
      result[key] = _value(depth + 1);
      _space();
      if (_take(0x7d)) return result;
      if (!_take(0x2c)) _fail('CLIENT_PROTOCOL_INVALID');
      _space();
    }
  }

  List<Object?> _array(int depth) {
    index += 1;
    final List<Object?> result = <Object?>[];
    _space();
    if (_take(0x5d)) return result;
    while (true) {
      result.add(_value(depth + 1));
      _space();
      if (_take(0x5d)) return result;
      if (!_take(0x2c)) _fail('CLIENT_PROTOCOL_INVALID');
      _space();
    }
  }

  String _string() {
    final int start = index;
    index += 1;
    while (index < input.length) {
      final int token = input.codeUnitAt(index);
      if (token == 0x22) {
        index += 1;
        try {
          final Object? decoded = jsonDecode(input.substring(start, index));
          if (decoded is! String) _fail('CLIENT_PROTOCOL_INVALID');
          _validateUnicode(decoded, 'CLIENT_PROTOCOL_INVALID');
          return decoded;
        } on FormatException catch (error) {
          _fail('CLIENT_PROTOCOL_INVALID', error);
        }
      }
      if (token < 0x20) _fail('CLIENT_PROTOCOL_INVALID');
      if (token == 0x5c) {
        index += 1;
        if (index >= input.length) _fail('CLIENT_PROTOCOL_INVALID');
        final int escaped = input.codeUnitAt(index);
        if (escaped == 0x75) {
          if (index + 4 >= input.length ||
              !RegExp(
                r'^[0-9a-fA-F]{4}$',
              ).hasMatch(input.substring(index + 1, index + 5))) {
            _fail('CLIENT_PROTOCOL_INVALID');
          }
          index += 5;
          continue;
        }
        if (!const <int>{
          0x22,
          0x5c,
          0x2f,
          0x62,
          0x66,
          0x6e,
          0x72,
          0x74,
        }.contains(escaped)) {
          _fail('CLIENT_PROTOCOL_INVALID');
        }
      }
      index += 1;
    }
    _fail('CLIENT_PROTOCOL_INVALID');
  }

  Object _number() {
    final RegExpMatch? match = RegExp(
      r'^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?',
    ).firstMatch(input.substring(index));
    if (match == null) _fail('CLIENT_PROTOCOL_INVALID');
    final String raw = match.group(0)!;
    index += raw.length;
    if (raw.contains('.') || raw.contains('e') || raw.contains('E')) {
      final double value = double.parse(raw);
      if (!value.isFinite) _fail('CLIENT_VALUE_PRECISION');
      if (preserveFloatingNumbers) return _ParsedDouble(value);
      return value == 0 ? 0 : value;
    }
    final BigInt value = BigInt.parse(raw);
    if (value.abs() > BigInt.from(maximumSafeInteger)) {
      _fail('CLIENT_VALUE_PRECISION');
    }
    return value.toInt();
  }

  Object? _keyword(String keyword, Object? value) {
    index += keyword.length;
    return value;
  }

  void _space() {
    while (index < input.length &&
        const <int>{0x20, 0x09, 0x0a, 0x0d}.contains(input.codeUnitAt(index))) {
      index += 1;
    }
  }

  bool _take(int expected) {
    if (index >= input.length || input.codeUnitAt(index) != expected)
      return false;
    index += 1;
    return true;
  }
}

final class _ParsedDouble {
  const _ParsedDouble(this.value);

  final double value;
}

void _validateUnicode(String value, String code) {
  for (int index = 0; index < value.length; index += 1) {
    final int unit = value.codeUnitAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      if (index + 1 >= value.length) _fail(code);
      final int next = value.codeUnitAt(index + 1);
      if (next < 0xdc00 || next > 0xdfff) _fail(code);
      index += 1;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      _fail(code);
    }
  }
}

Never _fail(String code, [Object? cause]) => fail(code, cause);
