#ifdef HAVE_CONFIG_H
#include "config.h"
#endif

#include "php.h"
#include "php_ini.h"
#include "ext/standard/info.h"
#include "ext/json/php_json.h"
#include "Zend/zend_exceptions.h"
#include "Zend/zend_smart_str.h"
#include "php_naatre.h"

#define NAATRE_ABI_VERSION 1

typedef struct {
    zend_long max_depth;
    zend_long max_nodes;
    zend_long max_bytes;
    zend_long max_output;
} naatre_limits;

typedef struct {
    const unsigned char *input;
    size_t length;
    size_t offset;
    zend_long nodes;
    naatre_limits limits;
} naatre_scanner;

typedef struct {
    zend_string *key;
    zval *value;
} naatre_member;

typedef struct {
    zend_string *kind;
    zend_string *canonical;
    zval value;
    zend_long nodes;
    zend_object std;
} naatre_representation;

typedef struct {
    zend_string *key;
    zend_string *schema;
    zend_string *operation;
    zend_object std;
} naatre_plan;

static zend_class_entry *naatre_representation_ce;
static zend_class_entry *naatre_plan_ce;
static zend_object_handlers naatre_representation_handlers;
static zend_object_handlers naatre_plan_handlers;

static inline naatre_representation *naatre_rep_from_object(zend_object *object)
{
    return (naatre_representation *)((char *) object - XtOffsetOf(naatre_representation, std));
}

static inline naatre_plan *naatre_plan_from_object(zend_object *object)
{
    return (naatre_plan *)((char *) object - XtOffsetOf(naatre_plan, std));
}

static void naatre_fail(const char *code)
{
    zend_throw_exception(zend_ce_exception, code, 0);
}

/*
 * Hard ceiling on nesting depth, independent of any caller-supplied value.
 * The scanner is mutually recursive over native C stack frames, so an
 * unbounded max_depth lets attacker- or config-controlled input exhaust the
 * fixed worker-thread stack (SIGSEGV / DoS). This ceiling caps recursion well
 * under any realistic thread-stack budget while staying above every default
 * limit in the codebase (protocol DefaultLimits=64, Native\Limits=128,
 * DuplicateKeyGuard=512).
 */
#define NAATRE_MAX_DEPTH_CEILING 512

static bool naatre_limits_valid(naatre_limits *limits)
{
    if (limits->max_depth < 1 || limits->max_nodes < 1 || limits->max_bytes < 1 || limits->max_output < 1) {
        naatre_fail("CLIENT_NATIVE_LIMIT_INVALID");
        return false;
    }
    if (limits->max_depth > NAATRE_MAX_DEPTH_CEILING) {
        naatre_fail("CLIENT_NATIVE_LIMIT_INVALID");
        return false;
    }
    return true;
}

static void naatre_space(naatre_scanner *scanner)
{
    while (scanner->offset < scanner->length) {
        unsigned char c = scanner->input[scanner->offset];
        if (c != ' ' && c != '\t' && c != '\r' && c != '\n') {
            return;
        }
        scanner->offset++;
    }
}

static bool naatre_json_decode(const char *input, size_t length, zend_long depth, zend_long options, zval *result)
{
    zval function, arguments[4];
    ZVAL_STRING(&function, "json_decode");
    ZVAL_STRINGL(&arguments[0], input, length);
    ZVAL_FALSE(&arguments[1]);
    ZVAL_LONG(&arguments[2], depth);
    ZVAL_LONG(&arguments[3], options | PHP_JSON_THROW_ON_ERROR);
    if (call_user_function(EG(function_table), NULL, &function, result, 4, arguments) == FAILURE || EG(exception)) {
        zval_ptr_dtor(&function);
        zval_ptr_dtor(&arguments[0]);
        return false;
    }
    zval_ptr_dtor(&function);
    zval_ptr_dtor(&arguments[0]);
    return true;
}

static bool naatre_scan_string(naatre_scanner *scanner, size_t *start, size_t *length)
{
    size_t begin = scanner->offset;
    if (begin >= scanner->length || scanner->input[begin] != '"') {
        return false;
    }
    scanner->offset++;
    while (scanner->offset < scanner->length) {
        unsigned char c = scanner->input[scanner->offset++];
        if (c == '"') {
            *start = begin;
            *length = scanner->offset - begin;
            return true;
        }
        if (c == '\\') {
            if (scanner->offset >= scanner->length) {
                return false;
            }
            scanner->offset++;
        } else if (c < 0x20) {
            return false;
        }
    }
    return false;
}

static bool naatre_scan_value(naatre_scanner *scanner, zend_long depth);

static bool naatre_scan_object(naatre_scanner *scanner, zend_long depth)
{
    HashTable seen;
    zend_hash_init(&seen, 8, NULL, NULL, 0);
    scanner->offset++;
    naatre_space(scanner);
    if (scanner->offset < scanner->length && scanner->input[scanner->offset] == '}') {
        scanner->offset++;
        zend_hash_destroy(&seen);
        return true;
    }
    while (scanner->offset < scanner->length) {
        size_t start = 0, length = 0;
        zval key;
        if (!naatre_scan_string(scanner, &start, &length)
            || !naatre_json_decode((const char *) scanner->input + start, length, 2, 0, &key)
            || Z_TYPE(key) != IS_STRING) {
            zend_hash_destroy(&seen);
            return false;
        }
        if (zend_hash_exists(&seen, Z_STR(key))) {
            zval_ptr_dtor(&key);
            zend_hash_destroy(&seen);
            naatre_fail("CLIENT_JSON_DUPLICATE_KEY");
            return false;
        }
        zend_hash_add_empty_element(&seen, Z_STR(key));
        zval_ptr_dtor(&key);
        naatre_space(scanner);
        if (scanner->offset >= scanner->length || scanner->input[scanner->offset++] != ':') {
            zend_hash_destroy(&seen);
            return false;
        }
        if (!naatre_scan_value(scanner, depth + 1)) {
            zend_hash_destroy(&seen);
            return false;
        }
        naatre_space(scanner);
        if (scanner->offset >= scanner->length) {
            zend_hash_destroy(&seen);
            return false;
        }
        unsigned char separator = scanner->input[scanner->offset++];
        if (separator == '}') {
            zend_hash_destroy(&seen);
            return true;
        }
        if (separator != ',') {
            zend_hash_destroy(&seen);
            return false;
        }
        naatre_space(scanner);
    }
    zend_hash_destroy(&seen);
    return false;
}

