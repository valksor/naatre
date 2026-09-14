# frozen_string_literal: true

require "minitest/autorun"
require "naatre"

class NaatreCoreTest < Minitest::Test
  def test_string_and_symbol_keys_have_one_wire_meaning
    assert_equal Naatre::Wire.canonical_json("id" => "acct-1"), Naatre::Wire.canonical_json(:id => "acct-1")
    error = assert_raises(Naatre::ClientError) { Naatre::Wire.canonical_json("id" => 1, :id => 1) }
    assert_equal "CLIENT_KEY_COLLISION", error.code
  end

  def test_lossless_scalar_adapters_and_canonical_time
    assert_equal "9223372036854775807", Naatre::Scalar.encode("Int64", 9_223_372_036_854_775_807)
    assert_equal 18_446_744_073_709_551_615, Naatre::Scalar.decode("UInt64", "18446744073709551615")
    assert_equal "9007199254740993", Naatre::Scalar.encode("BigInt", "09007199254740993")
    assert_equal "1.23", Naatre::Scalar.encode("Decimal", BigDecimal("001.2300"))
    assert_equal BigDecimal("1.23"), Naatre::Scalar.decode("Decimal", "001.2300")
    assert_raises(Naatre::ClientError) { Naatre::Scalar.encode("Decimal", 1.2) }

    original_tz = ENV["TZ"]
    original_locale = ENV["LC_ALL"]
    ENV["TZ"] = "Pacific/Honolulu"
    ENV["LC_ALL"] = "C"
    first = Naatre::Timestamp.new("2026-09-11T23:30:01.123456789+03:00").wire
    ENV["TZ"] = "Europe/Riga"
    ENV["LC_ALL"] = "C.UTF-8"
    second = Naatre::Timestamp.new("2026-09-11T23:30:01.123456789+03:00").wire
    assert_equal "2026-09-11T20:30:01.123456789Z", first
    assert_equal first, second
  ensure
    ENV["TZ"] = original_tz
    ENV["LC_ALL"] = original_locale
  end

  def test_bytes_uuid_duration_and_unknown_variants_are_lossless
    bytes = Naatre::Bytes.new("Hello".b)
    assert_equal "SGVsbG8", bytes.wire
    assert_equal "Hello".b, Naatre::Bytes.decode(bytes.wire).value
    assert_equal "-42", Naatre::Duration.new("-00042").to_s
    assert_equal "550e8400-e29b-41d4-a716-446655440000", Naatre::UUID.new("550e8400-e29b-41d4-a716-446655440000").wire
    unknown = Naatre::Generated::Status.decode("FUTURE")
    assert_instance_of Naatre::UnknownVariant, unknown
    assert_equal "FUTURE", unknown.raw
    union = Naatre::OpenUnion.new("Search", ["Account"])
    assert_equal "Other", union.decode("Other", { "id" => 1 }).raw
  end

  def test_selected_states_partial_errors_and_immutable_shapes
    value = Naatre::Generated::GetAccountResult.decode("profile" => { "display" => "Ada", "nickname" => nil })
    assert_equal "present", value.profile.state
    assert_equal "present", value.profile.value.display.state
    assert_equal "null", value.profile.value.nickname.state
    assert_equal "pending", value.later.state
    assert value.frozen?
    assert value.profile.value.to_h.frozen?

    result = Naatre::Generated.get_account(:id => "acct-1").decode_result('{"complete":false,"data":{"profile":{"display":"Ada"}},"errors":[{"code":"PARTIAL","path":["later"]}]}')
    refute result.complete
    assert_equal "Ada", result.data.profile.value.display.value
    assert_equal "missing", result.data.profile.value.nickname.state
    assert_equal "PARTIAL", result.errors.first.code
  end

  def test_strict_protocol_limits_and_stream_terminal_contract
    operation = Naatre::Generated.get_account("id" => "acct-1")
    assert_raises(Naatre::ClientError) { operation.decode_result('{"complete":true,"complete":false}') }
    assert_raises(Naatre::ClientError) { operation.decode_result("{}", 1) }
    assert operation.replayable?

    stream = Naatre::StreamState.new
    stream.accept("type" => "next", "position" => 0)
    stream.accept("type" => "complete", "position" => 1)
    assert stream.finish
    assert_raises(Naatre::ClientError) { Naatre::StreamState.new.finish }
    assert_raises(Naatre::ClientError) { Naatre::StreamState.new(1).accept('{"type":"complete"}') }
  end

  def test_unsupported_transport_is_typed_and_writes_are_not_replayable
    definition = Naatre::Generated::GET_ACCOUNT_DEFINITION.merge("kind" => "mutation", "name" => "WriteAccount")
    mutation = Naatre::Operation.new(definition, { "id" => "acct-1" }, ->(value) { value })
    refute mutation.replayable?
    error = assert_raises(Naatre::ClientError) { Naatre.unsupported!("http") }
    assert_equal "CLIENT_CAPABILITY_UNSUPPORTED:http", error.code
  end

  def test_typed_variables_defer_custom_scalar_encoding_and_codec_keys_cannot_collide
    variables_class = Class.new(Naatre::Variables)
    variables_class.define([{ "name" => "amount", "type" => "Money", "required" => true, "nullable" => false }])
    variables = variables_class.new(:amount => BigDecimal("01.20"))
    definition = {
      "name" => "Pay", "kind" => "mutation", "persisted" => Naatre::Generated::GET_ACCOUNT_DEFINITION.fetch("persisted"),
      "variables" => variables_class.definitions
    }
    decimal_codec = ->(value) { Naatre::Scalar.decimal(value) }
    operation = Naatre::Operation.new(definition, variables, ->(value) { value }, :Money => decimal_codec)
    assert_equal "1.2", operation.request.fetch("variables").fetch("amount")
    assert_raises(Naatre::ClientError) do
      Naatre::Operation.new(definition, variables, ->(value) { value }, "Money" => decimal_codec, :Money => ->(value) { value })
    end
  end

  def test_pagination_is_bounded_and_rejects_cursor_cycles
    calls = 0
    items = Naatre::Pagination.collect(2) do |cursor|
      calls += 1
      cursor.nil? ? { :items => [1], :hasMore => true, :nextCursor => "next" } : { "items" => [2], "hasMore" => false }
    end
    assert_equal [1, 2], items
    assert items.frozen?
    assert_equal 2, calls

    error = assert_raises(Naatre::ClientError) do
      Naatre::Pagination.collect(3) { { "items" => [], "hasMore" => true, "nextCursor" => "same" } }
    end
    assert_equal "CLIENT_PAGINATION_INVALID", error.code
  end
end
