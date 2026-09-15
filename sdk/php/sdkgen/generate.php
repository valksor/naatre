<?php

declare(strict_types=1);

const MAXIMUM_INPUT_BYTES = 8_388_608;

/** @return array<string, mixed> */
function document(string $path): array
{
    $bytes = file_get_contents($path);
    if ($bytes === false || $bytes === '' || strlen($bytes) > MAXIMUM_INPUT_BYTES) {
        fail('PHP_SDK_GENERATOR_INPUT_LIMIT');
    }
    try {
        $value = json_decode($bytes, true, 512, JSON_THROW_ON_ERROR | JSON_BIGINT_AS_STRING);
    } catch (\JsonException) {
        fail('PHP_SDK_GENERATOR_INVALID_INPUT');
    }
    if (!is_array($value) || array_is_list($value)) {
        fail('PHP_SDK_GENERATOR_INVALID_INPUT');
    }
    $document = [];
    foreach ($value as $key => $member) {
        if (!is_string($key)) {
            fail('PHP_SDK_GENERATOR_INVALID_INPUT');
        }
        $document[$key] = $member;
    }
    return $document;
}

/**
 * @param array<string, mixed> $model
 * @param array<string, mixed> $reference
 * @return array{string, string, string}
 */
function generate(array $model, array $reference): array
{
    if (($model['version'] ?? null) !== 'naatre.generator-model-1'
        || ($model['protocolVersion'] ?? null) !== '1'
        || ($model['canonicalVersion'] ?? null) !== 'c14n-1'
        || ($reference['modelVersion'] ?? null) !== $model['version']
        || ($reference['protocolVersion'] ?? null) !== $model['protocolVersion']
        || ($reference['canonicalVersion'] ?? null) !== $model['canonicalVersion']
        || ($reference['generatorVersion'] ?? null) !== 'naatre.generator.reference-json-1') {
        fail('PHP_SDK_GENERATOR_VERSION_SKEW');
    }
    $operations = $model['operations'] ?? null;
    $referenceOperations = $reference['operations'] ?? null;
    if (!is_array($operations) || !array_is_list($operations) || count($operations) !== 1
        || !is_array($referenceOperations) || !array_is_list($referenceOperations) || count($referenceOperations) !== 1) {
        fail('PHP_SDK_GENERATOR_UNSUPPORTED_MODEL');
    }
    $operation = $operations[0];
    $referenceOperation = $referenceOperations[0];
    if (!is_array($operation) || array_is_list($operation)
        || !is_array($referenceOperation) || array_is_list($referenceOperation)) {
        fail('PHP_SDK_GENERATOR_INVALID_OPERATION');
    }
    $operationDocument = [];
    foreach ($operation as $key => $member) {
        if (!is_string($key)) {
            fail('PHP_SDK_GENERATOR_INVALID_OPERATION');
        }
        $operationDocument[$key] = $member;
    }
    $operation = $operationDocument;
    $name = identifier($operation['name'] ?? null);
    if (($referenceOperation['name'] ?? null) !== $name || ($referenceOperation['symbol'] ?? null) !== $name) {
        fail('PHP_SDK_GENERATOR_REFERENCE_DRIFT');
    }
    validateSchema($model);
    validateOperation($operation);
    $persisted = $referenceOperation['persisted'] ?? null;
    if (!is_array($persisted) || ($persisted['algorithm'] ?? null) !== 'sha-256'
        || ($persisted['canonicalVersion'] ?? null) !== 'c14n-1'
        || !is_string($persisted['digest'] ?? null)
        || preg_match('/^[a-f0-9]{64}$/', $persisted['digest']) !== 1) {
        fail('PHP_SDK_GENERATOR_REFERENCE_DRIFT');
    }
    $source = str_replace(
        ['{{OPERATION}}', '{{OPERATION_LOWER}}', '{{DIGEST}}'],
        [$name, lcfirst($name), $persisted['digest']],
        template(),
    );
    $handlerSource = str_replace('{{OPERATION}}', $name, handlerTemplate());
    $manifest = [
        'canonicalVersion' => 'c14n-1',
        'operations' => [[
            'kind' => 'query',
            'name' => $name,
            'persisted' => $persisted,
        ]],
        'profile' => 'sdk.php.core-1',
        'protocolVersion' => '1',
        'version' => '1',
    ];
    try {
        $manifestBytes = json_encode($manifest, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES) . "\n";
    } catch (\JsonException) {
        fail('PHP_SDK_GENERATOR_INVALID_OUTPUT');
    }
    return [$source, $manifestBytes, $handlerSource];
}

