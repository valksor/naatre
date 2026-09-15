import 'dart:convert';
import 'dart:typed_data';

import 'error.dart';
import 'json.dart';

const String dartGeneratorVersion = 'naatre.generator.dart-sdk-1';
const int _maximumInputBytes = 4 << 20;

final class DartGeneratorException extends NaatreClientException {
  const DartGeneratorException(String code, [Object? cause])
    : super(code, cause);
}

final class DartArtifacts {
  const DartArtifacts({required this.source, required this.manifest});

  final Uint8List source;
  final Uint8List manifest;
}

DartArtifacts generateDartSdk(List<int> modelBytes, List<int> referenceBytes) {
  if (modelBytes.isEmpty ||
      referenceBytes.isEmpty ||
      modelBytes.length > _maximumInputBytes ||
      referenceBytes.length > _maximumInputBytes) {
    _reject('DART_SDK_GENERATOR_INPUT_LIMIT');
  }
  late final Map<String, Object?> model;
  late final Map<String, Object?> reference;
  try {
    model = _object(
      parseStrictJson(modelBytes, maximumBytes: _maximumInputBytes),
    );
    reference = _object(
      parseStrictJson(referenceBytes, maximumBytes: _maximumInputBytes),
    );
  } on NaatreClientException catch (error) {
    throw DartGeneratorException('DART_SDK_GENERATOR_INVALID_INPUT', error);
  }
  _validateVersions(model, reference);
  _validateSchema(model, reference);
  final List<Map<String, Object?>> operations = _array(
    model['operations'],
  ).map(_object).toList();
  final List<Map<String, Object?>> references = _array(
    reference['operations'],
  ).map(_object).toList();
  if (operations.length != references.length)
    _reject('DART_SDK_GENERATOR_REFERENCE_DRIFT');
  final Map<String, Map<String, Object?>> byName =
      <String, Map<String, Object?>>{};
  for (final Map<String, Object?> operation in references) {
    final String name = _string(operation['name']);
    if (byName.containsKey(name)) _reject('DART_SDK_GENERATOR_REFERENCE_DRIFT');
    byName[name] = operation;
  }
  final List<_Binding> bindings =
      operations
          .map((Map<String, Object?> operation) => _binding(operation, byName))
          .toList()
        ..sort(
          (_Binding left, _Binding right) => left.name.compareTo(right.name),
        );
  _validateGeneratedSymbols(model, bindings);
  final Map<String, Object?> mappings = _object(
    _object(model['configuration'])['scalarMappings'],
  );
  final String source = _generateSource(model, bindings, mappings);
  final Map<String, Object?> manifest = <String, Object?>{
    'profile': 'sdk.dart.core-1',
    'version': '1',
    'protocolVersion': '1',
    'canonicalVersion': 'c14n-1',
    'operations': bindings
        .map(
          (_Binding binding) => <String, Object?>{
            'name': binding.name,
            'kind': binding.kind,
            'persisted': binding.persisted,
          },
        )
        .toList(),
  };
  return DartArtifacts(
    source: Uint8List.fromList(utf8.encode('$source\n')),
    manifest: Uint8List.fromList(utf8.encode('${canonicalJson(manifest)}\n')),
  );
}

void _validateVersions(
  Map<String, Object?> model,
  Map<String, Object?> reference,
) {
  if (model['profile'] != 'sdk.generation-1' ||
      model['version'] != 'naatre.generator-model-1' ||
      model['protocolVersion'] != '1' ||
      model['canonicalVersion'] != 'c14n-1' ||
      reference['generatorVersion'] != 'naatre.generator.reference-json-1' ||
      reference['modelVersion'] != model['version'] ||
      reference['protocolVersion'] != model['protocolVersion'] ||
      reference['canonicalVersion'] != model['canonicalVersion']) {
    _reject('DART_SDK_GENERATOR_VERSION_SKEW');
  }
}

