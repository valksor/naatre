# frozen_string_literal: true

require_relative "generator"
require "fileutils"

root = File.expand_path("../../..", __dir__)
model = File.binread(File.join(root, "conformance/v1/generator-model.json"))
reference = File.binread(File.join(root, "conformance/v1/generator-output.json"))
artifacts = Naatre::SDKGen.generate(model, reference)
outputs = {
  File.join(root, "sdk/ruby/lib/naatre/generated/operations.rb") => artifacts.source,
  File.join(root, "sdk/ruby/generated/operations.json") => artifacts.manifest,
  File.join(root, "sdk/ruby/sig/generated.rbs") => artifacts.rbs
}
outputs.each do |path, content|
  FileUtils.mkdir_p(File.dirname(path))
  temporary = "#{path}.tmp-#{Process.pid}"
  File.binwrite(temporary, content)
  File.rename(temporary, path)
ensure
  File.unlink(temporary) if temporary && File.exist?(temporary)
end