/** @param array<string, mixed> $model */
function validateSchema(array $model): void
{
    $configuration = $model['configuration'] ?? null;
    $mappings = is_array($configuration) ? ($configuration['scalarMappings'] ?? null) : null;
    $schema = $model['schema'] ?? null;
    $types = is_array($schema) ? ($schema['types'] ?? null) : null;
    if (!is_array($mappings) || array_is_list($mappings) || !is_array($types) || !array_is_list($types)) {
        fail('PHP_SDK_GENERATOR_INVALID_SCHEMA');
    }
    $symbols = [];
    foreach ($types as $type) {
        if (!is_array($type)) {
            fail('PHP_SDK_GENERATOR_INVALID_SCHEMA');
        }
        $name = identifier($type['name'] ?? null);
        if (isset($symbols[strtolower($name)])) {
            fail('PHP_SDK_GENERATOR_SYMBOL_COLLISION');
        }
        $symbols[strtolower($name)] = true;
        $typeID = $type['id'] ?? null;
        if (!is_string($typeID)) {
            fail('PHP_SDK_GENERATOR_INVALID_SCHEMA');
        }
        if (($type['kind'] ?? null) === 'scalar' && !array_key_exists($typeID, $mappings)) {
            fail('PHP_SDK_GENERATOR_UNMAPPED_SCALAR');
        }
        if (($type['kind'] ?? null) === 'enum' && ($type['open'] ?? null) !== true) {
            fail('PHP_SDK_GENERATOR_UNSUPPORTED_MODEL');
        }
    }
}

/** @param array<string, mixed> $operation */
function validateOperation(array $operation): void
{
    $variables = $operation['variables'] ?? null;
    $result = $operation['result'] ?? null;
    if (!is_array($variables) || !array_is_list($variables) || !is_array($result) || ($result['kind'] ?? null) !== 'object') {
        fail('PHP_SDK_GENERATOR_INVALID_OPERATION');
    }
    $seen = [];
    foreach ($variables as $variable) {
        if (!is_array($variable)) {
            fail('PHP_SDK_GENERATOR_INVALID_OPERATION');
        }
        $name = identifier($variable['name'] ?? null);
        if (isset($seen[strtolower($name)])) {
            fail('PHP_SDK_GENERATOR_SYMBOL_COLLISION');
        }
        $seen[strtolower($name)] = true;
    }
}

function identifier(mixed $value): string
{
    if (!is_string($value) || preg_match('/^[A-Za-z_][A-Za-z0-9_]*$/', $value) !== 1) {
        fail('PHP_SDK_GENERATOR_INVALID_IDENTIFIER');
    }
    $reserved = ['class', 'enum', 'function', 'interface', 'match', 'namespace', 'readonly', 'trait'];
    if (in_array(strtolower($value), $reserved, true)) {
        fail('PHP_SDK_GENERATOR_INVALID_IDENTIFIER');
    }
    return $value;
}

function fail(string $code): never
{
    fwrite(STDERR, $code . "\n");
    exit(1);
}

