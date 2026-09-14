import 'dart:async';
import 'dart:convert';

import 'error.dart';
import 'json.dart';
import 'model.dart';

final class StreamFrame {
  const StreamFrame({
    required this.type,
    required this.stream,
    required this.sequence,
    required this.value,
  });

  final String type;
  final String stream;
  final int? sequence;
  final Map<String, Object?> value;

  bool get terminal =>
      type == 'complete' || type == 'error' && value['final'] == true;
}

Stream<StreamFrame> decodeSse(
  Stream<List<int>> source, {
  int maximumFrameBytes = 1 << 20,
  Future<void> Function()? close,
}) async* {
  final List<int> lineBuffer = <int>[];
  final List<String> data = <String>[];
  String? event;
  String? eventId;
  String? acceptedStream;
  int acceptedSequence = 0;
  int eventBytes = 0;
  int dataBytes = 0;
  bool terminal = false;
  bool sourceEnded = false;
  try {
    await for (final List<int> chunk in source) {
      for (final int byte in chunk) {
        if (terminal) _fail('CLIENT_STREAM_INVALID');
        eventBytes += 1;
        if (eventBytes > maximumFrameBytes) _fail('CLIENT_STREAM_LIMIT');
        if (byte != 0x0a) {
          lineBuffer.add(byte);
          continue;
        }
        final List<int> raw = List<int>.of(lineBuffer);
        lineBuffer.clear();
        if (raw.isNotEmpty && raw.last == 0x0d) raw.removeLast();
        late final String line;
        try {
          line = utf8.decode(raw, allowMalformed: false);
        } on FormatException catch (error) {
          _fail('CLIENT_STREAM_INVALID', error);
        }
        if (line.isEmpty) {
          if (data.isNotEmpty) {
            final StreamFrame frame = _decodeEvent(
              event,
              eventId,
              data,
              maximumFrameBytes,
            );
            if (acceptedStream != null && frame.stream != acceptedStream) {
              _fail('CLIENT_STREAM_INVALID');
            }
            if (frame.type == 'keepalive') {
              if (acceptedSequence == 0) _fail('CLIENT_STREAM_INVALID');
            } else {
              if (frame.sequence != acceptedSequence + 1 ||
                  acceptedSequence == 0 && frame.type != 'open' ||
                  (frame.type == 'resume' ||
                          frame.type == 'history-unavailable') &&
                      acceptedSequence != 1) {
                _fail('CLIENT_STREAM_INVALID');
              }
              acceptedStream ??= frame.stream;
              acceptedSequence = frame.sequence!;
            }
            terminal = frame.terminal;
            yield frame;
            if (terminal) return;
          }
          event = null;
          eventId = null;
          data.clear();
          eventBytes = 0;
          dataBytes = 0;
          continue;
        }
        if (line.startsWith(':')) continue;
        final int separator = line.indexOf(':');
        final String field = separator < 0
            ? line
            : line.substring(0, separator);
        String value = separator < 0 ? '' : line.substring(separator + 1);
        if (value.startsWith(' ')) value = value.substring(1);
        switch (field) {
          case 'event':
            if (event != null) _fail('CLIENT_STREAM_INVALID');
            event = value;
          case 'id':
            if (eventId != null || value.contains('\u0000'))
              _fail('CLIENT_STREAM_INVALID');
            eventId = value;
          case 'data':
            data.add(value);
            dataBytes += utf8.encode(value).length;
            if (data.length > 1) dataBytes += 1;
            if (dataBytes > maximumFrameBytes) {
              _fail('CLIENT_STREAM_LIMIT');
            }
          case 'retry':
            break;
          default:
            _fail('CLIENT_STREAM_INVALID');
        }
      }
    }
    sourceEnded = true;
    if (lineBuffer.isNotEmpty ||
        data.isNotEmpty ||
        event != null ||
        eventId != null ||
        !terminal) {
      _fail('CLIENT_STREAM_TRUNCATED');
    }
  } finally {
    await close?.call();
    if (!sourceEnded) {
      lineBuffer.clear();
      data.clear();
    }
  }
}

StreamFrame _decodeEvent(
  String? event,
  String? eventId,
  List<String> lines,
  int maximumFrameBytes,
) {
  final Map<String, Object?> frame = expectObject(
    parseStrictJson(lines.join('\n'), maximumBytes: maximumFrameBytes),
  );
  final Object? type = frame['type'];
  final Object? stream = frame['stream'];
  final Object? sequence = frame['sequence'];
  if (type is! String ||
      stream is! String ||
      !const <String>{
        'open',
        'data',
        'patch',
        'error',
        'complete',
        'keepalive',
        'resume',
        'history-unavailable',
      }.contains(type) ||
      event != 'naatre.$type') {
    _fail('CLIENT_STREAM_INVALID');
  }
  if (type == 'keepalive') {
    if (sequence != null) _fail('CLIENT_STREAM_INVALID');
  } else if (sequence is! int || sequence < 1) {
    _fail('CLIENT_STREAM_INVALID');
  }
  final Object? cursor = frame['cursor'];
  if ((eventId == null) != (cursor == null) ||
      eventId != null && (cursor is! String || cursor != eventId)) {
    _fail('CLIENT_STREAM_INVALID');
  }
  return StreamFrame(
    type: type,
    stream: stream,
    sequence: sequence as int?,
    value: Map<String, Object?>.unmodifiable(frame),
  );
}

Never _fail(String code, [Object? cause]) => fail(code, cause);