static bool naatre_scan_array(naatre_scanner *scanner, zend_long depth)
{
    scanner->offset++;
    naatre_space(scanner);
    if (scanner->offset < scanner->length && scanner->input[scanner->offset] == ']') {
        scanner->offset++;
        return true;
    }
    while (scanner->offset < scanner->length) {
        if (!naatre_scan_value(scanner, depth + 1)) {
            return false;
        }
        naatre_space(scanner);
        if (scanner->offset >= scanner->length) {
            return false;
        }
        unsigned char separator = scanner->input[scanner->offset++];
        if (separator == ']') {
            return true;
        }
        if (separator != ',') {
            return false;
        }
    }
    return false;
}

static bool naatre_scan_value(naatre_scanner *scanner, zend_long depth)
{
    size_t unused_start, unused_length;
    naatre_space(scanner);
    if (depth > scanner->limits.max_depth) {
        naatre_fail("CLIENT_JSON_DEPTH_EXCEEDED");
        return false;
    }
    if (++scanner->nodes > scanner->limits.max_nodes) {
        naatre_fail("CLIENT_JSON_NODE_LIMIT_EXCEEDED");
        return false;
    }
    if (scanner->offset >= scanner->length) {
        return false;
    }
    switch (scanner->input[scanner->offset]) {
        case '{': return naatre_scan_object(scanner, depth);
        case '[': return naatre_scan_array(scanner, depth);
        case '"': return naatre_scan_string(scanner, &unused_start, &unused_length);
        default:
            while (scanner->offset < scanner->length) {
                unsigned char c = scanner->input[scanner->offset];
                if (c == ',' || c == ']' || c == '}' || c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                    break;
                }
                scanner->offset++;
            }
            return true;
    }
}

static bool naatre_decode(zend_string *input, naatre_limits limits, zval *decoded, zend_long *nodes)
{
    naatre_scanner scanner;
    if (!naatre_limits_valid(&limits)) {
        return false;
    }
    if (ZSTR_LEN(input) > (size_t) limits.max_bytes) {
        naatre_fail("CLIENT_RESPONSE_TOO_LARGE");
        return false;
    }
    scanner.input = (const unsigned char *) ZSTR_VAL(input);
    scanner.length = ZSTR_LEN(input);
    scanner.offset = 0;
    scanner.nodes = 0;
    scanner.limits = limits;
    if (!naatre_scan_value(&scanner, 1)) {
        if (!EG(exception)) {
            naatre_fail("CLIENT_JSON_INVALID");
        }
        return false;
    }
    naatre_space(&scanner);
    if (scanner.offset != scanner.length
        || !naatre_json_decode(ZSTR_VAL(input), ZSTR_LEN(input), limits.max_depth, PHP_JSON_BIGINT_AS_STRING, decoded)) {
        if (!EG(exception)) {
            naatre_fail("CLIENT_JSON_INVALID");
        }
        return false;
    }
    *nodes = scanner.nodes;
    return true;
}

static uint32_t naatre_utf8_codepoint(const unsigned char *bytes, size_t length, size_t *offset)
{
    unsigned char c = bytes[(*offset)++];
    if (c < 0x80) return c;
    if ((c & 0xe0) == 0xc0 && *offset < length) {
        uint32_t result = ((uint32_t)(c & 0x1f) << 6) | (bytes[(*offset)++] & 0x3f);
        return result;
    }
    if ((c & 0xf0) == 0xe0 && *offset + 1 < length) {
        uint32_t result = ((uint32_t)(c & 0x0f) << 12) | ((uint32_t)(bytes[(*offset)++] & 0x3f) << 6);
        return result | (bytes[(*offset)++] & 0x3f);
    }
    if ((c & 0xf8) == 0xf0 && *offset + 2 < length) {
        uint32_t result = ((uint32_t)(c & 0x07) << 18) | ((uint32_t)(bytes[(*offset)++] & 0x3f) << 12);
        result |= ((uint32_t)(bytes[(*offset)++] & 0x3f) << 6);
        return result | (bytes[(*offset)++] & 0x3f);
    }
    return 0xfffd;
}

typedef struct {
    const unsigned char *bytes;
    size_t length;
    size_t offset;
    uint16_t pending;
} naatre_utf16_cursor;

static bool naatre_next_utf16(naatre_utf16_cursor *cursor, uint16_t *unit)
{
    if (cursor->pending != 0) {
        *unit = cursor->pending;
        cursor->pending = 0;
        return true;
    }
    if (cursor->offset >= cursor->length) return false;
    uint32_t cp = naatre_utf8_codepoint(cursor->bytes, cursor->length, &cursor->offset);
    if (cp <= 0xffff) {
        *unit = (uint16_t) cp;
    } else {
        cp -= 0x10000;
        *unit = (uint16_t)(0xd800 + (cp >> 10));
        cursor->pending = (uint16_t)(0xdc00 + (cp & 0x3ff));
    }
    return true;
}