void _validateSchema(
  Map<String, Object?> model,
  Map<String, Object?> reference,
) {
  final Map<String, Object?> schema = _object(model['schema']);
  final Map<String, Object?> referenceSchema = _object(reference['schema']);
  if (referenceSchema['algorithm'] != 'sha-256' ||
      referenceSchema['canonicalVersion'] != 'c14n-1' ||
      referenceSchema['digest'] !=
          semanticHash('schema', _normalizeSchema(schema))) {
    _reject('DART_SDK_GENERATOR_REFERENCE_DRIFT');
  }
  final Map<String, Object?> configuration = _object(model['configuration']);
  final Map<String, Object?> mappings = _object(
    configuration['scalarMappings'],
  );
  if (canonicalJson(mappings) != canonicalJson(reference['scalarMappings'])) {
    _reject('DART_SDK_GENERATOR_REFERENCE_DRIFT');
  }
  for (final Object? raw in _array(schema['types'])) {
    final Map<String, Object?> descriptor = _object(raw);
    if (descriptor['kind'] == 'scalar' &&
        !mappings.containsKey(_string(descriptor['name']))) {
      _reject('DART_SDK_GENERATOR_UNMAPPED_SCALAR');
    }
  }
}

Map<String, Object?> _normalizeSchema(Map<String, Object?> source) {
  final Map<String, Object?> value = _deepObject(source);
  _sortStrings(value, 'capabilities');
  _sortById(value, 'types');
  for (final Object? raw
      in value['types'] as List<Object?>? ?? const <Object?>[]) {
    final Map<String, Object?> descriptor = raw! as Map<String, Object?>;
    for (final String member in <String>[
      'variants',
      'enumValues',
      'capabilities',
    ]) {
      _sortStrings(descriptor, member);
    }
    for (final String member in <String>[
      'fields',
      'enumMembers',
      'variantMembers',
      'retired',
      'traits',
    ]) {
      _sortById(descriptor, member);
    }
    final Object? entity = descriptor['entity'];
    if (entity is Map<String, Object?>) _sortStrings(entity, 'keys');
    final Object? scalar = descriptor['scalar'];
    if (scalar is Map<String, Object?>)
      _sortStrings(scalar, 'acceptedWireShapes');
  }
  for (final String member in <String>[
    'operations',
    'members',
    'retired',
    'traits',
    'directives',
    'extensions',
    'references',
  ]) {
    _sortById(value, member);
  }
  return value;
}

Map<String, Object?> _deepObject(Map<String, Object?> source) {
  return source.map(
    (String key, Object? value) =>
        MapEntry<String, Object?>(key, _deepValue(value)),
  );
}

Object? _deepValue(Object? value) {
  if (value is Map<String, Object?>) return _deepObject(value);
  if (value is List<Object?>) return value.map(_deepValue).toList();
  return value;
}

void _sortStrings(Map<String, Object?> value, String member) {
  final Object? raw = value[member];
  if (raw is List<Object?>)
    raw.sort(
      (Object? left, Object? right) => _string(left).compareTo(_string(right)),
    );
}

void _sortById(Map<String, Object?> value, String member) {
  final Object? raw = value[member];
  if (raw is List<Object?>) {
    raw.sort((Object? left, Object? right) {
      final Map<String, Object?> leftObject = _object(left);
      final Map<String, Object?> rightObject = _object(right);
      return _string(leftObject['id']).compareTo(_string(rightObject['id']));
    });
  }
}

_Binding _binding(
  Map<String, Object?> operation,
  Map<String, Map<String, Object?>> references,
) {
  final String name = _identifier(operation['name']);
  final Map<String, Object?>? reference = references[name];
  if (reference == null) _reject('DART_SDK_GENERATOR_REFERENCE_DRIFT');
  final String symbol = _declarationName(reference['symbol']);
  final Map<String, Object?> document = _object(operation['document']);
  final List<Map<String, Object?>> definitions = _array(
    document['operations'],
  ).map(_object).toList();
  final List<Map<String, Object?>> matches = definitions
      .where((Map<String, Object?> value) => value['name'] == name)
      .toList();
  if (matches.length != 1) _reject('DART_SDK_GENERATOR_INVALID_OPERATION');
  final String kind = _string(matches.single['kind']);
  if (!const <String>{'query', 'mutation', 'subscription'}.contains(kind)) {
    _reject('DART_SDK_GENERATOR_INVALID_OPERATION');
  }
  final Map<String, Object?> persisted = _object(reference['persisted']);
  if (persisted['algorithm'] != 'sha-256' ||
      persisted['canonicalVersion'] != 'c14n-1' ||
      persisted['digest'] != semanticHash('document', document) ||
      canonicalJson(operation['variables']) !=
          canonicalJson(reference['variables']) ||
      canonicalJson(operation['result']) !=
          canonicalJson(_object(reference['result'])['data'])) {
    _reject('DART_SDK_GENERATOR_REFERENCE_DRIFT');
  }
  return _Binding(
    name: name,
    symbol: symbol,
    kind: kind,
    persisted: Map<String, Object?>.unmodifiable(persisted),
    variables: _array(operation['variables']).map(_object).toList(),
    result: _object(operation['result']),
  );
}

