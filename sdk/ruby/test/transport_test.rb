# frozen_string_literal: true

require "minitest/autorun"
require "naatre"
require "naatre/faraday"
require "naatre/rails"

class NaatreTransportTest < Minitest::Test
  class Body
    attr_reader :close_count, :cancel_count

    def initialize(chunks, before_yield = nil)
      @chunks = chunks
      @before_yield = before_yield
      @close_count = 0
      @cancel_count = 0
    end

    def each
      @chunks.each_with_index do |chunk, index|
        @before_yield.call(index) if @before_yield
        yield chunk
      end
    end

    def close
      @close_count += 1
    end

    def cancel
      @cancel_count += 1
    end
  end

  class SequenceAdapter
    attr_reader :requests, :cancellations

    def initialize(*responses, &handler)
      @responses = responses
      @handler = handler
      @requests = []
      @cancellations = []
      @mutex = Mutex.new
    end

    def call(request, cancellation)
      @mutex.synchronize do
        @requests << request
        @cancellations << cancellation
      end
      return @handler.call(request, cancellation) if @handler

      response = @mutex.synchronize { @responses.shift }
      raise response if response.is_a?(Exception)
      response
    end
  end

  FakeFaradayResponse = Struct.new(:status, :headers, :body)

  class FakeFaradayOptions
    attr_accessor :timeout, :open_timeout
  end

  class FakeFaradayRequest
    attr_reader :options

    def initialize
      @options = FakeFaradayOptions.new
    end
  end

  class FakeFaradayConnection
    attr_reader :arguments, :request

    def initialize(response)
      @response = response
    end

    def run_request(*arguments)
      @arguments = arguments
      @request = FakeFaradayRequest.new
      yield @request
      @response
    end
  end

  class RackBody
    attr_reader :closed

    def initialize
      @closed = false
    end

    def each
      yield "ok"
    end

    def close
      @closed = true
    end
  end

  def operation(kind = "query", name = "GetAccount")
    definition = Naatre::Generated::GET_ACCOUNT_DEFINITION.merge("kind" => kind, "name" => name)
    Naatre::Operation.new(definition, { "id" => "acct-1" }, ->(value) { value })
  end

  def response(body, status = 200, headers = {})
    defaults = { "content-type" => "application/vnd.naatre.response+json;version=1" }
    Naatre::Transport::Response.new(:status => status, :headers => defaults.merge(headers), :body => body)
  end

  def success_body(value = "Ada")
    %({"complete":true,"data":{"profile":{"display":"#{value}"}}})
  end

  def test_unary_success_closes_owned_body_and_accepts_symbol_options
    body = Body.new([success_body])
    adapter = SequenceAdapter.new(response(body))
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test/v1", :adapter => adapter)
    result = client.execute(operation, :headers => { :"X-Trace" => "safe" }, :timeout => 2)

    assert_equal "Ada", result.data.fetch("profile").fetch("display")
    assert_equal 1, body.close_count
    assert_equal "safe", adapter.requests.first.headers.fetch("x-trace")
    assert_equal 2, adapter.requests.first.timeout
    assert adapter.requests.first.frozen?
  end

  def test_failures_are_stable_and_drop_private_transport_details
    adapter = SequenceAdapter.new(RuntimeError.new("Authorization: Bearer protected; tenant=secret"))
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)
    error = assert_raises(Naatre::ClientError) { client.execute(operation) }

    assert_equal "CLIENT_TRANSPORT_FAILED", error.code
    assert_equal "Naatre client failed: CLIENT_TRANSPORT_FAILED", error.message
    refute_includes error.message, "protected"
    refute_includes error.message, "tenant"
  end

  def test_private_adapter_and_body_codes_are_sanitized
    adapter = SequenceAdapter.new(Naatre::ClientError.new("PRIVATE_BACKEND_CODE"))
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)
    assert_equal "CLIENT_TRANSPORT_FAILED", assert_raises(Naatre::ClientError) { client.execute(operation) }.code

    unary_body = Body.new(["unused"], ->(_index) { raise Naatre::ClientError, "PRIVATE_BODY_CODE" })
    stream_body = Body.new(["unused"], ->(_index) { raise Naatre::ClientError, "PRIVATE_STREAM_CODE" })
    body_adapter = SequenceAdapter.new(
      response(unary_body),
      response(stream_body, 200, "content-type" => "text/event-stream")
    )
    body_client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => body_adapter)
    assert_equal "CLIENT_TRANSPORT_FAILED", assert_raises(Naatre::ClientError) { body_client.execute(operation) }.code
    assert_equal "CLIENT_STREAM_FAILED", assert_raises(Naatre::ClientError) { body_client.stream(operation("subscription", "PrivateWatch")).to_a }.code
    assert_equal [1, 1], [unary_body, stream_body].map(&:close_count)
  end

  def test_response_limit_media_type_and_remote_errors_close_resources
    bodies = [Body.new(["12345"]), Body.new(["{}"]), Body.new(["{}"]), Body.new([]), Body.new(["{}"])]
    adapter = SequenceAdapter.new(
      response(bodies[0]),
      response(bodies[1], 200, "content-type" => "text/plain"),
      response(bodies[2], 503),
      response(bodies[3], 200, "content-length" => "5"),
      response(bodies[4], 200, "content-type" => 'application/vnd.naatre.response+json;version="1')
    )
    client = Naatre::Transport::Client.new(
      :endpoint => "https://api.example.test", :adapter => adapter, :limits => { :responseBytes => 4 }
    )

    assert_equal "CLIENT_RESPONSE_LIMIT", assert_raises(Naatre::ClientError) { client.execute(operation) }.code
    assert_equal "CLIENT_UNSUPPORTED_MEDIA_TYPE", assert_raises(Naatre::ClientError) { client.execute(operation) }.code
    assert_equal "CLIENT_REMOTE_ERROR", assert_raises(Naatre::ClientError) { client.execute(operation) }.code
    assert_equal "CLIENT_RESPONSE_LIMIT", assert_raises(Naatre::ClientError) { client.execute(operation) }.code
    assert_equal "CLIENT_UNSUPPORTED_MEDIA_TYPE", assert_raises(Naatre::ClientError) { client.execute(operation) }.code
    assert_equal [1, 1, 1, 1, 1], bodies.map(&:close_count)
  end


  def test_invalid_configuration_and_unary_event_cursor_are_rejected
    adapter = SequenceAdapter.new
    endpoint_error = assert_raises(Naatre::ClientError) do
      Naatre::Transport::Client.new(:endpoint => "https://user:secret@api.example.test", :adapter => adapter)
    end
    assert_equal "CLIENT_CONFIG_INVALID", endpoint_error.code
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)
    cursor_error = assert_raises(Naatre::ClientError) do
      client.execute(operation, :lastEventId => "cursor")
    end
    assert_equal "CLIENT_CONFIG_INVALID", cursor_error.code
  end

  def test_redirects_strip_credentials_and_bound_hops
    redirect_body = Body.new([])
    final_body = Body.new([success_body])
    adapter = SequenceAdapter.new(
      response(redirect_body, 307, "location" => "https://regional.example.test/v1"),
      response(final_body)
    )
    client = Naatre::Transport::Client.new(
      :endpoint => "https://api.example.test/v1",
      :adapter => adapter,
      :redirect_origins => ["https://regional.example.test"],
      :authenticate => ->(headers) { headers.merge("Authorization" => "Bearer protected", "Naatre-Tenant" => "private") }
    )
    client.execute(operation, :headers => { "X-Safe" => "retained" })

    second = adapter.requests.fetch(1)
    refute second.headers.key?("authorization")
    refute second.headers.key?("naatre-tenant")
    assert_equal "retained", second.headers.fetch("x-safe")
    assert_equal 1, redirect_body.close_count

    denied = SequenceAdapter.new(response(Body.new([]), 302, "location" => "/other"))
    denied_client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => denied)
    assert_equal "CLIENT_REDIRECT_DENIED", assert_raises(Naatre::ClientError) { denied_client.execute(operation) }.code

    limited = SequenceAdapter.new(response(Body.new([]), 307, "location" => "/again"))
    limited_client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => limited, :limits => { :redirects => 0 })
    assert_equal "CLIENT_REDIRECT_LIMIT", assert_raises(Naatre::ClientError) { limited_client.execute(operation) }.code
  end

  def test_sse_stream_handles_boundaries_terminal_state_and_early_cleanup
    body = Body.new([
      "data: {\"type\":\"next\",\"position\":0}\n\n",
      "data: {\"type\":\"complete\",\"position\":1}\n\n"
    ])
    adapter = SequenceAdapter.new(response(body, 200, "content-type" => "text/event-stream;charset=utf-8"))
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)
    cancellation = Naatre::Transport::Cancellation.new
    stream = client.stream(operation("subscription", "WatchAccount"), :cancellation => cancellation)
    observed = []
    stream.each do |frame|
      observed << frame.fetch("type")
      break
    end

    assert_equal ["next"], observed
    assert stream.closed?
    assert cancellation.cancelled?
    assert_equal 1, body.cancel_count
    assert_equal 1, body.close_count
  end

  def test_completed_sse_closes_without_reporting_cancellation
    body = Body.new(["data: {\"type\":\"complete\",\"position\":0}\n\n"])
    adapter = SequenceAdapter.new(response(body, 200, "content-type" => "text/event-stream"))
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)

    assert_equal ["complete"], client.stream(operation("subscription", "CompleteWatch")).map { |frame| frame.fetch("type") }
    assert_equal 0, body.cancel_count
    assert_equal 1, body.close_count
  end

  def test_sse_cancellation_truncation_and_frame_limit_release_resources
    cancellation = Naatre::Transport::Cancellation.new
    cancel_body = Body.new(
      ["data: {\"type\":\"next\",\"position\":0}\n\n", "ignored"],
      ->(index) { cancellation.cancel if index == 1 }
    )
    truncated_body = Body.new(["data: {\"type\":\"next\",\"position\":0}\n\n"])
    large_body = Body.new(["data: #{"x" * 65}\n\n"])
    adapter = SequenceAdapter.new(
      response(cancel_body, 200, "content-type" => "text/event-stream"),
      response(truncated_body, 200, "content-type" => "text/event-stream"),
      response(large_body, 200, "content-type" => "text/event-stream")
    )
    client = Naatre::Transport::Client.new(
      :endpoint => "https://api.example.test", :adapter => adapter, :limits => { :frameBytes => 64 }
    )

    error = assert_raises(Naatre::ClientError) do
      client.stream(operation("subscription", "CancelWatch"), :cancellation => cancellation).to_a
    end
    assert_equal "CLIENT_CANCELED", error.code
    assert_equal "CLIENT_STREAM_TRUNCATED", assert_raises(Naatre::ClientError) { client.stream(operation("subscription", "TruncatedWatch")).to_a }.code
    assert_equal "CLIENT_FRAME_LIMIT", assert_raises(Naatre::ClientError) { client.stream(operation("subscription", "LargeWatch")).to_a }.code
    assert_equal [1, 1, 1], [cancel_body, truncated_body, large_body].map(&:close_count)
  end

  def test_faraday_adapter_is_optional_and_uses_the_public_request_contract
    body = Body.new([success_body])
    connection = FakeFaradayConnection.new(FakeFaradayResponse.new(200, { "content-type" => "application/vnd.naatre.response+json;version=1" }, body))
    adapter = Naatre::FaradayAdapter.new(connection)
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)
    client.execute(operation, :timeout => 3)

    assert_equal :post, connection.arguments.fetch(0)
    assert_equal "https://api.example.test", connection.arguments.fetch(1)
    assert_equal 3, connection.request.options.timeout
    assert_equal 3, connection.request.options.open_timeout
    assert_equal 1, body.close_count
  end

  def test_rails_middleware_uses_request_owned_cancellation_without_thread_locals
    tokens = []
    rack_body = RackBody.new
    app = lambda do |environment|
      tokens << environment.fetch(Naatre::Rails::CANCELLATION_ENV)
      [200, { "content-type" => "text/plain" }, rack_body]
    end
    middleware = Naatre::Rails::Middleware.new(app)
    status, _headers, body = middleware.call({})
    body.each { |_chunk| }

    assert_equal 200, status
    assert rack_body.closed
    assert tokens.first.cancelled?

    stack = Class.new do
      attr_reader :used
      def use(value)
        @used = value
      end
    end.new
    application = Struct.new(:middleware).new(stack)
    assert_same application, Naatre::Rails.install!(application)
    assert_equal Naatre::Rails::Middleware, stack.used
  end

  def test_thread_fiber_and_persistent_process_state_are_request_isolated
    adapter = SequenceAdapter.new do |request, _cancellation|
      Fiber.yield(request.headers.fetch("x-id")) if request.headers["x-fiber"] == "yes"
      response(Body.new([success_body(request.headers.fetch("x-id"))]))
    end
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)

    values = 8.times.map do |index|
      Thread.new do
        client.execute(operation, :headers => { :"X-Id" => "thread-#{index}" }).data.fetch("profile").fetch("display")
      end
    end.map(&:value)
    assert_equal 8.times.map { |index| "thread-#{index}" }.sort, values.sort

    first = Fiber.new { client.execute(operation, :headers => { "X-Id" => "fiber-a", "X-Fiber" => "yes" }) }
    second = Fiber.new { client.execute(operation, :headers => { "X-Id" => "fiber-b", "X-Fiber" => "yes" }) }
    assert_equal "fiber-a", first.resume
    assert_equal "fiber-b", second.resume
    assert_equal "fiber-a", first.resume.data.fetch("profile").fetch("display")
    assert_equal "fiber-b", second.resume.data.fetch("profile").fetch("display")
    assert_equal adapter.cancellations.length, adapter.cancellations.map(&:object_id).uniq.length

    persistent = SequenceAdapter.new(RuntimeError.new("private"), response(Body.new([success_body("recovered")])))
    persistent_client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => persistent)
    assert_raises(Naatre::ClientError) { persistent_client.execute(operation) }
    assert_equal "recovered", persistent_client.execute(operation).data.fetch("profile").fetch("display")
  end


  def test_transport_pagination_uses_fresh_operations_and_core_bounds
    adapter = SequenceAdapter.new(
      response(Body.new(['{"complete":true,"data":{"items":[1],"hasMore":true,"nextCursor":"next"}}'])),
      response(Body.new(['{"complete":true,"data":{"items":[2],"hasMore":false}}']))
    )
    client = Naatre::Transport::Client.new(:endpoint => "https://api.example.test", :adapter => adapter)
    cursors = []
    items = client.paginate(2) do |cursor|
      cursors << cursor
      operation("query", "PageAccount")
    end

    assert_equal [1, 2], items
    assert_equal [nil, "next"], cursors
    assert_equal 2, adapter.requests.length
  end
end