function template(): string
{
    return <<<'PHP'
<?php

declare(strict_types=1);

// Code generated by naatre.generator.php-sdk-1. DO NOT EDIT.

namespace Naatre\Sdk\Generated;

use Naatre\Sdk\Attribute\WireField;
use Naatre\Sdk\Attribute\WireScalar;
use Naatre\Sdk\Attribute\WireVariant;
use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Protocol\Operation;
use Naatre\Sdk\Scalar\Decimal;
use Naatre\Sdk\Value\ListValue;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\OpenEnumValue;
use Naatre\Sdk\Value\Presence;
use Naatre\Sdk\Value\Selected;

final class GenerationMetadata
{
    public const MODEL_VERSION = 'naatre.generator-model-1';
    public const GENERATOR_VERSION = 'naatre.generator.php-sdk-1';
    public const PROTOCOL_VERSION = '1';
    public const CANONICAL_VERSION = 'c14n-1';
}

#[WireVariant('Status', open: true)]
final readonly class Status
{
    private const KNOWN = ['ACTIVE', 'PENDING'];

    private function __construct(public OpenEnumValue $value)
    {
    }

    public static function fromWire(string $raw): self
    {
        if ($raw === '') {
            throw new ClientException('CLIENT_RESULT_INVALID');
        }
        return new self(new OpenEnumValue($raw, in_array($raw, self::KNOWN, true)));
    }
}

#[WireScalar('Money', 'lossless-decimal-string')]
final readonly class Money
{
    public Decimal $value;

    public function __construct(string $wire)
    {
        $this->value = new Decimal($wire);
    }
}

final readonly class Account
{
    public function __construct(
        #[WireField('status')] public Status $status,
        #[WireField('balance')] public Money $balance,
        #[WireField('displayName')] public string $displayName,
    ) {
    }
}

final readonly class {{OPERATION}}Variables
{
    /**
     * @param Selected|null $nickname
     * @param Selected|null $tags
     * @param Selected|null $filter
     */
    public function __construct(
        #[WireField('id')] public string $id,
        #[WireField('nickname', required: false, nullable: true)] public ?Selected $nickname = null,
        #[WireField('tags', required: false)] public ?Selected $tags = null,
        #[WireField('filter', required: false)] public ?Selected $filter = null,
    ) {
    }

    public function toWire(): ObjectValue
    {
        $entries = [new MapEntry('id', $this->id)];
        self::append($entries, 'nickname', $this->nickname);
        self::append($entries, 'tags', $this->tags);
        self::append($entries, 'filter', $this->filter);
        return new ObjectValue($entries);
    }

    /**
     * @param list<MapEntry> $entries
     * @param Selected|null $selected
     */
    private static function append(array &$entries, string $name, ?Selected $selected): void
    {
        if ($selected === null || $selected->presence === Presence::Missing) {
            return;
        }
        if ($selected->presence === Presence::Null) {
            $entries[] = new MapEntry($name, null);
            return;
        }
        if ($selected->presence !== Presence::Present) {
            throw new ClientException('CLIENT_VARIABLE_INVALID');
        }
        $entries[] = new MapEntry($name, $selected->value);
    }
}

final readonly class {{OPERATION}}ProfileResult
{
    /** @param Selected $nickname */
    public function __construct(
        #[WireField('display')] public string $display,
        #[WireField('nickname', required: false, nullable: true)] public Selected $nickname,
    ) {
    }
}

final readonly class {{OPERATION}}Result
{
    /**
     * @param Selected $profile
     * @param Selected $later
     */
    public function __construct(
        #[WireField('profile')] public Selected $profile,
        #[WireField('later', required: false, pending: true)] public Selected $later,
    ) {
    }
}

final class Operations
{
    private function __construct()
    {
    }

    /** @return Operation */
    public static function {{OPERATION_LOWER}}({{OPERATION}}Variables $variables): Operation
    {
        return new Operation(
            '{{OPERATION}}',
            'query',
            '{{DIGEST}}',
            $variables->toWire(),
            self::decode{{OPERATION}}(...),
        );
    }

    private static function decode{{OPERATION}}(ObjectValue $data): {{OPERATION}}Result
    {
        $profile = $data->selected('profile');
        if ($profile->presence === Presence::Present) {
            if (!$profile->value instanceof ObjectValue) {
                throw new ClientException('CLIENT_RESULT_INVALID');
            }
            $display = $profile->value->selected('display');
            $nickname = $profile->value->selected('nickname');
            if ($display->presence !== Presence::Present || !is_string($display->value)) {
                throw new ClientException('CLIENT_RESULT_INVALID');
            }
            if ($nickname->presence === Presence::Present && !is_string($nickname->value)) {
                throw new ClientException('CLIENT_RESULT_INVALID');
            }
            $profile = Selected::present(new {{OPERATION}}ProfileResult($display->value, $nickname));
        }
        $later = $data->selected('later');
        if ($later->presence === Presence::Missing) {
            $later = Selected::pending();
        } elseif ($later->presence === Presence::Present && !is_string($later->value)) {
            throw new ClientException('CLIENT_RESULT_INVALID');
        }
        return new {{OPERATION}}Result($profile, $later);
    }
}

PHP;
}