static int naatre_compare_members(const void *left_ptr, const void *right_ptr)
{
    const naatre_member *left = (const naatre_member *) left_ptr;
    const naatre_member *right = (const naatre_member *) right_ptr;
    naatre_utf16_cursor a = {(const unsigned char *) ZSTR_VAL(left->key), ZSTR_LEN(left->key), 0, 0};
    naatre_utf16_cursor b = {(const unsigned char *) ZSTR_VAL(right->key), ZSTR_LEN(right->key), 0, 0};
    uint16_t au = 0, bu = 0;
    while (true) {
        bool ah = naatre_next_utf16(&a, &au);
        bool bh = naatre_next_utf16(&b, &bu);
        if (!ah || !bh) return ah == bh ? 0 : (ah ? 1 : -1);
        if (au != bu) return au < bu ? -1 : 1;
    }
}

static bool naatre_check_output(smart_str *output, naatre_limits limits)
{
    if (output->s != NULL && ZSTR_LEN(output->s) > (size_t) limits.max_output) {
        naatre_fail("CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED");
        return false;
    }
    return true;
}

static bool naatre_preflight_json_string(const char *value, size_t length, smart_str *output, naatre_limits limits)
{
    const unsigned char *bytes = (const unsigned char *) value;
    size_t offset = 0;
    size_t current = output->s == NULL ? 0 : ZSTR_LEN(output->s);
    size_t remaining = current > (size_t) limits.max_output ? 0 : (size_t) limits.max_output - current;
    size_t encoded = 2;
    bool exceeds = encoded > remaining;
    while (offset < length) {
        unsigned char c = bytes[offset];
        size_t sequence = 1;
        size_t added = 1;
        if (c < 0x20) {
            added = c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t' ? 2 : 6;
        } else if (c == '"' || c == '\\') {
            added = 2;
        } else if (c >= 0x80) {
            if (c >= 0xc2 && c <= 0xdf) {
                sequence = 2;
                if (offset + sequence > length || (bytes[offset + 1] & 0xc0) != 0x80) goto invalid_utf8;
            } else if (c >= 0xe0 && c <= 0xef) {
                sequence = 3;
                if (offset + sequence > length
                    || (bytes[offset + 1] & 0xc0) != 0x80
                    || (bytes[offset + 2] & 0xc0) != 0x80
                    || (c == 0xe0 && bytes[offset + 1] < 0xa0)
                    || (c == 0xed && bytes[offset + 1] >= 0xa0)) goto invalid_utf8;
            } else if (c >= 0xf0 && c <= 0xf4) {
                sequence = 4;
                if (offset + sequence > length
                    || (bytes[offset + 1] & 0xc0) != 0x80
                    || (bytes[offset + 2] & 0xc0) != 0x80
                    || (bytes[offset + 3] & 0xc0) != 0x80
                    || (c == 0xf0 && bytes[offset + 1] < 0x90)
                    || (c == 0xf4 && bytes[offset + 1] >= 0x90)) goto invalid_utf8;
            } else {
                goto invalid_utf8;
            }
            added = sequence;
        }
        if (!exceeds) {
            if (added > remaining - encoded) exceeds = true;
            else encoded += added;
        }
        offset += sequence;
    }
    if (exceeds) {
        naatre_fail("CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED");
        return false;
    }
    return true;

invalid_utf8:
    naatre_fail("CLIENT_JSON_INVALID");
    return false;
}

