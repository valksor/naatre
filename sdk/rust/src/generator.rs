use crate::{
    ClientError, Manifest, ManifestOperation, OperationKind, PersistedReference, decode_json,
};
use serde::Deserialize;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;

pub const GENERATOR_VERSION: &str = "naatre.generator.rust-sdk-1";
const MODEL_VERSION: &str = "naatre.generator-model-1";
const REFERENCE_VERSION: &str = "naatre.generator.reference-json-1";
const MAXIMUM_INPUT_BYTES: usize = 16 << 20;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Artifacts {
    pub source: Vec<u8>,
    pub manifest: Vec<u8>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Model {
    version: String,
    protocol_version: String,
    canonical_version: String,
    configuration: Configuration,
    schema: SchemaModel,
    operations: Vec<ModelOperation>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Configuration {
    scalar_mappings: BTreeMap<String, String>,
}

#[derive(Deserialize)]
struct SchemaModel {
    #[serde(default)]
    types: Vec<TypeDescriptor>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct TypeDescriptor {
    id: String,
    name: String,
    kind: String,
    #[serde(default)]
    open: bool,
    #[serde(default)]
    fields: Vec<FieldDescriptor>,
    #[serde(default)]
    enum_members: Vec<NamedMember>,
    #[serde(default)]
    variant_members: Vec<VariantMember>,
    element: Option<String>,
    #[serde(default)]
    element_nullable: bool,
}

#[derive(Deserialize)]
struct NamedMember {
    id: String,
    name: String,
}

#[derive(Deserialize)]
struct VariantMember {
    id: String,
    #[serde(rename = "type")]
    type_id: String,
}

#[derive(Deserialize)]
struct FieldDescriptor {
    id: String,
    name: String,
    #[serde(rename = "type")]
    type_id: String,
    #[serde(default)]
    required: bool,
    #[serde(default)]
    nullable: bool,
}

#[derive(Deserialize)]
struct ModelOperation {
    name: String,
    document: Value,
    #[serde(default)]
    variables: Vec<Variable>,
    result: ResultNode,
}

#[derive(Deserialize)]
struct Variable {
    name: String,
    #[serde(rename = "type")]
    type_id: String,
    #[serde(default)]
    required: bool,
    #[serde(default)]
    nullable: bool,
}

#[derive(Clone, Deserialize)]
struct ResultNode {
    kind: String,
    #[serde(rename = "type")]
    type_id: Option<String>,
    #[serde(default)]
    nullable: bool,
    #[serde(default)]
    fields: Vec<ResultField>,
    element: Option<Box<ResultNode>>,
}

#[derive(Clone, Deserialize)]
struct ResultField {
    name: String,
    presence: String,
    result: ResultNode,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ReferenceOutput {
    model_version: String,
    generator_version: String,
    protocol_version: String,
    canonical_version: String,
    schema: ReferenceDigest,
    operations: Vec<ReferenceOperation>,
}

#[derive(Deserialize)]
struct ReferenceDigest {
    digest: String,
}

#[derive(Deserialize)]
struct ReferenceOperation {
    name: String,
    persisted: ReferenceDigest,
}

/// Generates deterministic Rust bindings and their persisted manifest.
///
/// # Errors
///
/// Returns a stable generator error code when inputs are invalid, drift from
/// the reference, contain unsafe symbols, or use unsupported schema features.
pub fn generate(model_bytes: &[u8], reference_bytes: &[u8]) -> Result<Artifacts, ClientError> {
    if model_bytes.is_empty()
        || reference_bytes.is_empty()
        || model_bytes.len() > MAXIMUM_INPUT_BYTES
        || reference_bytes.len() > MAXIMUM_INPUT_BYTES
    {
        return Err(generation_error("RUST_SDK_GENERATOR_INPUT_LIMIT"));
    }

    let model_value: Value = decode_json(model_bytes)
        .map_err(|_| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
    let model: Model = serde_json::from_value(model_value.clone())
        .map_err(|_| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
    let reference_value: Value = decode_json(reference_bytes)
        .map_err(|_| generation_error("RUST_SDK_GENERATOR_INVALID_REFERENCE"))?;
    let mut canonical_reference = canonical_json(&reference_value).into_bytes();
    canonical_reference.push(b'\n');
    if canonical_reference != reference_bytes {
        return Err(generation_error("RUST_SDK_GENERATOR_REFERENCE_DRIFT"));
    }
    let reference: ReferenceOutput = serde_json::from_value(reference_value)
        .map_err(|_| generation_error("RUST_SDK_GENERATOR_INVALID_REFERENCE"))?;
    validate_versions(&model, &reference)?;
    validate_reference(&model, &model_value, &reference)?;
    validate_symbols(&model)?;
    validate_scalars(&model)?;

    let operations = bind_operations(&model, &reference)?;
    let source = generate_source(&model, &operations)?.into_bytes();
    let manifest = generate_manifest(&operations)?;
    Ok(Artifacts { source, manifest })
}

struct OperationBinding<'a> {
    model: &'a ModelOperation,
    kind: OperationKind,
    digest: &'a str,
}

fn validate_versions(model: &Model, reference: &ReferenceOutput) -> Result<(), ClientError> {
    if model.version != MODEL_VERSION
        || model.protocol_version != "1"
        || model.canonical_version != "c14n-1"
        || reference.model_version != model.version
        || reference.generator_version != REFERENCE_VERSION
        || reference.protocol_version != model.protocol_version
        || reference.canonical_version != model.canonical_version
    {
        return Err(generation_error("RUST_SDK_GENERATOR_VERSION_SKEW"));
    }
    Ok(())
}

fn validate_reference(
    model: &Model,
    model_value: &Value,
    reference: &ReferenceOutput,
) -> Result<(), ClientError> {
    let schema = model_value
        .get("schema")
        .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
    if semantic_digest("schema", &normalize_schema(schema.clone())) != reference.schema.digest {
        return Err(generation_error("RUST_SDK_GENERATOR_REFERENCE_DRIFT"));
    }
    if model.operations.len() != reference.operations.len() {
        return Err(generation_error("RUST_SDK_GENERATOR_REFERENCE_DRIFT"));
    }
    for operation in &model.operations {
        let expected = reference
            .operations
            .iter()
            .find(|candidate| candidate.name == operation.name)
            .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_REFERENCE_DRIFT"))?;
        if semantic_digest("document", &operation.document) != expected.persisted.digest {
            return Err(generation_error("RUST_SDK_GENERATOR_REFERENCE_DRIFT"));
        }
    }
    Ok(())
}

fn validate_symbols(model: &Model) -> Result<(), ClientError> {
    let mut type_symbols = BTreeSet::new();
    let mut function_symbols = BTreeSet::new();
    for descriptor in &model.schema.types {
        if descriptor.id.is_empty()
            || descriptor.name.is_empty()
            || !type_symbols.insert(rust_type_name(&descriptor.name)?)
        {
            return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
        }
        let mut field_symbols = BTreeSet::new();
        for field in &descriptor.fields {
            if field.id.is_empty() || !field_symbols.insert(rust_field_name(&field.name)?) {
                return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
            }
        }
        let mut variant_symbols = BTreeSet::new();
        for member in &descriptor.enum_members {
            if member.id.is_empty() || !variant_symbols.insert(rust_type_name(&member.name)?) {
                return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
            }
        }
        for member in &descriptor.variant_members {
            if member.id.is_empty()
                || member.type_id.is_empty()
                || !variant_symbols.insert(rust_type_name(union_discriminator(
                    &member.type_id,
                    model,
                )?)?)
            {
                return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
            }
        }
        if descriptor.open && !variant_symbols.insert("Unknown".to_owned()) {
            return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
        }
    }
    for operation in &model.operations {
        let operation_name = rust_type_name(&operation.name)?;
        if !type_symbols.insert(format!("{operation_name}Variables"))
            || !type_symbols.insert(format!("{operation_name}Result"))
            || !function_symbols.insert(rust_field_name(&operation.name)?)
        {
            return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
        }
        let mut variable_symbols = BTreeSet::new();
        for variable in &operation.variables {
            if !variable_symbols.insert(rust_field_name(&variable.name)?) {
                return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
            }
        }
        validate_result_symbols(
            &format!("{operation_name}Result"),
            &operation.result,
            &mut type_symbols,
        )?;
    }
    Ok(())
}

fn validate_result_symbols(
    owner: &str,
    node: &ResultNode,
    type_symbols: &mut BTreeSet<String>,
) -> Result<(), ClientError> {
    let mut field_symbols = BTreeSet::new();
    for field in &node.fields {
        if !field_symbols.insert(rust_field_name(&field.name)?) {
            return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
        }
        validate_nested_result_symbols(owner, field, type_symbols)?;
    }
    Ok(())
}

fn validate_nested_result_symbols(
    owner: &str,
    field: &ResultField,
    type_symbols: &mut BTreeSet<String>,
) -> Result<(), ClientError> {
    match field.result.kind.as_str() {
        "object" => {
            let nested = format!("{owner}{}", rust_type_name(&field.name)?);
            if !type_symbols.insert(nested.clone()) {
                return Err(generation_error("RUST_SDK_GENERATOR_SYMBOL_COLLISION"));
            }
            validate_result_symbols(&nested, &field.result, type_symbols)
        }
        "list" => {
            let element = field
                .result
                .element
                .as_deref()
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
            validate_nested_result_symbols(
                owner,
                &ResultField {
                    name: field.name.clone(),
                    presence: field.presence.clone(),
                    result: element.clone(),
                },
                type_symbols,
            )
        }
        _ => Ok(()),
    }
}

fn validate_scalars(model: &Model) -> Result<(), ClientError> {
    for descriptor in &model.schema.types {
        if descriptor.kind != "scalar" {
            continue;
        }
        let mapping = model
            .configuration
            .scalar_mappings
            .get(&descriptor.id)
            .or_else(|| model.configuration.scalar_mappings.get(&descriptor.name))
            .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_UNMAPPED_SCALAR"))?;
        if !matches!(
            mapping.as_str(),
            "string"
                | "lossless-decimal-string"
                | "int64-string"
                | "uint64-string"
                | "bigint-string"
                | "timestamp-string"
                | "duration-string"
                | "uuid-string"
                | "base64url-string"
                | "raw-json"
        ) {
            return Err(generation_error("RUST_SDK_GENERATOR_UNMAPPED_SCALAR"));
        }
    }
    Ok(())
}

fn bind_operations<'a>(
    model: &'a Model,
    reference: &'a ReferenceOutput,
) -> Result<Vec<OperationBinding<'a>>, ClientError> {
    let mut bindings = Vec::with_capacity(model.operations.len());
    for operation in &model.operations {
        let reference = reference
            .operations
            .iter()
            .find(|candidate| candidate.name == operation.name)
            .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_REFERENCE_DRIFT"))?;
        let kind = operation
            .document
            .pointer("/operations/0/kind")
            .and_then(Value::as_str)
            .and_then(|value| match value {
                "query" => Some(OperationKind::Query),
                "mutation" => Some(OperationKind::Mutation),
                "subscription" => Some(OperationKind::Subscription),
                _ => None,
            })
            .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
        bindings.push(OperationBinding {
            model: operation,
            kind,
            digest: &reference.persisted.digest,
        });
    }
    bindings.sort_by(|left, right| left.model.name.cmp(&right.model.name));
    Ok(bindings)
}

fn generate_source(
    model: &Model,
    operations: &[OperationBinding<'_>],
) -> Result<String, ClientError> {
    let mut source = String::from(
        "// Code generated by naatre-rust-sdk-generator; DO NOT EDIT.\n\n\
use crate::{decode_json, ClientError, Manifest, ManifestOperation, Operation, OperationKind, Optional, PersistedReference, Presence, Selected};\n\
use serde::{Deserialize, Deserializer, Serialize, Serializer};\n\
use std::collections::BTreeMap;\n\n",
    );
    writeln!(
        source,
        "pub const GENERATOR_VERSION: &str = {GENERATOR_VERSION:?};\n"
    )
    .map_err(|_| generation_error("RUST_SDK_GENERATOR_OUTPUT_FAILED"))?;

    let mut descriptors: Vec<_> = model.schema.types.iter().collect();
    descriptors.sort_by(|left, right| left.name.cmp(&right.name));
    for descriptor in descriptors {
        write_schema_type(&mut source, descriptor, model)?;
    }
    for binding in operations {
        write_operation(&mut source, binding, model)?;
    }
    write_manifest_function(&mut source, operations)?;
    Ok(source)
}

fn write_schema_type(
    source: &mut String,
    descriptor: &TypeDescriptor,
    model: &Model,
) -> Result<(), ClientError> {
    let name = rust_type_name(&descriptor.name)?;
    match descriptor.kind.as_str() {
        "scalar" => {
            let mapping = model
                .configuration
                .scalar_mappings
                .get(&descriptor.id)
                .or_else(|| model.configuration.scalar_mappings.get(&descriptor.name))
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_UNMAPPED_SCALAR"))?;
            writeln!(source, "pub type {name} = {};\n", mapped_scalar(mapping))
                .map_err(output_error)?;
        }
        "enum" => write_enum(source, &name, descriptor)?,
        "union" => write_union(source, &name, descriptor, model)?,
        "object" | "input-object" => write_object(source, &name, descriptor, model)?,
        "list" => {
            let element = descriptor
                .element
                .as_deref()
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
            let mut element_type = rust_wire_type(element, model)?;
            if descriptor.element_nullable {
                element_type = format!("Option<{element_type}>");
            }
            writeln!(source, "pub type {name} = Vec<{element_type}>;\n").map_err(output_error)?;
        }
        "map" => {
            let element = descriptor
                .element
                .as_deref()
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
            let element_type = rust_wire_type(element, model)?;
            writeln!(
                source,
                "pub type {name} = BTreeMap<String, {element_type}>;\n"
            )
            .map_err(output_error)?;
        }
        "oneof" => write_one_of(source, &name, descriptor, model)?,
        _ => return Err(generation_error("RUST_SDK_GENERATOR_UNSUPPORTED_TYPE")),
    }
    Ok(())
}

fn write_enum(
    source: &mut String,
    name: &str,
    descriptor: &TypeDescriptor,
) -> Result<(), ClientError> {
    writeln!(
        source,
        "#[derive(Clone, Debug, Eq, PartialEq)]\npub enum {name} {{"
    )
    .map_err(output_error)?;
    let mut members: Vec<_> = descriptor.enum_members.iter().collect();
    members.sort_by(|left, right| left.name.cmp(&right.name));
    for member in &members {
        writeln!(source, "    {},", rust_type_name(&member.name)?).map_err(output_error)?;
    }
    if descriptor.open {
        writeln!(source, "    Unknown(String),").map_err(output_error)?;
    }
    writeln!(source, "}}\n").map_err(output_error)?;
    writeln!(source, "impl Serialize for {name} {{").map_err(output_error)?;
    writeln!(source, "    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error> where S: Serializer {{").map_err(output_error)?;
    writeln!(source, "        match self {{").map_err(output_error)?;
    for member in &members {
        let variant = rust_type_name(&member.name)?;
        writeln!(
            source,
            "            Self::{variant} => serializer.serialize_str({:?}),",
            member.name
        )
        .map_err(output_error)?;
    }
    if descriptor.open {
        writeln!(
            source,
            "            Self::Unknown(value) => serializer.serialize_str(value),"
        )
        .map_err(output_error)?;
    }
    writeln!(source, "        }}\n    }}\n}}\n").map_err(output_error)?;
    writeln!(source, "impl<'de> Deserialize<'de> for {name} {{").map_err(output_error)?;
    writeln!(source, "    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error> where D: Deserializer<'de> {{").map_err(output_error)?;
    writeln!(source, "        let value = String::deserialize(deserializer)?;\n        Ok(match value.as_str() {{").map_err(output_error)?;
    for member in &members {
        let variant = rust_type_name(&member.name)?;
        writeln!(source, "            {:?} => Self::{variant},", member.name)
            .map_err(output_error)?;
    }
    if descriptor.open {
        writeln!(source, "            _ => Self::Unknown(value),").map_err(output_error)?;
    } else {
        writeln!(source, "            _ => return Err(serde::de::Error::custom(\"unknown closed enum variant\")),").map_err(output_error)?;
    }
    writeln!(source, "        }})\n    }}\n}}\n").map_err(output_error)?;
    Ok(())
}

fn write_union(
    source: &mut String,
    name: &str,
    descriptor: &TypeDescriptor,
    model: &Model,
) -> Result<(), ClientError> {
    writeln!(
        source,
        "#[derive(Clone, Debug, PartialEq)]\npub enum {name} {{"
    )
    .map_err(output_error)?;
    let mut members: Vec<_> = descriptor.variant_members.iter().collect();
    members.sort_by(|left, right| left.type_id.cmp(&right.type_id));
    for member in &members {
        let wire_type = rust_wire_type(&member.type_id, model)?;
        writeln!(source, "    {wire_type}({wire_type}),").map_err(output_error)?;
    }
    if descriptor.open {
        writeln!(source, "    Unknown(crate::OpenUnion),").map_err(output_error)?;
    }
    writeln!(source, "}}\n").map_err(output_error)?;

    writeln!(source, "impl Serialize for {name} {{").map_err(output_error)?;
    writeln!(source, "    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error> where S: Serializer {{").map_err(output_error)?;
    writeln!(source, "        match self {{").map_err(output_error)?;
    for member in &members {
        let wire_type = rust_wire_type(&member.type_id, model)?;
        writeln!(source, "            Self::{wire_type}(value) => crate::OpenUnion::new({:?}, serde_json::to_value(value).map_err(serde::ser::Error::custom)?, true).serialize(serializer),", union_discriminator(&member.type_id, model)?).map_err(output_error)?;
    }
    if descriptor.open {
        writeln!(
            source,
            "            Self::Unknown(value) => value.serialize(serializer),"
        )
        .map_err(output_error)?;
    }
    writeln!(source, "        }}\n    }}\n}}\n").map_err(output_error)?;

    writeln!(source, "impl<'de> Deserialize<'de> for {name} {{").map_err(output_error)?;
    writeln!(source, "    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error> where D: Deserializer<'de> {{").map_err(output_error)?;
    writeln!(source, "        let value = crate::OpenUnion::deserialize(deserializer)?;\n        match value.discriminator() {{").map_err(output_error)?;
    for member in &members {
        let wire_type = rust_wire_type(&member.type_id, model)?;
        writeln!(source, "            {:?} => serde_json::from_value(value.value().clone()).map(Self::{wire_type}).map_err(serde::de::Error::custom),", union_discriminator(&member.type_id, model)?).map_err(output_error)?;
    }
    if descriptor.open {
        writeln!(source, "            _ => Ok(Self::Unknown(value)),").map_err(output_error)?;
    } else {
        writeln!(
            source,
            "            _ => Err(serde::de::Error::custom(\"unknown closed union variant\")),"
        )
        .map_err(output_error)?;
    }
    writeln!(source, "        }}\n    }}\n}}\n").map_err(output_error)?;
    Ok(())
}

fn union_discriminator<'a>(type_id: &str, model: &'a Model) -> Result<&'a str, ClientError> {
    model
        .schema
        .types
        .iter()
        .find(|descriptor| descriptor.id == type_id || descriptor.name == type_id)
        .map(|descriptor| descriptor.name.as_str())
        .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_UNSUPPORTED_TYPE"))
}

fn write_object(
    source: &mut String,
    name: &str,
    descriptor: &TypeDescriptor,
    model: &Model,
) -> Result<(), ClientError> {
    writeln!(
        source,
        "#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]\npub struct {name} {{"
    )
    .map_err(output_error)?;
    let mut fields: Vec<_> = descriptor.fields.iter().collect();
    fields.sort_by(|left, right| left.name.cmp(&right.name));
    for field in fields {
        let field_name = rust_field_name(&field.name)?;
        let wire_type = rust_wire_type(&field.type_id, model)?;
        if field.name != field_name {
            writeln!(source, "    #[serde(rename = {:?})]", field.name).map_err(output_error)?;
        }
        if field.required {
            let wire_type = if field.nullable {
                format!("Option<{wire_type}>")
            } else {
                wire_type
            };
            writeln!(source, "    pub {field_name}: {wire_type},").map_err(output_error)?;
        } else {
            let (wrapper, predicate) = if field.nullable {
                ("Presence", "Presence::is_missing")
            } else {
                ("Optional", "Optional::is_missing")
            };
            writeln!(source, "    #[serde(default, skip_serializing_if = \"{predicate}\")]\n    pub {field_name}: {wrapper}<{wire_type}>,").map_err(output_error)?;
        }
    }
    writeln!(source, "}}\n").map_err(output_error)?;
    Ok(())
}

fn write_one_of(
    source: &mut String,
    name: &str,
    descriptor: &TypeDescriptor,
    model: &Model,
) -> Result<(), ClientError> {
    writeln!(source, "#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]\n#[serde(untagged)]\npub enum {name} {{").map_err(output_error)?;
    let mut fields: Vec<_> = descriptor.fields.iter().collect();
    fields.sort_by(|left, right| left.name.cmp(&right.name));
    for field in fields {
        let variant = rust_type_name(&field.name)?;
        let field_name = rust_field_name(&field.name)?;
        let mut wire_type = rust_wire_type(&field.type_id, model)?;
        if field.nullable {
            wire_type = format!("Option<{wire_type}>");
        }
        writeln!(source, "    {variant} {{ {field_name}: {wire_type} }},").map_err(output_error)?;
    }
    writeln!(source, "}}\n").map_err(output_error)?;
    Ok(())
}

fn write_operation(
    source: &mut String,
    binding: &OperationBinding<'_>,
    model: &Model,
) -> Result<(), ClientError> {
    let operation = rust_type_name(&binding.model.name)?;
    let variables_name = format!("{operation}Variables");
    let result_name = format!("{operation}Result");
    writeln!(
        source,
        "#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]\npub struct {variables_name} {{"
    )
    .map_err(output_error)?;
    let mut variables: Vec<_> = binding.model.variables.iter().collect();
    variables.sort_by(|left, right| left.name.cmp(&right.name));
    for variable in variables {
        let field_name = rust_field_name(&variable.name)?;
        let mut wire_type = rust_wire_type(&variable.type_id, model)?;
        if variable.required {
            if variable.nullable {
                wire_type = format!("Option<{wire_type}>");
            }
            writeln!(source, "    pub {field_name}: {wire_type},").map_err(output_error)?;
        } else {
            let (wrapper, predicate) = if variable.nullable {
                ("Presence", "Presence::is_missing")
            } else {
                ("Optional", "Optional::is_missing")
            };
            writeln!(source, "    #[serde(default, skip_serializing_if = \"{predicate}\")]\n    pub {field_name}: {wrapper}<{wire_type}>,").map_err(output_error)?;
        }
    }
    writeln!(source, "}}\n").map_err(output_error)?;
    write_result_struct(source, &result_name, &binding.model.result, model)?;
    let function = format!("create_{}", rust_field_name(&binding.model.name)?);
    let decoder = format!("decode_{}_result", rust_field_name(&binding.model.name)?);
    writeln!(source, "fn {decoder}(input: &[u8]) -> Result<{result_name}, ClientError> {{\n    decode_json(input)\n}}\n").map_err(output_error)?;
    writeln!(source, "/// Creates the generated operation and validated persisted reference.\n///\n/// # Errors\n///\n/// Returns an error if the generated persisted reference is invalid.\npub fn {function}() -> Result<Operation<{variables_name}, {result_name}>, ClientError> {{\n    Ok(Operation::new({:?}, OperationKind::{}, PersistedReference::new({:?})?, {decoder}))\n}}\n", binding.model.name, kind_variant(&binding.kind), binding.digest).map_err(output_error)?;
    Ok(())
}

fn write_result_struct(
    source: &mut String,
    name: &str,
    node: &ResultNode,
    model: &Model,
) -> Result<(), ClientError> {
    if node.kind != "object" {
        return Err(generation_error("RUST_SDK_GENERATOR_UNSUPPORTED_RESULT"));
    }
    for field in &node.fields {
        write_nested_result_struct(source, name, field, model)?;
    }
    writeln!(
        source,
        "#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]\npub struct {name} {{"
    )
    .map_err(output_error)?;
    for field in &node.fields {
        let field_name = rust_field_name(&field.name)?;
        let field_type = result_type(name, field, model)?;
        if field.name != field_name {
            writeln!(source, "    #[serde(rename = {:?})]", field.name).map_err(output_error)?;
        }
        if field.presence == "pending" {
            writeln!(source, "    #[serde(default = \"Selected::pending\")]\n    pub {field_name}: Selected<{field_type}>,").map_err(output_error)?;
        } else {
            writeln!(
                source,
                "    #[serde(default)]\n    pub {field_name}: Selected<{field_type}>,"
            )
            .map_err(output_error)?;
        }
    }
    writeln!(source, "}}\n").map_err(output_error)?;
    Ok(())
}

fn write_nested_result_struct(
    source: &mut String,
    owner: &str,
    field: &ResultField,
    model: &Model,
) -> Result<(), ClientError> {
    match field.result.kind.as_str() {
        "object" => {
            let nested = format!("{owner}{}", rust_type_name(&field.name)?);
            write_result_struct(source, &nested, &field.result, model)
        }
        "list" => {
            let element = field
                .result
                .element
                .as_deref()
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
            write_nested_result_struct(
                source,
                owner,
                &ResultField {
                    name: field.name.clone(),
                    presence: field.presence.clone(),
                    result: element.clone(),
                },
                model,
            )
        }
        _ => Ok(()),
    }
}

fn result_type(owner: &str, field: &ResultField, model: &Model) -> Result<String, ClientError> {
    match field.result.kind.as_str() {
        "object" => Ok(format!("{owner}{}", rust_type_name(&field.name)?)),
        "scalar" => rust_wire_type(
            field
                .result
                .type_id
                .as_deref()
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?,
            model,
        ),
        "list" => {
            let element = field
                .result
                .element
                .as_deref()
                .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_INVALID_MODEL"))?;
            let nested = ResultField {
                name: field.name.clone(),
                presence: field.presence.clone(),
                result: element.clone(),
            };
            let mut element_type = result_type(owner, &nested, model)?;
            if element.nullable {
                element_type = format!("Option<{element_type}>");
            }
            Ok(format!("Vec<{element_type}>"))
        }
        _ => Err(generation_error("RUST_SDK_GENERATOR_UNSUPPORTED_RESULT")),
    }
}

fn write_manifest_function(
    source: &mut String,
    operations: &[OperationBinding<'_>],
) -> Result<(), ClientError> {
    writeln!(source, "/// Returns the generated operation manifest.\n///\n/// # Errors\n///\n/// Returns an error if a generated persisted reference is invalid.\npub fn manifest() -> Result<Manifest, ClientError> {{\n    Ok(Manifest {{\n        profile: \"sdk.rust.core-1\".to_owned(),\n        version: \"1\".to_owned(),\n        protocol_version: \"1\".to_owned(),\n        canonical_version: \"c14n-1\".to_owned(),\n        operations: vec![").map_err(output_error)?;
    for binding in operations {
        writeln!(source, "            ManifestOperation {{ name: {:?}.to_owned(), kind: OperationKind::{}, persisted: PersistedReference::new({:?})? }},", binding.model.name, kind_variant(&binding.kind), binding.digest).map_err(output_error)?;
    }
    writeln!(source, "        ],\n    }})\n}}").map_err(output_error)?;
    Ok(())
}

fn generate_manifest(operations: &[OperationBinding<'_>]) -> Result<Vec<u8>, ClientError> {
    let manifest = Manifest {
        profile: "sdk.rust.core-1".to_owned(),
        version: "1".to_owned(),
        protocol_version: "1".to_owned(),
        canonical_version: "c14n-1".to_owned(),
        operations: operations
            .iter()
            .map(|binding| {
                Ok(ManifestOperation {
                    name: binding.model.name.clone(),
                    kind: binding.kind.clone(),
                    persisted: PersistedReference::new(binding.digest)?,
                })
            })
            .collect::<Result<Vec<_>, ClientError>>()?,
    };
    let mut bytes = manifest.canonical_json()?;
    bytes.push(b'\n');
    Ok(bytes)
}

fn rust_wire_type(type_id: &str, model: &Model) -> Result<String, ClientError> {
    let builtin = match type_id {
        "Boolean" => Some("bool"),
        "String" | "ID" => Some("String"),
        "Int32" => Some("i32"),
        "Float64" => Some("f64"),
        "Int64" => Some("crate::Int64"),
        "UInt64" => Some("crate::UInt64"),
        "BigInt" => Some("crate::BigInt"),
        "Decimal" => Some("crate::Decimal"),
        "Timestamp" => Some("crate::Timestamp"),
        "Duration" => Some("crate::Duration"),
        "UUID" => Some("crate::Uuid"),
        "Bytes" => Some("crate::Bytes"),
        "StringList" => Some("Vec<String>"),
        "StringMap" => Some("BTreeMap<String, String>"),
        _ => None,
    };
    if let Some(builtin) = builtin {
        return Ok(builtin.to_owned());
    }
    let descriptor = model
        .schema
        .types
        .iter()
        .find(|descriptor| descriptor.id == type_id || descriptor.name == type_id)
        .ok_or_else(|| generation_error("RUST_SDK_GENERATOR_UNSUPPORTED_TYPE"))?;
    rust_type_name(&descriptor.name)
}

fn mapped_scalar(mapping: &str) -> &'static str {
    match mapping {
        "lossless-decimal-string" => "crate::Decimal",
        "int64-string" => "crate::Int64",
        "uint64-string" => "crate::UInt64",
        "bigint-string" => "crate::BigInt",
        "timestamp-string" => "crate::Timestamp",
        "duration-string" => "crate::Duration",
        "uuid-string" => "crate::Uuid",
        "base64url-string" => "crate::Bytes",
        "raw-json" => "serde_json::Value",
        _ => "String",
    }
}

fn kind_variant(kind: &OperationKind) -> &'static str {
    match kind {
        OperationKind::Query => "Query",
        OperationKind::Mutation => "Mutation",
        OperationKind::Subscription => "Subscription",
    }
}

fn rust_type_name(value: &str) -> Result<String, ClientError> {
    if !portable_identifier(value) {
        return Err(generation_error("RUST_SDK_GENERATOR_INVALID_SYMBOL"));
    }
    let mut output = String::new();
    for component in value.split('_') {
        let mut characters = component.chars();
        if let Some(first) = characters.next() {
            output.extend(first.to_uppercase());
            if component.chars().any(char::is_lowercase) {
                output.extend(characters);
            } else {
                output.extend(characters.flat_map(char::to_lowercase));
            }
        }
    }
    if output.is_empty() {
        return Err(generation_error("RUST_SDK_GENERATOR_INVALID_SYMBOL"));
    }
    if RUST_KEYWORDS.contains(&output.as_str()) {
        return Err(generation_error("RUST_SDK_GENERATOR_INVALID_SYMBOL"));
    }
    Ok(output)
}

fn rust_field_name(value: &str) -> Result<String, ClientError> {
    if !portable_identifier(value) {
        return Err(generation_error("RUST_SDK_GENERATOR_INVALID_SYMBOL"));
    }
    let mut output = String::new();
    for (index, character) in value.chars().enumerate() {
        if character.is_ascii_uppercase() {
            if index > 0 {
                output.push('_');
            }
            output.push(character.to_ascii_lowercase());
        } else {
            output.push(character);
        }
    }
    if RUST_KEYWORDS.contains(&output.as_str()) {
        output.push('_');
    }
    Ok(output)
}

fn portable_identifier(value: &str) -> bool {
    let mut bytes = value.bytes();
    bytes
        .next()
        .is_some_and(|byte| byte.is_ascii_alphabetic() || byte == b'_')
        && bytes.all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
}

const RUST_KEYWORDS: &[&str] = &[
    "as", "async", "await", "break", "const", "continue", "crate", "dyn", "else", "enum", "extern",
    "false", "fn", "for", "gen", "if", "impl", "in", "let", "loop", "match", "mod", "move", "mut",
    "pub", "ref", "return", "self", "Self", "static", "struct", "super", "trait", "true", "type",
    "unsafe", "use", "where", "while",
];

fn normalize_schema(mut schema: Value) -> Value {
    sort_strings(&mut schema, "capabilities");
    sort_by_id(&mut schema, "types");
    if let Some(types) = schema.get_mut("types").and_then(Value::as_array_mut) {
        for descriptor in types {
            for member in ["variants", "enumValues", "capabilities"] {
                sort_strings(descriptor, member);
            }
            for member in [
                "fields",
                "enumMembers",
                "variantMembers",
                "retired",
                "traits",
            ] {
                sort_by_id(descriptor, member);
            }
            if let Some(entity) = descriptor.get_mut("entity") {
                sort_strings(entity, "keys");
            }
            if let Some(scalar) = descriptor.get_mut("scalar") {
                sort_strings(scalar, "acceptedWireShapes");
            }
        }
    }
    schema
}

fn sort_strings(value: &mut Value, member: &str) {
    if let Some(values) = value.get_mut(member).and_then(Value::as_array_mut) {
        values.sort_by(|left, right| left.as_str().cmp(&right.as_str()));
    }
}

fn sort_by_id(value: &mut Value, member: &str) {
    if let Some(values) = value.get_mut(member).and_then(Value::as_array_mut) {
        values.sort_by(|left, right| {
            left.get("id")
                .and_then(Value::as_str)
                .cmp(&right.get("id").and_then(Value::as_str))
        });
    }
}

fn semantic_digest(purpose: &str, value: &Value) -> String {
    let mut digest = Sha256::new();
    digest.update(format!("naatre:{purpose}:c14n-1\n"));
    digest.update(canonical_json(value));
    format!("{:x}", digest.finalize())
}

fn canonical_json(value: &Value) -> String {
    match value {
        Value::Null => "null".to_owned(),
        Value::Bool(value) => value.to_string(),
        Value::Number(value) => value.to_string(),
        Value::String(value) => {
            serde_json::to_string(value).expect("JSON strings always serialize")
        }
        Value::Array(values) => format!(
            "[{}]",
            values
                .iter()
                .map(canonical_json)
                .collect::<Vec<_>>()
                .join(",")
        ),
        Value::Object(values) => {
            let mut entries: Vec<_> = values.iter().collect();
            entries.sort_by(|(left, _), (right, _)| left.encode_utf16().cmp(right.encode_utf16()));
            format!(
                "{{{}}}",
                entries
                    .into_iter()
                    .map(|(key, value)| format!(
                        "{}:{}",
                        serde_json::to_string(key).expect("JSON object keys always serialize"),
                        canonical_json(value)
                    ))
                    .collect::<Vec<_>>()
                    .join(",")
            )
        }
    }
}

fn output_error(_: std::fmt::Error) -> ClientError {
    generation_error("RUST_SDK_GENERATOR_OUTPUT_FAILED")
}

fn generation_error(code: &'static str) -> ClientError {
    ClientError::new(code, "Rust SDK generation failed")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::path::PathBuf;

    #[test]
    fn reproduces_checked_artifacts() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
        let model = fs::read(root.join("conformance/v1/generator-model.json")).unwrap();
        let reference = fs::read(root.join("conformance/v1/generator-output.json")).unwrap();
        let artifacts = generate(&model, &reference).unwrap();
        assert_eq!(
            artifacts.source,
            fs::read(root.join("sdk/rust/generated/operations.rs")).unwrap()
        );
        assert_eq!(
            artifacts.manifest,
            fs::read(root.join("sdk/rust/generated/operations.json")).unwrap()
        );
        assert_eq!(artifacts, generate(&model, &reference).unwrap());
    }

    #[test]
    fn rejects_reference_drift_and_unmapped_scalars() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
        let model = fs::read(root.join("conformance/v1/generator-model.json")).unwrap();
        let reference = fs::read(root.join("conformance/v1/generator-output.json")).unwrap();
        let mut noncanonical_reference = reference.clone();
        noncanonical_reference.push(b'\n');
        assert_eq!(
            generate(&model, &noncanonical_reference)
                .unwrap_err()
                .code(),
            "RUST_SDK_GENERATOR_REFERENCE_DRIFT"
        );
        let mut drift: Value = serde_json::from_slice(&reference).unwrap();
        drift["operations"][0]["persisted"]["digest"] = Value::String("0".repeat(64));
        assert_eq!(
            generate(&model, &serde_json::to_vec(&drift).unwrap())
                .unwrap_err()
                .code(),
            "RUST_SDK_GENERATOR_REFERENCE_DRIFT"
        );

        let mut unmapped: Value = serde_json::from_slice(&model).unwrap();
        unmapped["configuration"]["scalarMappings"] = serde_json::json!({});
        assert_eq!(
            generate(&serde_json::to_vec(&unmapped).unwrap(), &reference)
                .unwrap_err()
                .code(),
            "RUST_SDK_GENERATOR_UNMAPPED_SCALAR"
        );

        let mut collision: Value = serde_json::from_slice(&model).unwrap();
        collision["schema"]["types"][2]["fields"]
            .as_array_mut()
            .unwrap()
            .push(serde_json::json!({
                "id": "Account.display_name",
                "name": "display_name",
                "type": "String"
            }));
        let mut collision_reference: Value = serde_json::from_slice(&reference).unwrap();
        collision_reference["schema"]["digest"] = Value::String(semantic_digest(
            "schema",
            &normalize_schema(collision["schema"].clone()),
        ));
        assert_eq!(
            generate(
                &serde_json::to_vec(&collision).unwrap(),
                &canonical_reference_bytes(&collision_reference)
            )
            .unwrap_err()
            .code(),
            "RUST_SDK_GENERATOR_SYMBOL_COLLISION"
        );
    }

    #[test]
    fn generates_typed_open_unions_and_preserves_rust_case() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
        let model_bytes = fs::read(root.join("conformance/v1/generator-model.json")).unwrap();
        let reference_bytes = fs::read(root.join("conformance/v1/generator-output.json")).unwrap();
        let mut model: Value = serde_json::from_slice(&model_bytes).unwrap();
        model["schema"]["types"].as_array_mut().unwrap().extend([
            serde_json::json!({
                "id": "Admin",
                "name": "Admin",
                "kind": "object",
                "fields": [{"id": "Admin.id", "name": "id", "type": "ID", "required": true}]
            }),
            serde_json::json!({
                "id": "SearchResult",
                "name": "SearchResult",
                "kind": "union",
                "open": true,
                "variantMembers": [
                    {"id": "SearchResult.Admin", "type": "Admin"},
                    {"id": "SearchResult.Account", "type": "Account"}
                ]
            }),
        ]);
        let mut reference: Value = serde_json::from_slice(&reference_bytes).unwrap();
        reference["schema"]["digest"] = Value::String(semantic_digest(
            "schema",
            &normalize_schema(model["schema"].clone()),
        ));
        let artifacts = generate(
            &serde_json::to_vec(&model).unwrap(),
            &canonical_reference_bytes(&reference),
        )
        .unwrap();
        let source = String::from_utf8(artifacts.source).unwrap();
        assert!(source.contains("pub struct GetAccountVariables"));
        assert!(source.contains("pub enum SearchResult"));
        assert!(source.contains("Account(Account)"));
        assert!(source.contains("Admin(Admin)"));
        assert!(source.contains("Unknown(crate::OpenUnion)"));
    }

    fn canonical_reference_bytes(value: &Value) -> Vec<u8> {
        let mut bytes = canonical_json(value).into_bytes();
        bytes.push(b'\n');
        bytes
    }
}
