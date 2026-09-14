# frozen_string_literal: true

require "digest"
require_relative "../lib/naatre/core"

module Naatre
  module SDKGen
    VERSION = "naatre.generator.ruby-sdk-1"
    MAXIMUM_INPUT_BYTES = 4 * 1024 * 1024
    IDENTIFIER = /\A[A-Za-z_][A-Za-z0-9_]{0,127}\z/.freeze

    Artifacts = Struct.new(:source, :manifest, :rbs)

    module_function

    def generate(model_bytes, reference_bytes)
      reject("RUBY_SDK_GENERATOR_INPUT_LIMIT") if model_bytes.bytesize.zero? || model_bytes.bytesize > MAXIMUM_INPUT_BYTES || reference_bytes.bytesize.zero? || reference_bytes.bytesize > MAXIMUM_INPUT_BYTES
      model = Wire.parse_json(model_bytes, MAXIMUM_INPUT_BYTES)
      reference = Wire.parse_json(reference_bytes, MAXIMUM_INPUT_BYTES)
      validate_versions(model, reference)
      reference_by_name = reference.fetch("operations").each_with_object({}) { |operation, result| result[operation.fetch("name")] = operation }
      reject("RUBY_SDK_GENERATOR_REFERENCE_DRIFT") unless reference_by_name.length == model.fetch("operations").length
      mappings = model.fetch("configuration").fetch("scalarMappings")
      validate_schema(model.fetch("schema"), mappings)
      reference_schema = reference.fetch("schema")
      expected_schema = semantic_digest("schema", normalize_schema(model.fetch("schema")))
      valid_schema = reference_schema["algorithm"] == "sha-256" && reference_schema["canonicalVersion"] == "c14n-1" && reference_schema["digest"] == expected_schema
      reject("RUBY_SDK_GENERATOR_REFERENCE_DRIFT") unless valid_schema && Wire.canonical_json(reference.fetch("scalarMappings")) == Wire.canonical_json(mappings)
      operations = model.fetch("operations").sort_by { |operation| operation.fetch("name") }.map do |operation|
        bind_operation(operation, reference_by_name[operation.fetch("name")])
      end
      Artifacts.new(render_source(model, operations), render_manifest(operations), render_rbs(model, operations)).freeze
    rescue KeyError, TypeError, JSON::ParserError
      reject("RUBY_SDK_GENERATOR_INVALID_INPUT")
    end

    def validate_versions(model, reference)
      valid = model["version"] == "naatre.generator-model-1" && model["protocolVersion"] == "1" && model["canonicalVersion"] == "c14n-1" &&
              reference["modelVersion"] == model["version"] && reference["protocolVersion"] == "1" && reference["canonicalVersion"] == "c14n-1" &&
              reference["generatorVersion"] == "naatre.generator.reference-json-1" && model["operations"].is_a?(Array) && reference["operations"].is_a?(Array)
      reject("RUBY_SDK_GENERATOR_VERSION_SKEW") unless valid
    end

    def validate_schema(schema, mappings)
      reject("RUBY_SDK_GENERATOR_UNMAPPED_SCALAR") unless mappings.is_a?(Hash)
      names = {}
      schema.fetch("types").each do |descriptor|
        name = constant_name(descriptor["name"] || descriptor.fetch("id"))
        reject("RUBY_SDK_GENERATOR_SYMBOL_COLLISION") if names.key?(name)
        names[name] = true
        reject("RUBY_SDK_GENERATOR_UNSUPPORTED_TYPE") unless %w[scalar enum union list map object input input-object oneof].include?(descriptor["kind"])
        if descriptor["kind"] == "scalar" && !builtin_type?(descriptor.fetch("id"))
          reject("RUBY_SDK_GENERATOR_UNMAPPED_SCALAR") unless mappings.key?(descriptor.fetch("id"))
        end
      end
    end

    def bind_operation(operation, reference)
      valid = reference.is_a?(Hash) && reference["name"] == operation["name"] && IDENTIFIER.match?(reference["symbol"].to_s) &&
              reference.dig("persisted", "algorithm") == "sha-256" && reference.dig("persisted", "canonicalVersion") == "c14n-1" &&
              /\A[0-9a-f]{64}\z/.match?(reference.dig("persisted", "digest").to_s)
      reject("RUBY_SDK_GENERATOR_REFERENCE_DRIFT") unless valid
      document_operation = operation.fetch("document").fetch("operations").find { |candidate| candidate["name"] == operation["name"] }
      valid_operation = document_operation && %w[query mutation subscription].include?(document_operation["kind"]) && operation["variables"].is_a?(Array) && operation["result"].is_a?(Hash)
      reject("RUBY_SDK_GENERATOR_INVALID_OPERATION") unless valid_operation
      reject("RUBY_SDK_GENERATOR_REFERENCE_DRIFT") unless semantic_digest("document", normalize_document(operation.fetch("document"))) == reference.fetch("persisted").fetch("digest")
      variables_match = Wire.canonical_json(operation.fetch("variables").sort_by { |entry| entry.fetch("name") }) == Wire.canonical_json(reference.fetch("variables", []).sort_by { |entry| entry.fetch("name") })
      result_matches = Wire.canonical_json(operation.fetch("result")) == Wire.canonical_json(reference.fetch("result").fetch("data"))
      reject("RUBY_SDK_GENERATOR_REFERENCE_DRIFT") unless variables_match && result_matches
      {
        "name" => operation.fetch("name"), "symbol" => reference.fetch("symbol"), "kind" => document_operation.fetch("kind"),
        "persisted" => reference.fetch("persisted"), "variables" => operation.fetch("variables").sort_by { |entry| entry.fetch("name") }, "result" => operation.fetch("result")
      }
    end

    def render_source(model, operations)
      lines = [
        "# frozen_string_literal: true", "", "# Code generated by naatre Ruby SDK generator; DO NOT EDIT.",
        "require_relative \"../core\"", "", "module Naatre", "  module Generated", "    GENERATOR_VERSION = #{VERSION.dump}.freeze", ""
      ]
      model.fetch("schema").fetch("types").sort_by { |entry| entry.fetch("id") }.each do |descriptor|
        constant = constant_name(descriptor.fetch("name"))
        case descriptor.fetch("kind")
        when "enum"
          members = descriptor.fetch("enumMembers", []).map { |member| member.fetch("name") }.sort
          lines << "    #{constant} = Naatre::OpenEnum.new(#{descriptor.fetch("name").dump}, #{ruby_literal(members)}).freeze"
        when "scalar"
          lines << "    #{constant}_SCALAR = #{descriptor.fetch("id").dump}.freeze"
        when "object"
          fields = descriptor.fetch("fields", []).sort_by { |field| field.fetch("name") }.map { |field| [field.fetch("name"), field.fetch("type")] }.to_h
          lines << "    #{constant}_FIELDS = #{ruby_literal(fields)}.freeze"
        when "union"
          members = descriptor.fetch("variantMembers", []).map { |member| member.fetch("type") }.sort
          lines << "    #{constant} = Naatre::OpenUnion.new(#{descriptor.fetch("name").dump}, #{ruby_literal(members)}).freeze"
        else
          lines << "    #{constant}_METADATA = Naatre::SchemaMetadata.new(#{descriptor.fetch("name").dump}, #{descriptor.fetch("kind").dump}, #{ruby_literal(descriptor)}).freeze"
        end
      end
      lines << ""
      operations.each do |operation|
        render_result_classes(lines, operation.fetch("result"), "#{operation.fetch("symbol")}Result")
        variable_class = "#{operation.fetch("symbol")}Variables"
        lines.concat([
          "    class #{variable_class} < Naatre::Variables",
          "      define #{ruby_literal(operation.fetch("variables"))}",
          "    end", ""
        ])
        definition = operation.slice("name", "kind", "persisted", "variables")
        lines << "    #{underscore(operation.fetch("symbol")).upcase}_DEFINITION = #{ruby_literal(definition)}.freeze"
        lines << ""
        lines << "    def self.#{underscore(operation.fetch("symbol"))}(variables, custom_codecs = {})"
        lines << "      values = variables.is_a?(#{variable_class}) ? variables.to_h : variables"
        lines << "      Naatre::Operation.new(#{underscore(operation.fetch("symbol")).upcase}_DEFINITION, values, ->(value) { #{operation.fetch("symbol")}Result.decode(value, custom_codecs) }, custom_codecs)"
        lines << "    end"
        lines << ""
      end
      manifest = manifest_value(operations)
      lines << "    MANIFEST = Naatre::Wire.deep_freeze(#{ruby_literal(manifest)})"
      lines.concat(["  end", "end", ""])
      lines.join("\n")
    end

    def render_result_classes(lines, node, name)
      return unless node.fetch("kind") == "object"
      node.fetch("fields").sort_by { |field| field.fetch("name") }.each do |field|
        nested = field.fetch("result")
        render_result_classes(lines, nested, "#{name}#{constant_name(field.fetch("name"))}") if nested["kind"] == "object"
      end
      lines << "    class #{name} < Naatre::GeneratedValue"
      node.fetch("fields").sort_by { |field| field.fetch("name") }.each do |field|
        nested = field.fetch("result")
        type = nested["kind"] == "object" ? "#{name}#{constant_name(field.fetch("name"))}" : nested.fetch("type")
        rendered_type = nested["kind"] == "object" ? type : type.dump
        lines << "      field #{field.fetch("name").dump}, :presence => #{field.fetch("presence").dump}, :type => #{rendered_type}"
      end
      lines.concat(["    end", ""])
    end

    def render_manifest(operations)
      "#{Wire.canonical_json(manifest_value(operations))}\n"
    end

    def manifest_value(operations)
      {
        "profile" => "sdk.ruby.core-1", "version" => "1", "protocolVersion" => "1", "canonicalVersion" => "c14n-1",
        "operations" => operations.map { |operation| operation.slice("name", "kind", "persisted") }
      }
    end

    def render_rbs(model, operations)
      lines = ["# Code generated by naatre Ruby SDK generator; DO NOT EDIT.", "", "module Naatre", "  module Generated"]
      model.fetch("schema").fetch("types").sort_by { |entry| entry.fetch("id") }.each do |descriptor|
        constant = constant_name(descriptor.fetch("name"))
        case descriptor.fetch("kind")
        when "enum" then lines << "    #{constant}: Naatre::OpenEnum"
        when "scalar" then lines << "    #{constant}_SCALAR: String"
        when "object" then lines << "    #{constant}_FIELDS: Hash[String, String]"
        when "union" then lines << "    #{constant}: Naatre::OpenUnion"
        else lines << "    #{constant}_METADATA: Naatre::SchemaMetadata"
        end
      end
      operations.each do |operation|
        collect_result_rbs(lines, operation.fetch("result"), "#{operation.fetch("symbol")}Result")
        lines << "    class #{operation.fetch("symbol")}Variables < Naatre::Variables"
        lines << "      def initialize: (Hash[(String | Symbol), untyped]) -> void"
        lines << "    end"
        lines << "    def self.#{underscore(operation.fetch("symbol"))}: ((#{operation.fetch("symbol")}Variables | Hash[(String | Symbol), untyped]) variables, ?Hash[(String | Symbol), untyped] custom_codecs) -> Naatre::Operation"
      end
      lines.concat(["  end", "end", ""])
      lines.join("\n")
    end

    def collect_result_rbs(lines, node, name)
      return unless node.fetch("kind") == "object"
      node.fetch("fields").sort_by { |field| field.fetch("name") }.each do |field|
        nested = field.fetch("result")
        collect_result_rbs(lines, nested, "#{name}#{constant_name(field.fetch("name"))}") if nested["kind"] == "object"
      end
      lines << "    class #{name} < Naatre::GeneratedValue"
      node.fetch("fields").sort_by { |field| field.fetch("name") }.each { |field| lines << "      attr_reader #{field.fetch("name")}: Naatre::Selected" }
      lines << "    end"
    end

    def builtin_type?(name)
      %w[Boolean Int32 Float64 Int64 UInt64 BigInt Decimal Timestamp Duration UUID Bytes String ID StringList StringMap].include?(name)
    end

    def normalize_document(document)
      value = Marshal.load(Marshal.dump(document))
      value["requires"].sort! if value["requires"].is_a?(Array)
      value
    end

    def normalize_schema(schema)
      value = Marshal.load(Marshal.dump(schema))
      sort_strings(value, "capabilities")
      sort_by_id(value, "types")
      value.fetch("types", []).each do |descriptor|
        %w[variants enumValues capabilities].each { |member| sort_strings(descriptor, member) }
        %w[fields enumMembers variantMembers retired traits].each { |member| sort_by_id(descriptor, member) }
        sort_strings(descriptor["entity"], "keys") if descriptor["entity"].is_a?(Hash)
        sort_strings(descriptor["scalar"], "acceptedWireShapes") if descriptor["scalar"].is_a?(Hash)
      end
      %w[operations members retired traits directives extensions references].each { |member| sort_by_id(value, member) }
      value
    end

    def sort_strings(value, member)
      value[member].sort! if value.is_a?(Hash) && value[member].is_a?(Array)
    end

    def sort_by_id(value, member)
      value[member].sort_by! { |entry| entry.fetch("id").to_s } if value.is_a?(Hash) && value[member].is_a?(Array)
    end

    def semantic_digest(purpose, value)
      Digest::SHA256.hexdigest("naatre:#{purpose}:c14n-1\n" + Wire.canonical_json(value))
    end

    def constant_name(value)
      words = value.to_s.scan(/[A-Za-z0-9]+/)
      result = words.map { |word| word[0].upcase + word[1..-1].to_s }.join
      reject("RUBY_SDK_GENERATOR_INVALID_SYMBOL") unless IDENTIFIER.match?(result)
      result
    end

    def underscore(value)
      value.gsub(/([a-z0-9])([A-Z])/, "\\1_\\2").downcase
    end

    def ruby_literal(value)
      case value
      when Hash
        "{" + value.keys.sort.map { |key| "#{key.dump} => #{ruby_literal(value.fetch(key))}" }.join(", ") + "}"
      when Array then "[" + value.map { |entry| ruby_literal(entry) }.join(", ") + "]"
      when String then value.dump
      when TrueClass then "true"
      when FalseClass then "false"
      when NilClass then "nil"
      else reject("RUBY_SDK_GENERATOR_INVALID_INPUT")
      end
    end

    def reject(code)
      raise ClientError, code
    end
  end
end