static bool naatre_encode_value(zval *value, smart_str *output, naatre_limits limits, zend_long depth, zend_long *nodes)
{
    if (depth > limits.max_depth) {
        naatre_fail("CLIENT_JSON_DEPTH_EXCEEDED");
        return false;
    }
    if (++(*nodes) > limits.max_nodes) {
        naatre_fail("CLIENT_JSON_NODE_LIMIT_EXCEEDED");
        return false;
    }
    ZVAL_DEREF(value);
    switch (Z_TYPE_P(value)) {
        case IS_NULL: smart_str_appends(output, "null"); break;
        case IS_TRUE: smart_str_appends(output, "true"); break;
        case IS_FALSE: smart_str_appends(output, "false"); break;
        case IS_LONG:
            if (Z_LVAL_P(value) > 9007199254740991LL || Z_LVAL_P(value) < -9007199254740991LL) {
                naatre_fail("CLIENT_JSON_UNSAFE_INTEGER");
                return false;
            }
            smart_str_append_long(output, Z_LVAL_P(value));
            break;
        case IS_DOUBLE: {
            smart_str temporary = {0};
            /*
             * Reject non-finite doubles (Inf/NaN) before encoding. PHP's
             * php_json_encode() appends the literal "0" and returns SUCCESS for
             * these (recording the error only in a side-channel error_code we do
             * not read), which would silently corrupt a non-finite value into a
             * valid-looking 0 and hash it. Fail closed, matching PureJson::encode
             * and Go's protocol/canonical.go canonicalJSONNumber.
             */
            if (!zend_finite(Z_DVAL_P(value))) {
                naatre_fail("CLIENT_JSON_INVALID");
                return false;
            }
            if (Z_DVAL_P(value) == 0.0) {
                smart_str_appendc(output, '0');
                break;
            }
            if (php_json_encode(&temporary, value, 0) == FAILURE) {
                smart_str_free(&temporary);
                naatre_fail("CLIENT_JSON_INVALID");
                return false;
            }
            smart_str_0(&temporary);
            size_t number_length = ZSTR_LEN(temporary.s);
            char *exponent = memchr(ZSTR_VAL(temporary.s), 'e', number_length);
            if (exponent != NULL && exponent - ZSTR_VAL(temporary.s) >= 2 && exponent[-2] == '.' && exponent[-1] == '0') {
                smart_str_appendl(output, ZSTR_VAL(temporary.s), (size_t)(exponent - ZSTR_VAL(temporary.s) - 2));
                smart_str_appendl(output, exponent, number_length - (size_t)(exponent - ZSTR_VAL(temporary.s)));
            } else {
                smart_str_append(output, temporary.s);
            }
            smart_str_free(&temporary);
            break;
        }
        case IS_STRING: {
            if (!naatre_preflight_json_string(Z_STRVAL_P(value), Z_STRLEN_P(value), output, limits)) return false;
            zend_string *encoded = php_json_encode_string(Z_STRVAL_P(value), Z_STRLEN_P(value), PHP_JSON_UNESCAPED_SLASHES | PHP_JSON_UNESCAPED_UNICODE);
            if (encoded == NULL) {
                naatre_fail("CLIENT_JSON_INVALID");
                return false;
            }
            smart_str_append(output, encoded);
            zend_string_release(encoded);
            break;
        }
        case IS_ARRAY: {
            zval *entry;
            bool first = true;
            if (!zend_array_is_list(Z_ARRVAL_P(value))) {
                naatre_fail("CLIENT_JSON_INVALID");
                return false;
            }
            smart_str_appendc(output, '[');
            ZEND_HASH_FOREACH_VAL(Z_ARRVAL_P(value), entry) {
                if (!first) smart_str_appendc(output, ',');
                first = false;
                if (!naatre_encode_value(entry, output, limits, depth + 1, nodes)) return false;
            } ZEND_HASH_FOREACH_END();
            smart_str_appendc(output, ']');
            break;
        }
        case IS_OBJECT: {
            HashTable *properties = Z_OBJPROP_P(value);
            uint32_t count = zend_hash_num_elements(properties), index = 0;
            if ((zend_ulong) count > (zend_ulong)(limits.max_nodes - *nodes)) {
                naatre_fail("CLIENT_JSON_NODE_LIMIT_EXCEEDED");
                return false;
            }
            naatre_member *members = count == 0 ? NULL : safe_emalloc(count, sizeof(naatre_member), 0);
            zend_string *key;
            zval *entry;
            ZEND_HASH_FOREACH_STR_KEY_VAL(properties, key, entry) {
                if (key == NULL) {
                    if (members != NULL) efree(members);
                    naatre_fail("CLIENT_JSON_INVALID");
                    return false;
                }
                members[index].key = key;
                members[index].value = entry;
                index++;
            } ZEND_HASH_FOREACH_END();
            if (count > 1) qsort(members, count, sizeof(naatre_member), naatre_compare_members);
            smart_str_appendc(output, '{');
            for (index = 0; index < count; index++) {
                zend_string *encoded_key;
                if (index != 0) smart_str_appendc(output, ',');
                if (!naatre_preflight_json_string(ZSTR_VAL(members[index].key), ZSTR_LEN(members[index].key), output, limits)) {
                    if (members != NULL) efree(members);
                    return false;
                }
                encoded_key = php_json_encode_string(ZSTR_VAL(members[index].key), ZSTR_LEN(members[index].key), PHP_JSON_UNESCAPED_SLASHES | PHP_JSON_UNESCAPED_UNICODE);
                if (encoded_key == NULL) {
                    if (members != NULL) efree(members);
                    naatre_fail("CLIENT_JSON_INVALID");
                    return false;
                }
                smart_str_append(output, encoded_key);
                zend_string_release(encoded_key);
                smart_str_appendc(output, ':');
                if (!naatre_encode_value(members[index].value, output, limits, depth + 1, nodes)) {
                    if (members != NULL) efree(members);
                    return false;
                }
            }
            if (members != NULL) efree(members);
            smart_str_appendc(output, '}');
            break;
        }
        default:
            naatre_fail("CLIENT_JSON_INVALID");
            return false;
    }
    return naatre_check_output(output, limits);
}

static zend_string *naatre_encode(zval *value, naatre_limits limits, zend_long *node_count)
{
    smart_str output = {0};
    zend_long nodes = 0;
    if (!naatre_limits_valid(&limits) || !naatre_encode_value(value, &output, limits, 1, &nodes)) {
        smart_str_free(&output);
        return NULL;
    }
    smart_str_0(&output);
    *node_count = nodes;
    return output.s;
}

static bool naatre_call_hash(zend_string *purpose, zend_string *canonical, zend_string **digest)
{
    static const char *allowed[] = {"document", "schema", "approval", "result-cache", "idempotency", "signed-message", "federation"};
    bool accepted = false;
    size_t i;
    zval function, arguments[2], result;
    smart_str input = {0};
    for (i = 0; i < sizeof(allowed) / sizeof(allowed[0]); i++) {
        if (zend_string_equals_cstr(purpose, allowed[i], strlen(allowed[i]))) {
            accepted = true;
            break;
        }
    }
    if (!accepted) {
        naatre_fail("CLIENT_NATIVE_PURPOSE_INVALID");
        return false;
    }
    smart_str_appends(&input, "naatre:");
    smart_str_append(&input, purpose);
    smart_str_appends(&input, ":c14n-1\n");
    smart_str_append(&input, canonical);
    smart_str_0(&input);
    ZVAL_STRING(&function, "hash");
    ZVAL_STRING(&arguments[0], "sha256");
    ZVAL_STR_COPY(&arguments[1], input.s);
    if (call_user_function(EG(function_table), NULL, &function, &result, 2, arguments) == FAILURE || Z_TYPE(result) != IS_STRING) {
        zval_ptr_dtor(&function);
        zval_ptr_dtor(&arguments[0]);
        zval_ptr_dtor(&arguments[1]);
        smart_str_free(&input);
        naatre_fail("CLIENT_NATIVE_HASH_UNAVAILABLE");
        return false;
    }
    *digest = zend_string_copy(Z_STR(result));
    zval_ptr_dtor(&result);
    zval_ptr_dtor(&function);
    zval_ptr_dtor(&arguments[0]);
    zval_ptr_dtor(&arguments[1]);
    smart_str_free(&input);
    return true;
}

