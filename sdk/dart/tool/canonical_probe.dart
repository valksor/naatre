// ignore_for_file: avoid_print

import 'dart:convert';

import 'package:naatre/generated.dart';
import 'package:naatre/naatre.dart';

void main() {
  final Operation<GetAccountVariables, GetAccountResult> operation = getAccount(
    const GetAccountVariables(
      id: 'acct-1',
      nickname: InputValue<String?>.present(null),
      tags: InputValue<List<String>>.present(<String>[]),
      filter: InputValue<Map<String, String>>.present(<String, String>{}),
    ),
  );
  bool unsafeRejected = false;
  try {
    canonicalJson(<String, Object?>{'value': 9007199254740992});
  } on NaatreClientException catch (error) {
    unsafeRejected = error.code == 'CLIENT_VALUE_PRECISION';
  }
  bool unsafeExtendedRejected = false;
  try {
    ExtendedInteger.bigInt(9007199254740992);
  } on NaatreClientException catch (error) {
    unsafeExtendedRejected = error.code == 'CLIENT_VALUE_PRECISION';
  }
  print(
    canonicalJson(<String, Object?>{
      'documentHash': semanticHash(
        'document',
        (parseStrictJson(
              utf8.encode(
                '{"operations":[{"kind":"query","name":"GetAccount","select":[{"\\u0024call":{"args":{"id":{"\\u0024var":"id"}},"as":"profile","name":"account","select":[{"\\u0024field":{"as":"display","name":"displayName"}},{"\\u0024field":{"name":"nickname"}}]}},{"\\u0024call":{"as":"later","name":"audit"}}],"variables":[{"name":"id","required":true,"type":"ID"},{"name":"nickname","nullable":true,"type":"String"},{"name":"tags","type":"StringList"},{"name":"filter","type":"StringMap"}]}]}',
              ),
            )
            as Map<String, Object?>),
      ),
      'request': utf8.decode(operation.canonicalRequest()),
      'sampleHash': semanticHash('document', <String, Object?>{'x': true}),
      'unsafeExtendedIntegerRejected': unsafeExtendedRejected,
      'unsafeIntegerRejected': unsafeRejected,
    }),
  );
}