String _generateSource(
  Map<String, Object?> model,
  List<_Binding> bindings,
  Map<String, Object?> mappings,
) {
  final List<String> lines = <String>[
    '// Code generated by the Naatre Dart SDK generator. DO NOT EDIT.',
    "import 'package:naatre/naatre.dart';",
    '',
    "const String generatorVersion = '$dartGeneratorVersion';",
    '',
  ];
  final List<Map<String, Object?>> types =
      _array(_object(model['schema'])['types']).map(_object).toList()..sort(
        (Map<String, Object?> left, Map<String, Object?> right) =>
            _string(left['id']).compareTo(_string(right['id'])),
      );
  final Map<String, String> typeKinds = <String, String>{
    for (final Map<String, Object?> descriptor in types)
      _string(descriptor['name'] ?? descriptor['id']): _string(
        descriptor['kind'],
      ),
  };
  for (final Map<String, Object?> descriptor in types) {
    lines.addAll(_renderSchema(descriptor, mappings));
    lines.add('');
  }
  for (final _Binding binding in bindings) {
    lines.addAll(_renderOperation(binding, mappings, typeKinds));
    lines.add('');
  }
  return lines.join('\n').trimRight();
}

List<String> _renderSchema(
  Map<String, Object?> descriptor,
  Map<String, Object?> mappings,
) {
  final String name = _declarationName(descriptor['name'] ?? descriptor['id']);
  final String kind = _string(descriptor['kind']);
  if (kind == 'scalar') {
    return <String>['typedef $name = ${_dartType(name, mappings)};'];
  }
  if (kind == 'enum') {
    final List<String> lines = <String>[
      'final class $name extends OpenEnumValue {',
      '  const $name._(super.raw, {required super.known});',
      '',
    ];
    final List<String> members = <String>[];
    for (final Object? raw in _array(descriptor['enumMembers'])) {
      final String member = _string(_object(raw)['name']);
      final String field = _dartName(member.toLowerCase());
      members.add(member);
      lines.add(
        "  static const $name $field = $name._(${_literal(member)}, known: true);",
      );
    }
    lines.addAll(<String>[
      '',
      '  factory $name.decode(String raw) {',
      '    return switch (raw) {',
      for (final String member in members)
        '      ${_literal(member)} => ${_dartName(member.toLowerCase())},',
      '      _ => $name._(raw, known: false),',
      '    };',
      '  }',
      '}',
    ]);
    return lines;
  }
  if (kind == 'union') {
    final List<String> members = _array(
      descriptor['variantMembers'],
    ).map((Object? raw) => _string(_object(raw)['type'])).toList();
    return <String>[
      'final class $name extends OpenVariant {',
      '  const $name._(super.discriminator, super.value, {required super.known});',
      '',
      '  factory $name.decode(String discriminator, Object? value) =>',
      '      $name._(discriminator, value, known: const <String>{${members.map(_literal).join(', ')}}.contains(discriminator));',
      '}',
    ];
  }
  if (const <String>{'object', 'input', 'input-object'}.contains(kind)) {
    final List<Map<String, Object?>> fields = _array(
      descriptor['fields'],
    ).map(_object).toList();
    final List<String> lines = <String>['final class $name {'];
    if (fields.isEmpty) {
      lines.add('  const $name();');
    } else {
      lines.add('  const $name({');
      for (final Map<String, Object?> field in fields) {
        lines.add('    required this.${_dartName(_string(field['name']))},');
      }
      lines.add('  });');
      lines.add('');
      for (final Map<String, Object?> field in fields) {
        final String type = _dartType(_string(field['type']), mappings);
        lines.add(
          '  final $type${field['nullable'] == true ? '?' : ''} ${_dartName(_string(field['name']))};',
        );
      }
    }
    lines.add('}');
    return lines;
  }
  _reject('DART_SDK_GENERATOR_UNSUPPORTED_TYPE');
}

