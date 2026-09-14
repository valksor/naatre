# frozen_string_literal: true

require "digest"
require "json"
require "minitest/autorun"
require "naatre"
require_relative "../sdkgen/generator"

class NaatreGeneratedTest < Minitest::Test
  ROOT = File.expand_path("../../..", __dir__)

  def test_generation_is_byte_identical_and_persisted_hash_matches_shared_vector
    model = File.binread(File.join(ROOT, "conformance/v1/generator-model.json"))
    reference = File.binread(File.join(ROOT, "conformance/v1/generator-output.json"))
    artifacts = Naatre::SDKGen.generate(model, reference)
    assert_equal File.binread(File.join(ROOT, "sdk/ruby/lib/naatre/generated/operations.rb")), artifacts.source
    assert_equal File.binread(File.join(ROOT, "sdk/ruby/generated/operations.json")), artifacts.manifest
    assert_equal File.binread(File.join(ROOT, "sdk/ruby/sig/generated.rbs")), artifacts.rbs

    expected = JSON.parse(reference).fetch("operations").first
    operation = Naatre::Generated.get_account(:id => "acct-1", "nickname" => nil, :tags => [], "filter" => {})
    assert_equal expected.fetch("persisted").fetch("digest"), operation.request.fetch("persisted").fetch("digest")
    assert_equal Naatre::Wire.canonical_json(expected.fetch("request")), operation.canonical_request
  end

  def test_generated_rbs_names_match_runtime_shapes
    rbs = File.read(File.join(ROOT, "sdk/ruby/sig/generated.rbs"))
    assert_includes rbs, "class GetAccountVariables < Naatre::Variables"
    assert_includes rbs, "class GetAccountResult < Naatre::GeneratedValue"
    assert_includes rbs, "attr_reader profile: Naatre::Selected"
    assert_respond_to Naatre::Generated::GetAccountResult.decode("profile" => {}), :profile
    assert_respond_to Naatre::Generated::GetAccountResult.decode("profile" => {}), :later
  end

  def test_generator_rejects_version_skew_and_unmapped_scalars
    model = JSON.parse(File.read(File.join(ROOT, "conformance/v1/generator-model.json")))
    reference = File.binread(File.join(ROOT, "conformance/v1/generator-output.json"))
    model["version"] = "naatre.generator-model-2"
    error = assert_raises(Naatre::ClientError) { Naatre::SDKGen.generate(JSON.generate(model), reference) }
    assert_equal "RUBY_SDK_GENERATOR_VERSION_SKEW", error.code

    model = JSON.parse(File.read(File.join(ROOT, "conformance/v1/generator-model.json")))
    model.fetch("configuration").fetch("scalarMappings").delete("Money")
    error = assert_raises(Naatre::ClientError) { Naatre::SDKGen.generate(JSON.generate(model), reference) }
    assert_equal "RUBY_SDK_GENERATOR_UNMAPPED_SCALAR", error.code

    model = JSON.parse(File.read(File.join(ROOT, "conformance/v1/generator-model.json")))
    model.fetch("schema")["revision"] = "tampered"
    error = assert_raises(Naatre::ClientError) { Naatre::SDKGen.generate(JSON.generate(model), reference) }
    assert_equal "RUBY_SDK_GENERATOR_REFERENCE_DRIFT", error.code
  end
end
