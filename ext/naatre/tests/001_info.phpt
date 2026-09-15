--TEST--
ext-naatre exposes exact runtime metadata and conservative defaults
--EXTENSIONS--
naatre
--FILE--
<?php
$info = naatre_native_info();
var_dump($info['abiVersion'], $info['phpVersionId'] === PHP_VERSION_ID, $info['defaultEnabled'], $info['preemptiveCancellation']);
?>
--EXPECT--
int(1)
bool(true)
array(0) {
}
bool(false)