List<String> _renderOperation(
  _Binding binding,
  Map<String, Object?> mappings,
  Map<String, String> typeKinds,
) {
  final String symbol = binding.symbol;
  final List<String> lines = <String>[
    'final class ${symbol}Variables {',
    '  const ${symbol}Variables({',
  ];
  for (final Map<String, Object?> variable in binding.variables) {
    final String name = _dartName(_string(variable['name']));
    if (variable['required'] == true) {
      lines.add('    required this.$name,');
    } else {
      final String type = _dartType(_string(variable['type']), mappings);
      final String nullable = variable['nullable'] == true ? '?' : '';
      lines.add('    this.$name = const InputValue<$type$nullable>.missing(),');
    }
  }
  lines.addAll(<String>['  });', '']);
  for (final Map<String, Object?> variable in binding.variables) {
    final String name = _dartName(_string(variable['name']));
    final String type = _dartType(_string(variable['type']), mappings);
    final String nullable = variable['nullable'] == true ? '?' : '';
    lines.add(
      variable['required'] == true
          ? '  final $type$nullable $name;'
          : '  final InputValue<$type$nullable> $name;',
    );
  }
  lines.addAll(<String>[
    '',
    '  Map<String, Object?> toWire() {',
    '    final Map<String, Object?> result = <String, Object?>{};',
  ]);
  for (final Map<String, Object?> variable in binding.variables) {
    final String wireName = _string(variable['name']);
    final String name = _dartName(wireName);
    final String type = _string(variable['type']);
    if (variable['required'] == true) {
      lines.add(
        "    result[${_literal(wireName)}] = encodeScalar(${_literal(_wireScalarType(type, mappings))}, $name);",
      );
    } else {
      lines.add('    if ($name.isPresent) {');
      lines.add(
        "      result[${_literal(wireName)}] = encodeScalar(${_literal(_wireScalarType(type, mappings))}, $name.value);",
      );
      lines.add('    }');
    }
  }
  lines.addAll(<String>['    return result;', '  }', '}', '']);
  lines.addAll(
    _renderResultClasses(symbol, binding.result, mappings, typeKinds),
  );
  lines.add('');
  final String function = _dartName(
    binding.name[0].toLowerCase() + binding.name.substring(1),
  );
  final String digest = _string(binding.persisted['digest']);
  lines.addAll(<String>[
    'Operation<${symbol}Variables, ${symbol}Result> $function(${symbol}Variables variables) {',
    '  return Operation<${symbol}Variables, ${symbol}Result>(',
    '    name: ${_literal(binding.name)},',
    '    kind: ${_literal(binding.kind)},',
    '    persisted: const PersistedReference(${_literal(digest)}),',
    '    variables: variables,',
    '    wireVariables: variables.toWire(),',
    '    decodeData: _decode${symbol}Result,',
    '  );',
    '}',
  ]);
  return lines;
}

List<String> _renderResultClasses(
  String symbol,
  Map<String, Object?> root,
  Map<String, Object?> mappings,
  Map<String, String> typeKinds,
) {
  final List<String> lines = <String>[];
  void render(String className, Map<String, Object?> node) {
    final List<Map<String, Object?>> fields = _array(
      node['fields'],
    ).map(_object).toList();
    for (final Map<String, Object?> field in fields) {
      final Map<String, Object?> child = _object(field['result']);
      if (child['kind'] == 'object') {
        render('$className${_pascal(_string(field['name']))}', child);
      }
    }
    lines.add('final class $className {');
    lines.add('  const $className({');
    for (final Map<String, Object?> field in fields) {
      lines.add('    required this.${_dartName(_string(field['name']))},');
    }
    lines.addAll(<String>['  });', '']);
    for (final Map<String, Object?> field in fields) {
      final Map<String, Object?> child = _object(field['result']);
      final String type = child['kind'] == 'object'
          ? '$className${_pascal(_string(field['name']))}'
          : _dartType(_string(child['type']), mappings);
      lines.add(
        '  final Selected<$type> ${_dartName(_string(field['name']))};',
      );
    }
    lines.addAll(<String>['}', '']);
    lines.add('$className _decode$className(Object? value) {');
    lines.add('  final Map<String, Object?> object = expectObject(value);');
    for (final Map<String, Object?> field in fields) {
      final String wireName = _string(field['name']);
      final String name = _dartName(wireName);
      final Map<String, Object?> child = _object(field['result']);
      final bool pending = field['presence'] == 'pending';
      final String type = child['kind'] == 'object'
          ? '$className${_pascal(wireName)}'
          : _dartType(_string(child['type']), mappings);
      final String decoder = child['kind'] == 'object'
          ? '_decode$className${_pascal(wireName)}'
          : _decoder(_string(child['type']), mappings, typeKinds);
      lines.add(
        '  final Selected<$type> $name = decodeSelected<$type>(object, ${_literal(wireName)}, pendingWhenMissing: $pending, decode: $decoder);',
      );
    }
    lines.add('  return $className(');
    for (final Map<String, Object?> field in fields) {
      final String name = _dartName(_string(field['name']));
      lines.add('    $name: $name,');
    }
    lines.addAll(<String>['  );', '}']);
  }

  render('${symbol}Result', root);
  return lines;
}