static zend_object *naatre_representation_create(zend_class_entry *ce)
{
    naatre_representation *representation = zend_object_alloc(sizeof(naatre_representation), ce);
    representation->kind = NULL;
    representation->canonical = NULL;
    ZVAL_UNDEF(&representation->value);
    representation->nodes = 0;
    zend_object_std_init(&representation->std, ce);
    object_properties_init(&representation->std, ce);
    representation->std.handlers = &naatre_representation_handlers;
    return &representation->std;
}

static void naatre_representation_free(zend_object *object)
{
    naatre_representation *representation = naatre_rep_from_object(object);
    if (representation->kind != NULL) zend_string_release(representation->kind);
    if (representation->canonical != NULL) zend_string_release(representation->canonical);
    if (!Z_ISUNDEF(representation->value)) zval_ptr_dtor(&representation->value);
    zend_object_std_dtor(&representation->std);
}

static zend_object *naatre_plan_create(zend_class_entry *ce)
{
    naatre_plan *plan = zend_object_alloc(sizeof(naatre_plan), ce);
    plan->key = NULL;
    plan->schema = NULL;
    plan->operation = NULL;
    zend_object_std_init(&plan->std, ce);
    object_properties_init(&plan->std, ce);
    plan->std.handlers = &naatre_plan_handlers;
    return &plan->std;
}

static void naatre_plan_free(zend_object *object)
{
    naatre_plan *plan = naatre_plan_from_object(object);
    if (plan->key != NULL) zend_string_release(plan->key);
    if (plan->schema != NULL) zend_string_release(plan->schema);
    if (plan->operation != NULL) zend_string_release(plan->operation);
    zend_object_std_dtor(&plan->std);
}

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_info, 0, 0, IS_ARRAY, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_json, 0, 5, IS_MIXED, 0)
    ZEND_ARG_TYPE_INFO(0, input, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, maximumDepth, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumNodes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumBytes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumOutputBytes, IS_LONG, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_encode, 0, 5, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, value, IS_MIXED, 0)
    ZEND_ARG_TYPE_INFO(0, maximumDepth, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumNodes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumBytes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumOutputBytes, IS_LONG, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_batch, 0, 5, IS_ARRAY, 0)
    ZEND_ARG_TYPE_INFO(0, inputs, IS_ARRAY, 0)
    ZEND_ARG_TYPE_INFO(0, maximumDepth, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumNodes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumBytes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumOutputBytes, IS_LONG, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_hash, 0, 6, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, purpose, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, canonical, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, maximumDepth, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumNodes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumBytes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumOutputBytes, IS_LONG, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_parse, 0, 6, IS_OBJECT, 0)
    ZEND_ARG_TYPE_INFO(0, kind, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, input, IS_STRING, 0)
    ZEND_ARG_TYPE_INFO(0, maximumDepth, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumNodes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumBytes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumOutputBytes, IS_LONG, 0)
ZEND_END_ARG_INFO()

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_naatre_compile, 0, 6, IS_OBJECT, 0)
    ZEND_ARG_TYPE_INFO(0, schema, IS_OBJECT, 0)
    ZEND_ARG_TYPE_INFO(0, operation, IS_OBJECT, 0)
    ZEND_ARG_TYPE_INFO(0, maximumDepth, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumNodes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumBytes, IS_LONG, 0)
    ZEND_ARG_TYPE_INFO(0, maximumOutputBytes, IS_LONG, 0)
ZEND_END_ARG_INFO()

PHP_FUNCTION(naatre_native_info)
{
    ZEND_PARSE_PARAMETERS_NONE();
    array_init(return_value);
    add_assoc_string(return_value, "extensionVersion", PHP_NAATRE_VERSION);
    add_assoc_long(return_value, "abiVersion", NAATRE_ABI_VERSION);
    add_assoc_long(return_value, "phpVersionId", PHP_VERSION_ID);
    add_assoc_long(return_value, "phpApi", PHP_API_VERSION);
    add_assoc_long(return_value, "zendModuleApi", ZEND_MODULE_API_NO);
    add_assoc_bool(return_value, "zts", ZTS_V != 0);
#if ZEND_DEBUG
    add_assoc_bool(return_value, "debug", true);
#else
    add_assoc_bool(return_value, "debug", false);
#endif
    add_assoc_long(return_value, "pointerBits", SIZEOF_ZEND_LONG * 8);
    zval capabilities, defaults;
    array_init(&capabilities);
    add_next_index_string(&capabilities, "json-codec");
    add_next_index_string(&capabilities, "canonical-json");
    add_next_index_string(&capabilities, "semantic-hash");
    add_next_index_string(&capabilities, "document-representation");
    add_next_index_string(&capabilities, "operation-plan");
    add_next_index_string(&capabilities, "lossless-values");
    add_next_index_string(&capabilities, "structural-validation");
    add_next_index_string(&capabilities, "batch-canonical-json");
    add_assoc_zval(return_value, "capabilities", &capabilities);
    array_init(&defaults);
    add_assoc_zval(return_value, "defaultEnabled", &defaults);
    add_assoc_bool(return_value, "preemptiveCancellation", false);
    add_assoc_string(return_value, "cacheOwnership", "request-only-no-persistent-cache");
}

PHP_FUNCTION(naatre_native_decode_json)
{
    zend_string *input;
    naatre_limits limits;
    zend_long nodes;
    zval decoded;
    ZVAL_UNDEF(&decoded);
    ZEND_PARSE_PARAMETERS_START(5, 5)
        Z_PARAM_STR(input)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!naatre_decode(input, limits, &decoded, &nodes)) RETURN_THROWS();
    zend_string *check = naatre_encode(&decoded, limits, &nodes);
    if (check == NULL) {
        zval_ptr_dtor(&decoded);
        RETURN_THROWS();
    }
    zend_string_release(check);
    RETURN_COPY_VALUE(&decoded);
}

