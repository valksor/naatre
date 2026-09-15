--TEST--
ext-naatre canonical codec, limits, representations, and batch entry point
--EXTENSIONS--
naatre
--FILE--
<?php
$limits = [16, 100, 1000, 1000];
echo naatre_native_canonicalize_json('{"":2,"😀":1,"a":3}', ...$limits), "\n";
var_dump(naatre_native_batch_canonicalize(['{"b":2,"a":1}', '[]'], ...$limits));
$schema = naatre_native_parse('schema', '{"types":[],"revision":"r1"}', ...$limits);
$operation = naatre_native_parse('operation', '{"operations":[]}', ...$limits);
$plan = naatre_native_compile($schema, $operation, ...$limits);
var_dump($schema->kind(), $schema->nodeCount(), strlen($plan->key()));
try { naatre_native_decode_json('{"a":1,"\u0061":2}', ...$limits); } catch (Throwable $error) { echo $error->getMessage(), "\n"; }
try { naatre_native_batch_canonicalize(['0', '0'], 16, 1, 2, 2); } catch (Throwable $error) { echo $error->getMessage(), "\n"; }
try { naatre_native_compile($schema, $operation, 16, 4, 1000, 1000); } catch (Throwable $error) { echo $error->getMessage(), "\n"; }
try { naatre_native_encode_json(str_repeat('x', 1048576), 16, 100, 1048576, 4); } catch (Throwable $error) { echo $error->getMessage(), "\n"; }
?>
--EXPECT--
{"a":3,"😀":1,"":2}
array(2) {
  [0]=>
  string(13) "{"a":1,"b":2}"
  [1]=>
  string(2) "[]"
}
string(6) "schema"
int(3)
int(64)
CLIENT_JSON_DUPLICATE_KEY
CLIENT_JSON_NODE_LIMIT_EXCEEDED
CLIENT_JSON_NODE_LIMIT_EXCEEDED
CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED
