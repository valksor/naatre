# frozen_string_literal: true

require "base64"
require "bigdecimal"
require "json"
require "time"

module Naatre
  MAX_RESPONSE_BYTES = 8 * 1024 * 1024
  MAX_DECOMPRESSED_BYTES = 16 * 1024 * 1024
  MAX_FRAME_BYTES = 1024 * 1024
  MAX_REDIRECTS = 5

  class ClientError < StandardError
    attr_reader :code

    def initialize(code)
      @code = String(code).freeze
      super("Naatre client failed: #{@code}")
    end
  end

  class StrictHash < Hash
    def []=(key, value)
      raise ClientError, "CLIENT_PROTOCOL_INVALID" if key?(key)

      super
    end
  end

  module Wire
    module_function

    SAFE_INTEGER = 9_007_199_254_740_991

    def parse_json(input, maximum_bytes = MAX_DECOMPRESSED_BYTES)
      text = String(input)
      fail_with("CLIENT_RESPONSE_LIMIT") if text.bytesize > maximum_bytes
      fail_with("CLIENT_PROTOCOL_INVALID") unless text.encoding == Encoding::UTF_8 || text.dup.force_encoding(Encoding::UTF_8).valid_encoding?

      JSON.parse(text, :object_class => StrictHash, :create_additions => false)
    rescue JSON::ParserError, EncodingError
      fail_with("CLIENT_PROTOCOL_INVALID")
    end

    def canonical_json(value)
      encode(normalize_keys(value))
    end

    def normalize_keys(value)
      case value
      when Hash
        result = {}
        value.each do |key, entry|
          fail_with("CLIENT_VALUE_INVALID") unless key.is_a?(String) || key.is_a?(Symbol)
          normalized = key.to_s
          fail_with("CLIENT_KEY_COLLISION") if result.key?(normalized)
          result[normalized] = normalize_keys(entry)
        end
        result
      when Array
        value.map { |entry| normalize_keys(entry) }
      else
        value
      end
    end

    def deep_freeze(value)
      case value
      when Hash
        value.each { |key, entry| key.freeze; deep_freeze(entry) }
      when Array
        value.each { |entry| deep_freeze(entry) }
      end
      value.freeze
    end

    def encode(value)
      case value
      when Hash
        members = value.keys.sort_by { |key| key.encode(Encoding::UTF_16BE).bytes }
        "{" + members.map { |key| "#{JSON.generate(key)}:#{encode(value.fetch(key))}" }.join(",") + "}"
      when Array
        "[" + value.map { |entry| encode(entry) }.join(",") + "]"
      when String
        fail_with("CLIENT_VALUE_INVALID") unless value.encoding == Encoding::UTF_8 && value.valid_encoding?
        JSON.generate(value)
      when Integer
        fail_with("CLIENT_VALUE_PRECISION") if value.abs > SAFE_INTEGER
        value.to_s
      when Float
        fail_with("CLIENT_VALUE_INVALID") unless value.finite?
        value.zero? ? "0" : JSON.generate(value)
      when TrueClass
        "true"
      when FalseClass
        "false"
      when NilClass
        "null"
      else
        fail_with("CLIENT_VALUE_PRECISION")
      end
    end

    def fail_with(code)
      raise ClientError, code
    end
  end

  class Timestamp
    attr_reader :wire

    PATTERN = /\A(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})\z/.freeze

    def initialize(value)
      @wire = self.class.canonical(value).freeze
      freeze
    end

    def self.canonical(value)
      return from_time(value) if value.is_a?(Time)
      match = PATTERN.match(value) if value.is_a?(String)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless match

      parsed = Time.iso8601(value)
      fraction = (match[7] || "").sub(/0+\z/, "")
      base = parsed.getutc.strftime("%Y-%m-%dT%H:%M:%S")
      "#{base}#{fraction.empty? ? "" : ".#{fraction}"}Z"
    rescue ArgumentError
      raise ClientError, "CLIENT_SCALAR_INVALID"
    end

    def self.from_time(value)
      utc = value.getutc
      fraction = format("%09d", utc.nsec).sub(/0+\z/, "")
      "#{utc.strftime("%Y-%m-%dT%H:%M:%S")}#{fraction.empty? ? "" : ".#{fraction}"}Z"
    end

    def to_s
      wire
    end
  end

  class Duration
    attr_reader :nanoseconds

    def initialize(value)
      @nanoseconds = Scalar.integer(value, nil, nil)
      freeze
    end

    def to_s
      nanoseconds.to_s
    end
  end

  class UUID
    attr_reader :wire

    PATTERN = /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/.freeze

    def initialize(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value.is_a?(String) && PATTERN.match?(value)
      @wire = value.freeze
      freeze
    end

    def to_s
      wire
    end
  end

  class Bytes
    attr_reader :value

    def initialize(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value.is_a?(String)
      @value = value.b.dup.freeze
      freeze
    end

    def self.decode(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value.is_a?(String) && /\A[A-Za-z0-9_-]*\z/.match?(value) && value.length % 4 != 1
      new(Base64.urlsafe_decode64(value + "=" * ((4 - value.length % 4) % 4)))
    rescue ArgumentError
      raise ClientError, "CLIENT_SCALAR_INVALID"
    end

    def wire
      Base64.urlsafe_encode64(value, :padding => false)
    end
  end

  class UnknownVariant
    attr_reader :raw, :value

    def initialize(raw, value = nil)
      raise ClientError, "CLIENT_RESULT_INVALID" unless raw.is_a?(String)
      @raw = raw.freeze
      @value = value
      freeze
    end
  end

  class OpenEnum
    attr_reader :name, :members

    def initialize(name, members)
      @name = String(name).freeze
      @members = members.map { |member| String(member).freeze }.sort.freeze
      freeze
    end

    def decode(value)
      raise ClientError, "CLIENT_RESULT_INVALID" unless value.is_a?(String)
      members.include?(value) ? value.freeze : UnknownVariant.new(value)
    end
  end

  class OpenUnion
    attr_reader :name, :members

    def initialize(name, members)
      @name = String(name).freeze
      @members = members.map { |member| String(member).freeze }.sort.freeze
      freeze
    end

    def decode(discriminator, value)
      raise ClientError, "CLIENT_RESULT_INVALID" unless discriminator.is_a?(String)
      members.include?(discriminator) ? Selected.present(value) : UnknownVariant.new(discriminator, value)
    end
  end

  class SchemaMetadata
    attr_reader :name, :kind, :descriptor

    def initialize(name, kind, descriptor)
      @name = String(name).freeze
      @kind = String(kind).freeze
      @descriptor = Wire.deep_freeze(Wire.normalize_keys(descriptor))
      freeze
    end
  end

  module Scalar
    module_function

    SIGNED_MIN = -(1 << 63)
    SIGNED_MAX = (1 << 63) - 1
    UNSIGNED_MAX = (1 << 64) - 1
    DECIMAL = /\A-?[0-9]+(?:\.[0-9]+)?\z/.freeze

    def encode(type, value, custom = {})
      custom = Wire.normalize_keys(custom)
      codec = custom[type]
      return codec.call(value) if codec.respond_to?(:call)

      case type
      when "String", "ID" then string(value)
      when "Boolean" then boolean(value)
      when "Int32" then integer(value, -(1 << 31), (1 << 31) - 1)
      when "Float64" then float(value)
      when "Int64" then integer(value, SIGNED_MIN, SIGNED_MAX).to_s
      when "UInt64" then integer(value, 0, UNSIGNED_MAX).to_s
      when "BigInt" then integer(value, nil, nil).to_s
      when "Decimal" then decimal(value)
      when "Timestamp" then Timestamp.new(value).wire
      when "Duration" then Duration.new(value).to_s
      when "UUID" then UUID.new(value).wire
      when "Bytes" then value.is_a?(Bytes) ? value.wire : Bytes.new(value).wire
      when "StringList" then string_list(value)
      when "StringMap" then string_map(value)
      else raise ClientError, "CLIENT_SCALAR_CODEC_MISSING"
      end
    end

    def decode(type, value, custom = {})
      custom = Wire.normalize_keys(custom)
      codec = custom[type]
      return codec.respond_to?(:decode) ? codec.decode(value) : codec.call(value) if codec

      case type
      when "Int64", "UInt64", "BigInt" then integer(value, type == "UInt64" ? 0 : (type == "Int64" ? SIGNED_MIN : nil), type == "UInt64" ? UNSIGNED_MAX : (type == "Int64" ? SIGNED_MAX : nil))
      when "Decimal" then BigDecimal(decimal(value))
      when "Timestamp" then Timestamp.new(value)
      when "Duration" then Duration.new(value)
      when "UUID" then UUID.new(value)
      when "Bytes" then Bytes.decode(value)
      else encode(type, value, custom)
      end
    end

    def integer(value, minimum, maximum)
      raise ClientError, "CLIENT_VALUE_PRECISION" unless value.is_a?(Integer) || value.is_a?(String)
      text = value.to_s
      raise ClientError, "CLIENT_SCALAR_INVALID" unless /\A-?[0-9]+\z/.match?(text)
      parsed = Integer(text, 10)
      raise ClientError, "CLIENT_SCALAR_INVALID" if minimum && parsed < minimum
      raise ClientError, "CLIENT_SCALAR_INVALID" if maximum && parsed > maximum
      parsed
    end

    def decimal(value)
      text = value.is_a?(BigDecimal) ? value.to_s("F") : value
      raise ClientError, "CLIENT_VALUE_PRECISION" if value.is_a?(Float)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless text.is_a?(String) && DECIMAL.match?(text)
      negative = text.start_with?("-")
      integer_part, fraction = (negative ? text[1..-1] : text).split(".", 2)
      integer_part = integer_part.sub(/\A0+(?=[0-9])/, "")
      fraction = (fraction || "").sub(/0+\z/, "")
      normalized = fraction.empty? ? integer_part : "#{integer_part}.#{fraction}"
      normalized = "0" if /\A0(?:\.0*)?\z/.match?(normalized)
      negative && normalized != "0" ? "-#{normalized}" : normalized
    end

    def string(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value.is_a?(String) && value.encoding == Encoding::UTF_8 && value.valid_encoding?
      value
    end

    def boolean(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value == true || value == false
      value
    end

    def float(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value.is_a?(Float) && value.finite?
      value
    end

    def string_list(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless value.is_a?(Array)
      value.map { |entry| string(entry) }
    end

    def string_map(value)
      normalized = Wire.normalize_keys(value)
      raise ClientError, "CLIENT_SCALAR_INVALID" unless normalized.is_a?(Hash)
      normalized.each_value { |entry| string(entry) }
      normalized
    rescue ClientError => error
      raise error if error.code == "CLIENT_KEY_COLLISION"
      raise ClientError, "CLIENT_SCALAR_INVALID"
    end
  end

  class Selected
    attr_reader :state, :value, :errors, :reason

    def initialize(state, value = nil, errors = nil, reason = nil)
      @state = state.freeze
      @value = value
      @errors = errors && errors.freeze
      @reason = reason && String(reason).freeze
      freeze
    end

    MISSING = new("missing")
    NULL = new("null")
    PENDING = new("pending")

    def self.present(value)
      new("present", value)
    end

    def self.failed(errors)
      new("failed", nil, Array(errors).dup)
    end

    def self.skipped(reason)
      new("skipped", nil, nil, reason)
    end
  end

  class ErrorDetail
    attr_reader :code, :message, :path, :retryable, :raw

    def initialize(value)
      normalized = Wire.normalize_keys(value)
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless normalized.is_a?(Hash)
      @raw = Wire.deep_freeze(normalized)
      @code = raw["code"]
      @message = raw["message"]
      @path = raw["path"]
      @retryable = raw["retryable"]
      freeze
    end
  end

  class OperationResult
    attr_reader :data, :errors, :complete

    def initialize(data, errors, complete)
      @data = data
      @errors = errors.freeze
      @complete = complete
      freeze
    end
  end

  class GeneratedValue
    class << self
      def fields
        @fields ||= {}
      end

      def field(name, options)
        fields[name] = options.freeze
        define_method(name) { @values.fetch(name) }
      end

      def decode(value, codecs = {})
        input = Wire.normalize_keys(value)
        raise ClientError, "CLIENT_RESULT_INVALID" unless input.is_a?(Hash)
        unknown = input.keys - fields.keys
        raise ClientError, "CLIENT_RESULT_INVALID" unless unknown.empty?
        values = {}
        fields.each do |name, options|
          selected = if !input.key?(name)
                       options.fetch(:presence) == "pending" ? Selected::PENDING : Selected::MISSING
                     elsif input[name].nil?
                       Selected::NULL
                     else
                       Selected.present(decode_value(options.fetch(:type), input[name], codecs))
                     end
          values[name] = selected
        end
        new(values)
      end

      def decode_value(type, value, codecs)
        return type.decode(value, codecs) if type.respond_to?(:decode) && type.is_a?(Class)
        return type.decode(value) if type.is_a?(OpenEnum)
        Scalar.decode(type, value, codecs)
      end
    end

    def initialize(values)
      @values = Wire.deep_freeze(values)
      freeze
    end

    def to_h
      @values
    end
  end

  class Variables
    class << self
      attr_reader :definitions

      def define(definitions)
        @definitions = Wire.deep_freeze(definitions)
      end
    end

    def initialize(values)
      @values = Operation.normalize_variable_values(self.class.definitions, values)
      Wire.deep_freeze(@values)
      freeze
    end

    def to_h
      @values
    end
  end

  class Operation
    attr_reader :kind, :request

    def initialize(definition, variables, decoder, custom_codecs = {})
      normalized = Wire.normalize_keys(definition)
      validate_definition(normalized)
      @kind = normalized.fetch("kind").freeze
      @decoder = decoder
      @request = {
        "version" => "1",
        "operation" => normalized.fetch("name"),
        "persisted" => normalized.fetch("persisted"),
        "variables" => self.class.encode_variables(normalized.fetch("variables"), variables, custom_codecs)
      }
      Wire.deep_freeze(@request)
      freeze
    end

    def canonical_request
      Wire.canonical_json(request)
    end

    def decode_result(input, maximum_bytes = MAX_DECOMPRESSED_BYTES)
      envelope = input.is_a?(String) ? Wire.parse_json(input, maximum_bytes) : Wire.normalize_keys(input)
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless envelope.is_a?(Hash) && (envelope.keys - ["data", "errors", "complete"]).empty?
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless envelope.key?("complete") && [true, false].include?(envelope["complete"])
      errors = envelope.fetch("errors", [])
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless errors.is_a?(Array)
      data = envelope.key?("data") && !envelope["data"].nil? ? @decoder.call(envelope["data"]) : nil
      OperationResult.new(data, errors.map { |entry| ErrorDetail.new(entry) }, envelope["complete"])
    end

    def replayable?
      kind == "query"
    end

    def self.encode_variables(definitions, variables, custom_codecs)
      input = normalize_variable_values(definitions, variables)
      custom_codecs = Wire.normalize_keys(custom_codecs)
      definitions_by_name = definitions.each_with_object({}) { |entry, result| result[entry.fetch("name")] = entry }
      input.each_with_object({}) do |(name, value), result|
        definition = definitions_by_name.fetch(name)
        result[name] = value.nil? ? nil : Scalar.encode(definition.fetch("type"), value, custom_codecs)
      end
    rescue KeyError
      raise ClientError, "CLIENT_OPERATION_INVALID"
    end

    def self.normalize_variable_values(definitions, variables)
      input = variables.is_a?(Variables) ? variables.to_h : Wire.normalize_keys(variables)
      raise ClientError, "CLIENT_VARIABLES_INVALID" unless input.is_a?(Hash)
      allowed = definitions.each_with_object({}) { |entry, result| result[entry.fetch("name")] = entry }
      raise ClientError, "CLIENT_VARIABLES_INVALID" unless (input.keys - allowed.keys).empty?
      result = {}
      allowed.each do |name, definition|
        if !input.key?(name)
          raise ClientError, "CLIENT_VARIABLES_INVALID" if definition.fetch("required")
          next
        end
        value = input[name]
        if value.nil?
          raise ClientError, "CLIENT_VARIABLES_INVALID" unless definition.fetch("nullable")
          result[name] = nil
        else
          result[name] = value
        end
      end
      result
    rescue KeyError
      raise ClientError, "CLIENT_OPERATION_INVALID"
    end

    private

    def validate_definition(value)
      expected = ["kind", "name", "persisted", "variables"]
      raise ClientError, "CLIENT_OPERATION_INVALID" unless value.is_a?(Hash) && value.keys.sort == expected.sort
      persisted = value["persisted"]
      valid_digest = persisted.is_a?(Hash) && persisted["algorithm"] == "sha-256" && persisted["canonicalVersion"] == "c14n-1" && /\A[0-9a-f]{64}\z/.match?(persisted["digest"].to_s)
      valid_name = /\A[A-Za-z_][A-Za-z0-9_]{0,127}\z/.match?(value["name"].to_s)
      raise ClientError, "CLIENT_OPERATION_INVALID" unless valid_digest && valid_name && ["query", "mutation", "subscription"].include?(value["kind"]) && value["variables"].is_a?(Array)
    end
  end

  class StreamState
    attr_reader :terminal, :position

    def initialize(maximum_frame_bytes = MAX_FRAME_BYTES)
      @maximum_frame_bytes = maximum_frame_bytes
      @terminal = false
      @position = -1
    end

    def accept(input)
      raise ClientError, "CLIENT_STREAM_TERMINAL" if terminal
      frame = input.is_a?(String) ? Wire.parse_json(input, @maximum_frame_bytes) : Wire.normalize_keys(input)
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless frame.is_a?(Hash) && frame["type"].is_a?(String)
      if frame.key?("position")
        raise ClientError, "CLIENT_PROTOCOL_INVALID" unless frame["position"].is_a?(Integer) && frame["position"] == position + 1
        @position = frame["position"]
      end
      @terminal = true if ["complete", "failed", "history-unavailable"].include?(frame["type"])
      Wire.deep_freeze(frame)
    end

    def finish
      raise ClientError, "CLIENT_STREAM_TRUNCATED" unless terminal
      true
    end
  end

  module Pagination
    module_function

    def collect(maximum_pages, initial_cursor = nil)
      raise ClientError, "CLIENT_PAGINATION_INVALID" unless maximum_pages.is_a?(Integer) && maximum_pages.positive?
      cursor = initial_cursor
      seen = {}
      items = []
      maximum_pages.times do
        page = Wire.normalize_keys(yield(cursor))
        valid = page.is_a?(Hash) && page["items"].is_a?(Array) && [true, false].include?(page["hasMore"])
        raise ClientError, "CLIENT_PAGINATION_INVALID" unless valid
        items.concat(page["items"])
        return Wire.deep_freeze(items) unless page["hasMore"]
        cursor = page["nextCursor"]
        raise ClientError, "CLIENT_PAGINATION_INVALID" unless cursor.is_a?(String) && !cursor.empty? && !seen.key?(cursor)
        seen[cursor] = true
      end
      raise ClientError, "CLIENT_PAGINATION_LIMIT"
    end
  end

  def self.unsupported!(capability)
    raise ClientError, "CLIENT_CAPABILITY_UNSUPPORTED:#{capability}"
  end
end