PHP_FUNCTION(naatre_native_encode_json)
{
    zval *value;
    naatre_limits limits;
    zend_long nodes;
    ZEND_PARSE_PARAMETERS_START(5, 5)
        Z_PARAM_ZVAL(value)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    zend_string *encoded = naatre_encode(value, limits, &nodes);
    if (encoded == NULL) RETURN_THROWS();
    RETURN_STR(encoded);
}

PHP_FUNCTION(naatre_native_canonicalize_json)
{
    zend_string *input;
    naatre_limits limits;
    zend_long parsed_nodes, encoded_nodes;
    zval decoded;
    ZVAL_UNDEF(&decoded);
    ZEND_PARSE_PARAMETERS_START(5, 5)
        Z_PARAM_STR(input)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!naatre_decode(input, limits, &decoded, &parsed_nodes)) RETURN_THROWS();
    zend_string *encoded = naatre_encode(&decoded, limits, &encoded_nodes);
    zval_ptr_dtor(&decoded);
    if (encoded == NULL) RETURN_THROWS();
    RETURN_STR(encoded);
}

PHP_FUNCTION(naatre_native_validate_json)
{
    zend_string *input;
    naatre_limits limits;
    zend_long nodes;
    zval decoded;
    ZVAL_UNDEF(&decoded);
    ZEND_PARSE_PARAMETERS_START(5, 5)
        Z_PARAM_STR(input)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!naatre_decode(input, limits, &decoded, &nodes)) RETURN_THROWS();
    zval_ptr_dtor(&decoded);
    RETURN_TRUE;
}

PHP_FUNCTION(naatre_native_batch_canonicalize)
{
    HashTable *inputs;
    naatre_limits limits, remaining;
    zval *input;
    ZEND_PARSE_PARAMETERS_START(5, 5)
        Z_PARAM_ARRAY_HT(inputs)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!naatre_limits_valid(&limits)) RETURN_THROWS();
    if ((zend_ulong) zend_hash_num_elements(inputs) > (zend_ulong) limits.max_nodes) {
        naatre_fail("CLIENT_JSON_NODE_LIMIT_EXCEEDED");
        RETURN_THROWS();
    }
    remaining = limits;
    array_init_size(return_value, zend_hash_num_elements(inputs));
    ZEND_HASH_FOREACH_VAL(inputs, input) {
        zval decoded;
        zend_long parsed_nodes, encoded_nodes;
        zend_string *encoded;
        ZVAL_DEREF(input);
        ZVAL_UNDEF(&decoded);
        if (remaining.max_nodes < 1) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            naatre_fail("CLIENT_JSON_NODE_LIMIT_EXCEEDED");
            RETURN_THROWS();
        }
        if (remaining.max_bytes < 1) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            naatre_fail("CLIENT_RESPONSE_TOO_LARGE");
            RETURN_THROWS();
        }
        if (remaining.max_output < 1) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            naatre_fail("CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED");
            RETURN_THROWS();
        }
        if (Z_TYPE_P(input) != IS_STRING) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            naatre_fail("CLIENT_JSON_INVALID");
            RETURN_THROWS();
        }
        if (Z_STRLEN_P(input) > (size_t) remaining.max_bytes) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            naatre_fail("CLIENT_RESPONSE_TOO_LARGE");
            RETURN_THROWS();
        }
        if (!naatre_decode(Z_STR_P(input), remaining, &decoded, &parsed_nodes)) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            RETURN_THROWS();
        }
        encoded = naatre_encode(&decoded, remaining, &encoded_nodes);
        zval_ptr_dtor(&decoded);
        if (encoded == NULL) {
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            RETURN_THROWS();
        }
        if (ZSTR_LEN(encoded) > (size_t) remaining.max_output) {
            zend_string_release(encoded);
            zval_ptr_dtor(return_value);
            ZVAL_UNDEF(return_value);
            naatre_fail("CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED");
            RETURN_THROWS();
        }
        remaining.max_bytes -= Z_STRLEN_P(input);
        remaining.max_output -= ZSTR_LEN(encoded);
        remaining.max_nodes -= parsed_nodes;
        add_next_index_str(return_value, encoded);
    } ZEND_HASH_FOREACH_END();
}

PHP_FUNCTION(naatre_native_semantic_hash)
{
    zend_string *purpose, *canonical, *digest;
    naatre_limits limits;
    ZEND_PARSE_PARAMETERS_START(6, 6)
        Z_PARAM_STR(purpose)
        Z_PARAM_STR(canonical)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!naatre_limits_valid(&limits)) RETURN_THROWS();
    if (ZSTR_LEN(canonical) > (size_t) limits.max_bytes || ZSTR_LEN(canonical) > (size_t) limits.max_output) {
        naatre_fail("CLIENT_RESPONSE_TOO_LARGE");
        RETURN_THROWS();
    }
    if (!naatre_call_hash(purpose, canonical, &digest)) RETURN_THROWS();
    RETURN_STR(digest);
}