function handlerTemplate(): string
{
    return <<<'PHP'
<?php

declare(strict_types=1);

// Code generated by naatre.generator.php-sdk-1. DO NOT EDIT.

namespace Naatre\Sdk\Generated;

use Naatre\Sdk\Exception\ClientException;
use Naatre\Sdk\Server\Effect;
use Naatre\Sdk\Server\RegisteredHandler;
use Naatre\Sdk\Server\RequestContext;
use Naatre\Sdk\Value\MapEntry;
use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Value\Presence;
use Naatre\Sdk\Value\Selected;

final readonly class {{OPERATION}}HandlerInput
{
    public function __construct(public string $id, public Selected $nickname, public Selected $tags, public Selected $filter)
    {
    }
}

final readonly class {{OPERATION}}HandlerProfileOutput
{
    public function __construct(public string $display, public Selected $nickname)
    {
    }
}

final readonly class {{OPERATION}}HandlerOutput
{
    public function __construct(public {{OPERATION}}HandlerProfileOutput $profile, public Selected $later)
    {
    }
}

interface {{OPERATION}}Handler
{
    public function handle({{OPERATION}}HandlerInput $input, RequestContext $context): {{OPERATION}}HandlerOutput;
}

final class {{OPERATION}}HandlerBinding
{
    private function __construct()
    {
    }

    public static function register({{OPERATION}}Handler $handler): RegisteredHandler
    {
        return new RegisteredHandler(
            '{{OPERATION}}', '{{OPERATION}}Input', '{{OPERATION}}Output', Effect::Query, [],
            self::decodeInput(...),
            static function (object $input, RequestContext $context) use ($handler): object {
                if (!$input instanceof {{OPERATION}}HandlerInput) {
                    throw new ClientException('CLIENT_VARIABLE_INVALID');
                }
                return $handler->handle($input, $context);
            },
            static function (object $output): ObjectValue {
                if (!$output instanceof {{OPERATION}}HandlerOutput) {
                    throw new ClientException('CLIENT_RESULT_INVALID');
                }
                return self::encodeOutput($output);
            },
            self::validateOutput(...),
        );
    }

    private static function decodeInput(ObjectValue $input): object
    {
        $id = $input->selected('id');
        if ($id->presence !== Presence::Present || !is_string($id->value)) {
            throw new ClientException('CLIENT_VARIABLE_INVALID');
        }
        return new {{OPERATION}}HandlerInput($id->value, $input->selected('nickname'), $input->selected('tags'), $input->selected('filter'));
    }

    private static function encodeOutput({{OPERATION}}HandlerOutput $output): ObjectValue
    {
        $profileEntries = [new MapEntry('display', $output->profile->display)];
        self::appendSelected($profileEntries, 'nickname', $output->profile->nickname);
        $entries = [new MapEntry('profile', new ObjectValue($profileEntries))];
        self::appendSelected($entries, 'later', $output->later);
        return new ObjectValue($entries);
    }

    /** @param list<MapEntry> $entries */
    private static function appendSelected(array &$entries, string $name, Selected $selected): void
    {
        if ($selected->presence === Presence::Missing || $selected->presence === Presence::Pending) {
            return;
        }
        if ($selected->presence === Presence::Null) {
            $entries[] = new MapEntry($name, null);
            return;
        }
        if ($selected->presence !== Presence::Present) {
            throw new ClientException('CLIENT_RESULT_INVALID');
        }
        $entries[] = new MapEntry($name, $selected->value);
    }

    private static function validateOutput(ObjectValue $output): void
    {
        $profile = $output->selected('profile');
        if ($profile->presence !== Presence::Present || !$profile->value instanceof ObjectValue) {
            throw new ClientException('CLIENT_RESULT_INVALID');
        }
        $display = $profile->value->selected('display');
        $nickname = $profile->value->selected('nickname');
        $later = $output->selected('later');
        if ($display->presence !== Presence::Present || !is_string($display->value)
            || ($nickname->presence === Presence::Present && !is_string($nickname->value))
            || ($later->presence === Presence::Present && !is_string($later->value))) {
            throw new ClientException('CLIENT_RESULT_INVALID');
        }
    }
}

PHP;
}

$arguments = $_SERVER['argv'] ?? null;
if (!is_array($arguments) || (count($arguments) !== 5 && count($arguments) !== 6)) {
    fail('PHP_SDK_GENERATOR_USAGE');
}
foreach ($arguments as $argument) {
    if (!is_string($argument)) {
        fail('PHP_SDK_GENERATOR_USAGE');
    }
}
[$source, $manifest, $handlers] = generate(document($arguments[1]), document($arguments[2]));
if (file_put_contents($arguments[3], $source) === false || file_put_contents($arguments[4], $manifest) === false) {
    fail('PHP_SDK_GENERATOR_WRITE_FAILED');
}
if (isset($arguments[5]) && file_put_contents($arguments[5], $handlers) === false) {
    fail('PHP_SDK_GENERATOR_WRITE_FAILED');
}