String _decoder(
  String type,
  Map<String, Object?> mappings,
  Map<String, String> typeKinds,
) {
  if (typeKinds[type] == 'enum') {
    return '(Object? value) => ${_identifier(type)}.decode(expectString(value))';
  }
  if (typeKinds[type] == 'union') {
    return '(Object? value) => ${_identifier(type)}.decode('
        'expectString(expectObject(value)[${_literal(r'$type')}]), '
        'expectObject(value)[${_literal(r'$value')}])';
  }
  final String wireType = _wireScalarType(type, mappings);
  return '(Object? value) => decodeScalar(${_literal(wireType)}, value) as ${_dartType(type, mappings)}';
}

String _dartType(String type, Map<String, Object?> mappings) {
  final Object? mapping = mappings[type];
  if (mapping != null) {
    return switch (_string(mapping)) {
      'lossless-decimal-string' => 'DecimalValue',
      'string' => 'String',
      'bigint' => 'ExtendedInteger',
      'bytes' => 'BytesValue',
      'json-raw-message' => 'Object',
      _ => throw DartGeneratorException('DART_SDK_GENERATOR_UNMAPPED_SCALAR'),
    };
  }
  return switch (type) {
    'Boolean' => 'bool',
    'Int32' => 'int',
    'Float64' => 'double',
    'Int64' || 'UInt64' || 'BigInt' => 'ExtendedInteger',
    'Decimal' || 'Money' => 'DecimalValue',
    'Timestamp' => 'TimestampValue',
    'Duration' => 'DurationValue',
    'UUID' => 'UuidValue',
    'Bytes' => 'BytesValue',
    'String' || 'ID' => 'String',
    'StringList' => 'List<String>',
    'StringMap' => 'Map<String, String>',
    _ => _declarationName(type),
  };
}

void _validateGeneratedSymbols(
  Map<String, Object?> model,
  List<_Binding> bindings,
) {
  final Set<String> topLevel = <String>{};
  final List<Map<String, Object?>> types = _array(
    _object(model['schema'])['types'],
  ).map(_object).toList();
  for (final Map<String, Object?> descriptor in types) {
    final String name = _declarationName(
      descriptor['name'] ?? descriptor['id'],
    );
    _claimSymbol(topLevel, name);
    final String kind = _string(descriptor['kind']);
    if (kind == 'enum') {
      final Set<String> members = <String>{};
      for (final Object? raw in _array(descriptor['enumMembers'])) {
        _claimSymbol(
          members,
          _dartName(_string(_object(raw)['name']).toLowerCase()),
        );
      }
    }
    if (const <String>{'object', 'input', 'input-object'}.contains(kind)) {
      _validateFieldSymbols(_array(descriptor['fields']).map(_object));
    }
  }
  for (final _Binding binding in bindings) {
    _claimSymbol(topLevel, '${binding.symbol}Variables');
    _validateFieldSymbols(binding.variables);
    _validateResultSymbols(topLevel, '${binding.symbol}Result', binding.result);
    final String function = _dartName(
      binding.name[0].toLowerCase() + binding.name.substring(1),
    );
    _claimSymbol(topLevel, function);
  }
}