PHP_FUNCTION(naatre_native_parse)
{
    zend_string *kind, *input;
    naatre_limits limits;
    zend_long parsed_nodes, encoded_nodes;
    zval decoded;
    ZVAL_UNDEF(&decoded);
    ZEND_PARSE_PARAMETERS_START(6, 6)
        Z_PARAM_STR(kind)
        Z_PARAM_STR(input)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!zend_string_equals_literal(kind, "schema") && !zend_string_equals_literal(kind, "operation")) {
        naatre_fail("CLIENT_NATIVE_KIND_INVALID");
        RETURN_THROWS();
    }
    if (!naatre_decode(input, limits, &decoded, &parsed_nodes) || Z_TYPE(decoded) != IS_OBJECT) {
        if (!EG(exception)) naatre_fail("CLIENT_NATIVE_DOCUMENT_INVALID");
        if (!Z_ISUNDEF(decoded)) zval_ptr_dtor(&decoded);
        RETURN_THROWS();
    }
    zend_string *canonical = naatre_encode(&decoded, limits, &encoded_nodes);
    if (canonical == NULL) {
        zval_ptr_dtor(&decoded);
        RETURN_THROWS();
    }
    object_init_ex(return_value, naatre_representation_ce);
    naatre_representation *representation = naatre_rep_from_object(Z_OBJ_P(return_value));
    representation->kind = zend_string_copy(kind);
    representation->canonical = canonical;
    ZVAL_COPY_VALUE(&representation->value, &decoded);
    representation->nodes = parsed_nodes;
}

PHP_FUNCTION(naatre_native_compile)
{
    zval *schema_value, *operation_value;
    naatre_limits limits;
    zend_string *digest;
    ZEND_PARSE_PARAMETERS_START(6, 6)
        Z_PARAM_OBJECT_OF_CLASS(schema_value, naatre_representation_ce)
        Z_PARAM_OBJECT_OF_CLASS(operation_value, naatre_representation_ce)
        Z_PARAM_LONG(limits.max_depth)
        Z_PARAM_LONG(limits.max_nodes)
        Z_PARAM_LONG(limits.max_bytes)
        Z_PARAM_LONG(limits.max_output)
    ZEND_PARSE_PARAMETERS_END();
    if (!naatre_limits_valid(&limits)) RETURN_THROWS();
    naatre_representation *schema = naatre_rep_from_object(Z_OBJ_P(schema_value));
    naatre_representation *operation = naatre_rep_from_object(Z_OBJ_P(operation_value));
    if (!zend_string_equals_literal(schema->kind, "schema") || !zend_string_equals_literal(operation->kind, "operation")) {
        naatre_fail("CLIENT_NATIVE_PLAN_INVALID");
        RETURN_THROWS();
    }
    if (schema->nodes > limits.max_nodes || operation->nodes > limits.max_nodes - schema->nodes) {
        naatre_fail("CLIENT_JSON_NODE_LIMIT_EXCEEDED");
        RETURN_THROWS();
    }
    size_t schema_bytes = ZSTR_LEN(schema->canonical);
    size_t operation_bytes = ZSTR_LEN(operation->canonical);
    size_t maximum_bytes = (size_t) limits.max_bytes;
    if (schema_bytes >= maximum_bytes || operation_bytes > maximum_bytes - schema_bytes - 1) {
        naatre_fail("CLIENT_RESPONSE_TOO_LARGE");
        RETURN_THROWS();
    }
    size_t maximum_output = (size_t) limits.max_output;
    if (schema_bytes > maximum_output || operation_bytes > maximum_output - schema_bytes) {
        naatre_fail("CLIENT_JSON_OUTPUT_LIMIT_EXCEEDED");
        RETURN_THROWS();
    }
    smart_str key_input = {0};
    smart_str_append(&key_input, schema->canonical);
    smart_str_appendc(&key_input, '\n');
    smart_str_append(&key_input, operation->canonical);
    smart_str_0(&key_input);
    zend_string *compile_purpose = zend_string_init("document", sizeof("document") - 1, 0);
    if (!naatre_call_hash(compile_purpose, key_input.s, &digest)) {
        zend_string_release(compile_purpose);
        smart_str_free(&key_input);
        RETURN_THROWS();
    }
    zend_string_release(compile_purpose);
    smart_str_free(&key_input);
    object_init_ex(return_value, naatre_plan_ce);
    naatre_plan *plan = naatre_plan_from_object(Z_OBJ_P(return_value));
    plan->key = digest;
    plan->schema = zend_string_copy(schema->canonical);
    plan->operation = zend_string_copy(operation->canonical);
}

ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_rep_string, 0, 0, IS_STRING, 0)
ZEND_END_ARG_INFO()
ZEND_BEGIN_ARG_WITH_RETURN_TYPE_INFO_EX(arginfo_rep_nodes, 0, 0, IS_LONG, 0)
ZEND_END_ARG_INFO()
ZEND_BEGIN_ARG_INFO_EX(arginfo_native_constructor, 0, 0, 0)
ZEND_END_ARG_INFO()

PHP_METHOD(NaatreNativeRepresentation, kind)
{
    ZEND_PARSE_PARAMETERS_NONE();
    RETURN_STR_COPY(naatre_rep_from_object(Z_OBJ_P(ZEND_THIS))->kind);
}

PHP_METHOD(NaatreNativeRepresentation, canonical)
{
    ZEND_PARSE_PARAMETERS_NONE();
    RETURN_STR_COPY(naatre_rep_from_object(Z_OBJ_P(ZEND_THIS))->canonical);
}

PHP_METHOD(NaatreNativeRepresentation, nodeCount)
{
    ZEND_PARSE_PARAMETERS_NONE();
    RETURN_LONG(naatre_rep_from_object(Z_OBJ_P(ZEND_THIS))->nodes);
}

PHP_METHOD(NaatreNativePlan, key)
{
    ZEND_PARSE_PARAMETERS_NONE();
    RETURN_STR_COPY(naatre_plan_from_object(Z_OBJ_P(ZEND_THIS))->key);
}

PHP_METHOD(NaatreNativePlan, schema)
{
    ZEND_PARSE_PARAMETERS_NONE();
    RETURN_STR_COPY(naatre_plan_from_object(Z_OBJ_P(ZEND_THIS))->schema);
}

