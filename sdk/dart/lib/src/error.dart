class NaatreClientException implements Exception {
  const NaatreClientException(this.code, [Object? cause]);

  final String code;

  @override
  String toString() => 'NaatreClientException: $code';
}

Never fail(String code, [Object? cause]) {
  throw NaatreClientException(code, cause);
}
