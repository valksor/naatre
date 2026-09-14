import 'dart:typed_data';

import 'error.dart';
import 'json.dart';

enum PresenceState { missing, nullValue, pending, present, failed, skipped }

final class InputValue<T> {
  const InputValue.missing() : isPresent = false, value = null;

  const InputValue.present(T value) : isPresent = true, value = value;

  final bool isPresent;
  final T? value;
}

final class Selected<T> {
  const Selected._(
    this.state, {
    this.value,
    this.errors = const <NaatreError>[],
    this.reason,
  });

  const Selected.missing() : this._(PresenceState.missing);
  const Selected.nullValue() : this._(PresenceState.nullValue);
  const Selected.pending() : this._(PresenceState.pending);
  const Selected.present(T value) : this._(PresenceState.present, value: value);
  const Selected.failed(List<NaatreError> errors)
    : this._(PresenceState.failed, errors: errors);
  const Selected.skipped(String reason)
    : this._(PresenceState.skipped, reason: reason);

  final PresenceState state;
  final T? value;
  final List<NaatreError> errors;
  final String? reason;
}

class OpenEnumValue implements WireValue {
  const OpenEnumValue(this.raw, {required this.known});

  final String raw;
  final bool known;

  @override
  String toWire() => raw;
}

class OpenVariant {
  const OpenVariant(this.discriminator, this.value, {required this.known});

  final String discriminator;
  final Object? value;
  final bool known;
}

final class NaatreError {
  NaatreError._(this.code, this.message, this.path, this.raw);

  factory NaatreError.decode(Object? value) {
    final Map<String, Object?> map = expectObject(value);
    final Object? rawCode = map['code'];
    final Object? rawMessage = map['message'];
    final Object? rawPath = map['path'];
    if (rawCode is! String || rawMessage != null && rawMessage is! String) {
      _fail('CLIENT_PROTOCOL_INVALID');
    }
    final List<Object> path = <Object>[];
    if (rawPath != null) {
      if (rawPath is! List<Object?>) _fail('CLIENT_PROTOCOL_INVALID');
      for (final Object? segment in rawPath) {
        if (segment is! String && segment is! int)
          _fail('CLIENT_PROTOCOL_INVALID');
        path.add(segment!);
      }
    }
    return NaatreError._(
      rawCode,
      rawMessage as String? ?? '',
      List<Object>.unmodifiable(path),
      Map<String, Object?>.unmodifiable(map),
    );
  }

  final String code;
  final String message;
  final List<Object> path;
  final Map<String, Object?> raw;
}

final class PersistedReference implements WireValue {
  const PersistedReference(this.digest);

  final String digest;

  void validate() {
    if (!RegExp(r'^[0-9a-f]{64}$').hasMatch(digest)) {
      _fail('CLIENT_OPERATION_INVALID');
    }
  }

  @override
  Map<String, Object?> toWire() {
    validate();
    return <String, Object?>{
      'algorithm': 'sha-256',
      'canonicalVersion': 'c14n-1',
      'digest': digest,
    };
  }
}

typedef DataDecoder<T> = T Function(Object? value);

final class Operation<TVariables, TResult> {
  Operation({
    required this.name,
    required this.kind,
    required this.persisted,
    required this.variables,
    required Map<String, Object?> wireVariables,
    required DataDecoder<TResult> decodeData,
  }) : wireVariables = Map<String, Object?>.unmodifiable(wireVariables),
       _decodeData = decodeData {
    if (!const <String>{'query', 'mutation', 'subscription'}.contains(kind)) {
      _fail('CLIENT_OPERATION_INVALID');
    }
    persisted.validate();
  }

  final String name;
  final String kind;
  final PersistedReference persisted;
  final TVariables variables;
  final Map<String, Object?> wireVariables;
  final DataDecoder<TResult> _decodeData;

  bool get replayable => kind == 'query';

  Map<String, Object?> request() => <String, Object?>{
    'version': '1',
    'operation': name,
    'persisted': persisted,
    'variables': wireVariables,
  };

  Uint8List canonicalRequest() => canonicalJsonBytes(request());

  OperationResult<TResult> decodeResult(
    Object payload, {
    int maximumBytes = 16 << 20,
  }) {
    final Object? decoded = payload is Map<String, Object?>
        ? payload
        : parseStrictJson(payload, maximumBytes: maximumBytes);
    final Map<String, Object?> envelope = expectObject(decoded);
    for (final String key in envelope.keys) {
      if (!const <String>{'data', 'errors', 'complete'}.contains(key)) {
        _fail('CLIENT_PROTOCOL_INVALID');
      }
    }
    final Object? complete = envelope['complete'];
    if (complete is! bool) _fail('CLIENT_PROTOCOL_INVALID');
    final Object? rawErrors = envelope['errors'] ?? const <Object?>[];
    if (rawErrors is! List<Object?>) _fail('CLIENT_PROTOCOL_INVALID');
    final List<NaatreError> errors = rawErrors
        .map(NaatreError.decode)
        .toList(growable: false);
    final Object? rawData = envelope['data'];
    return OperationResult<TResult>(
      data: rawData == null ? null : _decodeData(rawData),
      errors: List<NaatreError>.unmodifiable(errors),
      complete: complete,
    );
  }
}

final class OperationResult<T> {
  const OperationResult({
    required this.data,
    required this.errors,
    required this.complete,
  });

  final T? data;
  final List<NaatreError> errors;
  final bool complete;
}

Selected<T> decodeSelected<T>(
  Map<String, Object?> owner,
  String key, {
  required bool pendingWhenMissing,
  required T Function(Object? value) decode,
}) {
  if (!owner.containsKey(key)) {
    return pendingWhenMissing ? Selected<T>.pending() : Selected<T>.missing();
  }
  final Object? value = owner[key];
  if (value == null) return Selected<T>.nullValue();
  return Selected<T>.present(decode(value));
}

Map<String, Object?> expectObject(Object? value) {
  if (value is! Map<String, Object?>) _fail('CLIENT_PROTOCOL_INVALID');
  return value;
}

String expectString(Object? value) {
  if (value is! String) _fail('CLIENT_PROTOCOL_INVALID');
  return value;
}

Never _fail(String code, [Object? cause]) => fail(code, cause);
