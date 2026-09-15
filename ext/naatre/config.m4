PHP_ARG_ENABLE([naatre],
  [whether to enable ext-naatre],
  [AS_HELP_STRING([--enable-naatre], [Enable the optional Naatre accelerator])],
  [no])

if test "$PHP_NAATRE" != "no"; then
  PHP_ADD_EXTENSION_DEP(naatre, json)
  PHP_ADD_EXTENSION_DEP(naatre, hash)
  PHP_NEW_EXTENSION(naatre, naatre.c, $ext_shared,, -DZEND_ENABLE_STATIC_TSRMLS_CACHE=1)
fi
