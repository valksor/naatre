import 'dart:convert';

import 'package:naatre/generated.dart';
import 'package:naatre/naatre.dart';

import 'support.dart';

void main() {
  check(
    canonicalJson(<String, Object?>{'\ue000': 1, '\u{10000}': 2}) ==
        '{"𐀀":2,"":1}',
    'canonical keys must use UTF-16 ordering',
  );
  check(
    semanticHash('document', <String, Object?>{'x': true}) ==
        '511ef301719694e653535d43322322ab6bc0579d18b0bba8aece5ec685f2da6f',
    'portable SHA-256 digest drifted',
  );
  check(
    canonicalizeJson(
          '[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001]',
        ) ==
        '[333333333.3333333,1e+30,4.5,0.002,1e-27]',
    'RFC 8785 number canonicalization drifted',
  );
  check(
    canonicalizeJson('{"negative":-1e-400,"positive":1e-400}') ==
        '{"negative":0,"positive":0}',
    'finite underflow canonicalization drifted',
  );
  expectClientError('CLIENT_PROTOCOL_INVALID', () {
    return parseStrictJson('{"a":1,"a":2}');
  });
  expectClientError('CLIENT_RESPONSE_LIMIT', () {
    return parseStrictJson('[[[]]]', maximumDepth: 1);
  });
  expectClientError('CLIENT_RESPONSE_LIMIT', () {
    return parseStrictJson('[1,2]', maximumTokens: 2);
  });
  expectClientError('CLIENT_VALUE_PRECISION', () {
    return parseStrictJson('{"value":9007199254740993}');
  });
  expectClientError('CLIENT_VALUE_PRECISION', () {
    return canonicalJson(<String, Object?>{'value': 9007199254740992});
  });
  expectClientError('CLIENT_VALUE_PRECISION', () {
    return ExtendedInteger.bigInt(9007199254740992);
  });
  expectClientError('CLIENT_VALUE_INVALID', () {
    return canonicalJson(<String, Object?>{'value': double.infinity});
  });
  check(
    parseStrictJson('[[0]]', maximumDepth: 2) is List<Object?>,
    'valid JSON depth was rejected',
  );
  expectClientError('CLIENT_RESPONSE_LIMIT', () {
    return parseStrictJson('[[[0]]]', maximumDepth: 2);
  });
  check(
    parseStrictJson('[0,0,0]', maximumTokens: 4) is List<Object?>,
    'valid JSON token count was rejected',
  );
  expectClientError('CLIENT_RESPONSE_LIMIT', () {
    return parseStrictJson('[0,0,0,0]', maximumTokens: 4);
  });

  final ExtendedInteger large = ExtendedInteger.bigInt(
    '100000000000000000000000000000000000007',
  );
  check(
    large.toWire() == '100000000000000000000000000000000000007',
    'extended integer lost precision',
  );
  check(
    DecimalValue('7.8900E-120').toWire() ==
        '0.${List<String>.filled(119, '0').join()}789',
    'decimal exponent or scale canonicalization failed',
  );
  final BytesValue bytes = BytesValue(utf8.encode('Naatre'));
  check(
    utf8.decode(BytesValue.decode(bytes.toWire()).bytes) == 'Naatre',
    'bytes drifted',
  );
  check(
    (decodeScalar('Money', '1.2300')! as DecimalValue).toWire() == '1.23',
    'custom scalar result decoding drifted',
  );
  check(
    (decodeScalar('Int64', '-9223372036854775808')! as ExtendedInteger)
            .toWire() ==
        '-9223372036854775808',
    'extended integer result decoding drifted',
  );
  expectClientError('CLIENT_SCALAR_INVALID', () {
    return decodeScalar('Int64', 1);
  });
  final TimestampValue timestamp = TimestampValue(
    '2026-09-11T23:30:01.123456789+03:00',
  );
  check(
    timestamp.toWire() == '2026-09-11T20:30:01.123456789Z',
    'timestamp precision or offset was lost',
  );
  check(
    TimestampValue('0000-02-29T12:34:56.1200Z').toWire() ==
        '0000-02-29T12:34:56.12Z',
    'timestamp year zero or fraction canonicalization failed',
  );
  check(
    TimestampValue('0099-12-31T23:30:00+01:00').toWire() ==
        '0099-12-31T22:30:00Z',
    'timestamp early-year offset canonicalization failed',
  );
  for (final String invalid in <String>[
    '0000-01-01T00:00:00+01:00',
    '9999-12-31T23:30:00-01:00',
    '2026-01-01T00:00:00+24:00',
    '2016-12-31T23:59:60Z',
    '2026-02-29T00:00:00Z',
  ]) {
    expectClientError('CLIENT_SCALAR_INVALID', () => TimestampValue(invalid));
  }
  check(!Status.decode('FUTURE').known, 'unknown open enum was not preserved');
}