void _validateResultSymbols(
  Set<String> topLevel,
  String className,
  Map<String, Object?> node,
) {
  _claimSymbol(topLevel, className);
  final List<Map<String, Object?>> fields = _array(
    node['fields'],
  ).map(_object).toList();
  _validateFieldSymbols(fields);
  for (final Map<String, Object?> field in fields) {
    final Map<String, Object?> child = _object(field['result']);
    if (child['kind'] == 'object') {
      _validateResultSymbols(
        topLevel,
        '$className${_pascal(_string(field['name']))}',
        child,
      );
    }
  }
}

void _validateFieldSymbols(Iterable<Map<String, Object?>> fields) {
  final Set<String> members = <String>{};
  for (final Map<String, Object?> field in fields) {
    _claimSymbol(members, _dartName(_string(field['name'])));
  }
}

void _claimSymbol(Set<String> scope, String symbol) {
  if (!scope.add(symbol)) _reject('DART_SDK_GENERATOR_SYMBOL_COLLISION');
}

String _wireScalarType(String type, Map<String, Object?> mappings) {
  final Object? mapping = mappings[type];
  if (mapping == null) return type;
  return switch (_string(mapping)) {
    'lossless-decimal-string' => 'Decimal',
    'string' => 'String',
    'bigint' => 'BigInt',
    'bytes' => 'Bytes',
    'json-raw-message' => 'JSON',
    _ => throw DartGeneratorException('DART_SDK_GENERATOR_UNMAPPED_SCALAR'),
  };
}

String _dartName(String value) {
  final String result = _identifier(value);
  return _reserved.contains(result) ? '${result}_' : result;
}

String _pascal(String value) {
  final String safe = _identifier(value);
  return safe[0].toUpperCase() + safe.substring(1);
}

String _identifier(Object? value) {
  final String result = _string(value);
  if (!RegExp(r'^[A-Za-z_][A-Za-z0-9_]{0,127}$').hasMatch(result)) {
    _reject('DART_SDK_GENERATOR_INVALID_IDENTIFIER');
  }
  return result;
}

String _declarationName(Object? value) {
  final String result = _identifier(value);
  if (_reserved.contains(result)) {
    _reject('DART_SDK_GENERATOR_INVALID_IDENTIFIER');
  }
  return result;
}

String _literal(String value) => jsonEncode(value).replaceAll(r'$', r'\$');

Map<String, Object?> _object(Object? value) {
  if (value is! Map<String, Object?>)
    _reject('DART_SDK_GENERATOR_INVALID_MODEL');
  return value;
}

List<Object?> _array(Object? value) {
  if (value is! List<Object?>) _reject('DART_SDK_GENERATOR_INVALID_MODEL');
  return value;
}

String _string(Object? value) {
  if (value is! String) _reject('DART_SDK_GENERATOR_INVALID_MODEL');
  return value;
}

Never _reject(String code) => throw DartGeneratorException(code);

final class _Binding {
  const _Binding({
    required this.name,
    required this.symbol,
    required this.kind,
    required this.persisted,
    required this.variables,
    required this.result,
  });

  final String name;
  final String symbol;
  final String kind;
  final Map<String, Object?> persisted;
  final List<Map<String, Object?>> variables;
  final Map<String, Object?> result;
}

const Set<String> _reserved = <String>{
  'abstract',
  'as',
  'assert',
  'async',
  'await',
  'base',
  'break',
  'case',
  'catch',
  'class',
  'const',
  'continue',
  'covariant',
  'default',
  'deferred',
  'do',
  'dynamic',
  'else',
  'enum',
  'export',
  'extends',
  'extension',
  'external',
  'factory',
  'false',
  'final',
  'finally',
  'for',
  'Function',
  'get',
  'hide',
  'if',
  'implements',
  'import',
  'in',
  'interface',
  'is',
  'late',
  'library',
  'mixin',
  'new',
  'null',
  'of',
  'on',
  'operator',
  'part',
  'required',
  'rethrow',
  'return',
  'sealed',
  'set',
  'show',
  'static',
  'super',
  'switch',
  'sync',
  'this',
  'throw',
  'true',
  'try',
  'typedef',
  'var',
  'void',
  'when',
  'while',
  'with',
  'yield',
};
