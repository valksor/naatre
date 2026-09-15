# frozen_string_literal: true

require "minitest/autorun"
require "naatre"

class NaatreRubyProfileTest < Minitest::Test
  PROFILES = ["sdk.ruby.core-1", "sdk.ruby.adapters-1"].freeze

  class ProfileBody
    attr_reader :closed, :cancelled

    def initialize(chunks)
      @chunks = chunks
      @closed = false
      @cancelled = false
    end

    def each
      @chunks.each { |chunk| yield chunk }
    end

    def close
      @closed = true
    end

    def cancel
      @cancelled = true
    end
  end

  class ProfileAdapter
    def initialize(responses)
      @responses = responses
    end

    def call(_request, _cancellation)
      @responses.shift
    end
  end

  def operation(kind, name)
    definition = Naatre::Generated::GET_ACCOUNT_DEFINITION.merge("kind" => kind, "name" => name)
    Naatre::Operation.new(definition, { "id" => "acct-1" }, ->(value) { value })
  end

  def test_value_boundaries_are_executed_under_every_advertised_profile
    PROFILES.each do |profile|
      assert_equal(
        Naatre::Wire.canonical_json("amount" => "1.20"),
        Naatre::Wire.canonical_json(:amount => "1.20"),
        profile
      )
      assert_equal "1.23", Naatre::Scalar.encode("Decimal", BigDecimal("001.2300")), profile
      assert_equal(
        "2026-09-11T20:30:01.123456789Z",
        Naatre::Timestamp.new("2026-09-11T23:30:01.123456789+03:00").wire,
        profile
      )

      body = ProfileBody.new(["data: {\"type\":\"complete\",\"position\":0}\n\n"])
      repeated = 2.times.map do |index|
        ProfileBody.new([%({"complete":true,"data":{"profile":{"display":"#{profile}-#{index}"}}})])
      end
      responses = [
        Naatre::Transport::Response.new(:status => 200, :headers => { "content-type" => "text/event-stream" }, :body => body)
      ] + repeated.map do |entry|
        Naatre::Transport::Response.new(
          :status => 200,
          :headers => { "content-type" => "application/vnd.naatre.response+json;version=1" },
          :body => entry
        )
      end
      client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => ProfileAdapter.new(responses))
      stream = client.stream(operation("subscription", "ProfileWatch"))
      stream.each { |_frame| break }
      assert body.closed, profile
      assert body.cancelled, profile

      displays = 2.times.map do |index|
        client.execute(operation("query", "ProfileQuery")).data.fetch("profile").fetch("display")
      end
      assert_equal ["#{profile}-0", "#{profile}-1"], displays, profile
      assert repeated.all?(&:closed), profile

      threads = 3.times.map do |index|
        Thread.new { Naatre::Wire.canonical_json(:profile => profile, :index => index) }
      end
      assert_equal 3, threads.map(&:value).uniq.length, profile
      fiber = Fiber.new do
        Fiber.yield Naatre::Wire.canonical_json(:profile => profile)
        Naatre::Wire.canonical_json("profile" => profile)
      end
      assert_equal fiber.resume, fiber.resume, profile
    end
  end
end
