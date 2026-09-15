# frozen_string_literal: true

require_relative "lib/naatre"

Gem::Specification.new do |spec|
  spec.name = "naatre"
  spec.version = Naatre::VERSION
  spec.summary = "Strict Ruby bindings and transport adapters for Naatre"
  spec.description = "Framework-neutral Naatre values, generated bindings, bounded transport client, and optional integrations."
  spec.authors = ["Valksor"]
  spec.license = "Apache-2.0"
  spec.homepage = "https://github.com/valksor/naatre"
  spec.required_ruby_version = ">= 2.6"
  spec.files = Dir.chdir(__dir__) { Dir["lib/**/*.rb", "sig/**/*.rbs", "README.md", "LICENSE"].sort }
  spec.require_paths = ["lib"]
  spec.metadata = { "source_code_uri" => spec.homepage, "rubygems_mfa_required" => "true" }
end