PHP_METHOD(NaatreNativePlan, operation)
{
    ZEND_PARSE_PARAMETERS_NONE();
    RETURN_STR_COPY(naatre_plan_from_object(Z_OBJ_P(ZEND_THIS))->operation);
}

PHP_METHOD(NaatreNativeObject, __construct)
{
    (void) return_value;
    ZEND_PARSE_PARAMETERS_NONE();
    zend_throw_error(NULL, "Naatre native objects can only be created by ext-naatre");
}

static const zend_function_entry naatre_representation_methods[] = {
    PHP_ME(NaatreNativeObject, __construct, arginfo_native_constructor, ZEND_ACC_PRIVATE)
    PHP_ME(NaatreNativeRepresentation, kind, arginfo_rep_string, ZEND_ACC_PUBLIC)
    PHP_ME(NaatreNativeRepresentation, canonical, arginfo_rep_string, ZEND_ACC_PUBLIC)
    PHP_ME(NaatreNativeRepresentation, nodeCount, arginfo_rep_nodes, ZEND_ACC_PUBLIC)
    PHP_FE_END
};

static const zend_function_entry naatre_plan_methods[] = {
    PHP_ME(NaatreNativeObject, __construct, arginfo_native_constructor, ZEND_ACC_PRIVATE)
    PHP_ME(NaatreNativePlan, key, arginfo_rep_string, ZEND_ACC_PUBLIC)
    PHP_ME(NaatreNativePlan, schema, arginfo_rep_string, ZEND_ACC_PUBLIC)
    PHP_ME(NaatreNativePlan, operation, arginfo_rep_string, ZEND_ACC_PUBLIC)
    PHP_FE_END
};

static const zend_function_entry naatre_functions[] = {
    PHP_FE(naatre_native_info, arginfo_naatre_info)
    PHP_FE(naatre_native_decode_json, arginfo_naatre_json)
    PHP_FE(naatre_native_encode_json, arginfo_naatre_encode)
    PHP_FE(naatre_native_canonicalize_json, arginfo_naatre_json)
    PHP_FE(naatre_native_validate_json, arginfo_naatre_json)
    PHP_FE(naatre_native_batch_canonicalize, arginfo_naatre_batch)
    PHP_FE(naatre_native_semantic_hash, arginfo_naatre_hash)
    PHP_FE(naatre_native_parse, arginfo_naatre_parse)
    PHP_FE(naatre_native_compile, arginfo_naatre_compile)
    PHP_FE_END
};

PHP_MINIT_FUNCTION(naatre)
{
    zend_class_entry representation_entry, plan_entry;
    (void) type;
    (void) module_number;
    INIT_NS_CLASS_ENTRY(representation_entry, "Naatre\\Native", "Representation", naatre_representation_methods);
    naatre_representation_ce = zend_register_internal_class_with_flags(&representation_entry, NULL, ZEND_ACC_FINAL | ZEND_ACC_NO_DYNAMIC_PROPERTIES | ZEND_ACC_NOT_SERIALIZABLE);
    naatre_representation_ce->create_object = naatre_representation_create;
    memcpy(&naatre_representation_handlers, zend_get_std_object_handlers(), sizeof(zend_object_handlers));
    naatre_representation_handlers.offset = XtOffsetOf(naatre_representation, std);
    naatre_representation_handlers.free_obj = naatre_representation_free;
    naatre_representation_handlers.clone_obj = NULL;

    INIT_NS_CLASS_ENTRY(plan_entry, "Naatre\\Native", "Plan", naatre_plan_methods);
    naatre_plan_ce = zend_register_internal_class_with_flags(&plan_entry, NULL, ZEND_ACC_FINAL | ZEND_ACC_NO_DYNAMIC_PROPERTIES | ZEND_ACC_NOT_SERIALIZABLE);
    naatre_plan_ce->create_object = naatre_plan_create;
    memcpy(&naatre_plan_handlers, zend_get_std_object_handlers(), sizeof(zend_object_handlers));
    naatre_plan_handlers.offset = XtOffsetOf(naatre_plan, std);
    naatre_plan_handlers.free_obj = naatre_plan_free;
    naatre_plan_handlers.clone_obj = NULL;
    return SUCCESS;
}

PHP_MSHUTDOWN_FUNCTION(naatre)
{
    (void) type;
    (void) module_number;
    return SUCCESS;
}

PHP_RINIT_FUNCTION(naatre)
{
    (void) type;
    (void) module_number;
#if defined(ZTS) && defined(COMPILE_DL_NAATRE)
    ZEND_TSRMLS_CACHE_UPDATE();
#endif
    return SUCCESS;
}

PHP_RSHUTDOWN_FUNCTION(naatre)
{
    (void) type;
    (void) module_number;
    return SUCCESS;
}

PHP_MINFO_FUNCTION(naatre)
{
    (void) zend_module;
    php_info_print_table_start();
    php_info_print_table_header(2, "Naatre accelerator", "enabled");
    php_info_print_table_row(2, "Version", PHP_NAATRE_VERSION);
    php_info_print_table_row(2, "ABI", "1");
    php_info_print_table_row(2, "Persistent cache", "disabled");
    php_info_print_table_row(2, "Preemptive cancellation", "not advertised");
    php_info_print_table_end();
}

zend_module_entry naatre_module_entry = {
    STANDARD_MODULE_HEADER,
    "naatre",
    naatre_functions,
    PHP_MINIT(naatre),
    PHP_MSHUTDOWN(naatre),
    PHP_RINIT(naatre),
    PHP_RSHUTDOWN(naatre),
    PHP_MINFO(naatre),
    PHP_NAATRE_VERSION,
    STANDARD_MODULE_PROPERTIES
};

#ifdef ZTS
ZEND_TSRMLS_CACHE_DEFINE()
#endif
ZEND_GET_MODULE(naatre)
