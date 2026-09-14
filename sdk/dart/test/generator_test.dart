import 'dart:convert';
import 'dart:io';

import 'package:naatre/naatre.dart';

import 'support.dart';

void main() {
  final Directory root = Directory.current.parent.parent;
  final List<int> model = File(
    '${root.path}/conformance/v1/generator-model.json',
  ).readAsBytesSync();
  final List<int> reference = File(
    '${root.path}/conformance/v1/generator-output.json',
  ).readAsBytesSync();
  final DartArtifacts artifacts = generateDartSdk(model, reference);
  check(
    _equal(
      artifacts.source,
      File(
        '${root.path}/sdk/dart/lib/src/generated/operations.dart',
      ).readAsBytesSync(),
    ),
    'generated Dart source drifted',
  );
  check(
    _equal(
      artifacts.manifest,
      File(
        '${root.path}/sdk/dart/lib/src/generated/operations.json',
      ).readAsBytesSync(),
    ),
    'generated Dart manifest drifted',
  );

  final Map<String, Object?> changed =
      parseStrictJson(model) as Map<String, Object?>;
  final Map<String, Object?> schema =
      changed['schema']! as Map<String, Object?>;
  final List<Object?> types = schema['types']! as List<Object?>;
  types.add(<String, Object?>{
    'id': 'Subject',
    'name': 'Subject',
    'kind': 'union',
    'open': true,
    'variantMembers': <Object?>[
      <String, Object?>{'id': 'Subject.Account', 'type': 'Account'},
    ],
  });
  final Map<String, Object?> operation =
      (changed['operations']! as List<Object?>).single! as Map<String, Object?>;
  final Map<String, Object?> result =
      operation['result']! as Map<String, Object?>;
  final List<Object?> resultFields = result['fields']! as List<Object?>;
  resultFields.addAll(<Object?>[
    <String, Object?>{
      'name': 'balance',
      'presence': 'required',
      'result': <String, Object?>{'kind': 'scalar', 'type': 'Money'},
    },
    <String, Object?>{
      'name': 'status',
      'presence': 'required',
      'result': <String, Object?>{'kind': 'scalar', 'type': 'Status'},
    },
    <String, Object?>{
      'name': 'subject',
      'presence': 'required',
      'result': <String, Object?>{'kind': 'scalar', 'type': 'Subject'},
    },
  ]);
  final Map<String, Object?> changedReference =
      parseStrictJson(reference) as Map<String, Object?>;
  final Map<String, Object?> referenceSchema =
      changedReference['schema']! as Map<String, Object?>;
  referenceSchema['digest'] = semanticHash(
    'schema',
    _normalizeForFixture(schema),
  );
  final Map<String, Object?> referenceOperation =
      (changedReference['operations']! as List<Object?>).single!
          as Map<String, Object?>;
  final Map<String, Object?> referenceResult =
      referenceOperation['result']! as Map<String, Object?>;
  referenceResult['data'] = parseStrictJson(canonicalJson(result));
  final DartArtifacts union = generateDartSdk(
    utf8.encode(canonicalJson(changed)),
    utf8.encode(canonicalJson(changedReference)),
  );
  check(
    utf8
        .decode(union.source)
        .contains('final class Subject extends OpenVariant'),
    'open union wrapper was not generated',
  );
  final String generated = utf8.decode(union.source);
  check(
    generated.contains('decodeScalar("Decimal", value) as DecimalValue'),
    'custom scalar result decoder was not generated',
  );
  check(
    generated.contains('Status.decode(expectString(value))'),
    'open enum result decoder was not generated',
  );
  check(
    generated.contains('Subject.decode('),
    'open union result decoder was not generated',
  );

  final Map<String, Object?> reservedReference =
      parseStrictJson(reference) as Map<String, Object?>;
  final Map<String, Object?> reservedOperation =
      (reservedReference['operations']! as List<Object?>).single!
          as Map<String, Object?>;
  reservedOperation['symbol'] = 'class';
  expectClientError('DART_SDK_GENERATOR_INVALID_IDENTIFIER', () {
    return generateDartSdk(
      model,
      utf8.encode(canonicalJson(reservedReference)),
    );
  });

  final Map<String, Object?> collisionModel =
      parseStrictJson(model) as Map<String, Object?>;
  final Map<String, Object?> collisionSchema =
      collisionModel['schema']! as Map<String, Object?>;
  final Map<String, Object?> status =
      (collisionSchema['types']! as List<Object?>).first!
          as Map<String, Object?>;
  final List<Object?> members = status['enumMembers']! as List<Object?>;
  (members[0]! as Map<String, Object?>)['name'] = 'CLASS';
  (members[1]! as Map<String, Object?>)['name'] = 'class_';
  final Map<String, Object?> collisionReference =
      parseStrictJson(reference) as Map<String, Object?>;
  final Map<String, Object?> collisionReferenceSchema =
      collisionReference['schema']! as Map<String, Object?>;
  collisionReferenceSchema['digest'] = semanticHash(
    'schema',
    _normalizeForFixture(collisionSchema),
  );
  expectClientError('DART_SDK_GENERATOR_SYMBOL_COLLISION', () {
    return generateDartSdk(
      utf8.encode(canonicalJson(collisionModel)),
      utf8.encode(canonicalJson(collisionReference)),
    );
  });
}

Map<String, Object?> _normalizeForFixture(Map<String, Object?> schema) {
  final Map<String, Object?> copy =
      parseStrictJson(canonicalJson(schema)) as Map<String, Object?>;
  (copy['capabilities']! as List<Object?>).sort(
    (Object? left, Object? right) =>
        (left! as String).compareTo(right! as String),
  );
  final List<Object?> types = copy['types']! as List<Object?>;
  types.sort((Object? left, Object? right) {
    return ((left! as Map<String, Object?>)['id']! as String).compareTo(
      (right! as Map<String, Object?>)['id']! as String,
    );
  });
  for (final Object? raw in types) {
    final Map<String, Object?> descriptor = raw! as Map<String, Object?>;
    for (final String member in <String>[
      'fields',
      'enumMembers',
      'variantMembers',
    ]) {
      final Object? values = descriptor[member];
      if (values is List<Object?>) {
        values.sort((Object? left, Object? right) {
          return ((left! as Map<String, Object?>)['id']! as String).compareTo(
            (right! as Map<String, Object?>)['id']! as String,
          );
        });
      }
    }
  }
  return copy;
}

bool _equal(List<int> left, List<int> right) {
  if (left.length != right.length) return false;
  for (int index = 0; index < left.length; index += 1) {
    if (left[index] != right[index]) return false;
  }
  return true;
}
