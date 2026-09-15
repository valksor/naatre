# frozen_string_literal: true

require "thread"
require "uri"

module Naatre
  module Transport
    REQUEST_MEDIA_TYPE = "application/vnd.naatre.request+json;version=1".freeze
    RESPONSE_MEDIA_TYPE = "application/vnd.naatre.response+json".freeze
    SSE_MEDIA_TYPE = "text/event-stream".freeze
    SENSITIVE_HEADERS = [
      "authorization", "cookie", "last-event-id", "naatre-principal",
      "naatre-tenant", "proxy-authorization", "x-csrf-token"
    ].freeze
    REDIRECT_STATUSES = [307, 308].freeze
    REJECTED_REDIRECT_STATUSES = [301, 302, 303].freeze

    class Cancellation
      def initialize
        @mutex = Mutex.new
        @cancelled = false
        @callbacks = []
      end

      def cancel
        callbacks = @mutex.synchronize do
          return false if @cancelled

          @cancelled = true
          current = @callbacks
          @callbacks = []
          current
        end
        callbacks.each do |callback|
          begin
            callback.call
          rescue StandardError
            # Cancellation remains stable even when a backend cleanup hook fails.
          end
        end
        true
      end

      def cancelled?
        @mutex.synchronize { @cancelled }
      end

      def raise_if_cancelled!
        raise ClientError, "CLIENT_CANCELED" if cancelled?
      end

      def on_cancel(&callback)
        raise ClientError, "CLIENT_CONFIG_INVALID" unless callback

        invoke = @mutex.synchronize do
          if @cancelled
            true
          else
            @callbacks << callback
            false
          end
        end
        callback.call if invoke
        -> { @mutex.synchronize { @callbacks.delete(callback) } }
      end
    end

    class Request
      attr_reader :method, :url, :headers, :body, :timeout

      def initialize(method:, url:, headers:, body:, timeout: nil)
        @method = String(method).upcase.freeze
        @url = String(url).freeze
        @headers = Transport.normalize_headers(headers)
        @body = String(body).dup.freeze
        @timeout = timeout
        freeze
      end

      def redirect(url, headers)
        self.class.new(:method => method, :url => url, :headers => headers, :body => body, :timeout => timeout)
      end
    end

    class Response
      attr_reader :status, :headers, :body

      def initialize(status:, headers:, body:)
        @status = Integer(status)
        @headers = Transport.normalize_headers(headers)
        @body = body
        @mutex = Mutex.new
        @closed = false
      rescue ArgumentError, TypeError
        raise ClientError, "CLIENT_TRANSPORT_FAILED"
      end

      def close
        owned_body = @mutex.synchronize do
          return false if @closed

          @closed = true
          body
        end
        owned_body.close if owned_body.respond_to?(:close)
        true
      rescue StandardError
        false
      end

      def cancel
        body.cancel if body.respond_to?(:cancel)
        close
      rescue StandardError
        close
      end

      def closed?
        @mutex.synchronize { @closed }
      end
    end

    class Client
      attr_reader :endpoint, :adapter, :limits

      def initialize(endpoint:, adapter:, authenticate: nil, redirect_origins: [], limits: {})
        @endpoint = Transport.endpoint(endpoint).freeze
        raise ClientError, "CLIENT_CONFIG_INVALID" unless adapter.respond_to?(:call)
        raise ClientError, "CLIENT_CONFIG_INVALID" unless authenticate.nil? || authenticate.respond_to?(:call)

        @adapter = adapter
        @authenticate = authenticate
        @redirect_origins = Transport.origins(redirect_origins).freeze
        normalized_limits = Wire.normalize_keys(limits)
        raise ClientError, "CLIENT_CONFIG_INVALID" unless normalized_limits.is_a?(Hash)
        raise ClientError, "CLIENT_CONFIG_INVALID" unless (normalized_limits.keys - ["responseBytes", "frameBytes", "redirects"]).empty?
        @limits = Wire.deep_freeze(
          "responseBytes" => integer_limit(normalized_limits["responseBytes"], MAX_RESPONSE_BYTES, 1),
          "frameBytes" => integer_limit(normalized_limits["frameBytes"], MAX_FRAME_BYTES, 1),
          "redirects" => integer_limit(normalized_limits["redirects"], MAX_REDIRECTS, 0)
        )
        freeze
      rescue ClientError
        raise
      rescue StandardError
        raise ClientError, "CLIENT_CONFIG_INVALID"
      end

      def execute(operation, options = {})
        options = normalized_options(options, false)
        cancellation = options.fetch("cancellation") { Cancellation.new }
        response = perform(operation, options, cancellation, RESPONSE_MEDIA_TYPE + ";version=1")
        validate_unary(response)
        payload = Transport.read_body(response.body, limits.fetch("responseBytes"), cancellation)
        raise ClientError, "CLIENT_REMOTE_ERROR" unless response.status >= 200 && response.status < 300

        operation.decode_result(payload)
      rescue ClientError
        raise
      rescue StandardError
        raise ClientError, "CLIENT_TRANSPORT_FAILED"
      ensure
        response.close if response
      end

      def stream(operation, options = {})
        options = normalized_options(options, true)
        raise ClientError, "CLIENT_OPERATION_INVALID" unless operation.respond_to?(:kind) && operation.kind == "subscription"

        cancellation = options.fetch("cancellation") { Cancellation.new }
        response = perform(operation, options, cancellation, SSE_MEDIA_TYPE)
        validate_stream(response)
        Stream.new(response, cancellation, limits)
      rescue ClientError
        response.close if response
        raise
      rescue StandardError
        response.close if response
        raise ClientError, "CLIENT_TRANSPORT_FAILED"
      end

      def paginate(maximum_pages, initial_cursor = nil, options = {})
        raise ClientError, "CLIENT_CONFIG_INVALID" unless block_given?

        Pagination.collect(maximum_pages, initial_cursor) do |cursor|
          result = execute(yield(cursor), options)
          raise ClientError, "CLIENT_PAGINATION_INVALID" unless result.complete && result.errors.empty? && result.data.is_a?(Hash)
          result.data
        end
      end

      private

      def normalized_options(options, streaming)
        normalized = Wire.normalize_keys(options)
        allowed = ["cancellation", "headers", "timeout"]
        allowed << "lastEventId" if streaming
        raise ClientError, "CLIENT_CONFIG_INVALID" unless normalized.is_a?(Hash) && (normalized.keys - allowed).empty?
        cancellation = normalized["cancellation"]
        raise ClientError, "CLIENT_CONFIG_INVALID" unless cancellation.nil? || cancellation.is_a?(Cancellation)
        timeout = normalized["timeout"]
        raise ClientError, "CLIENT_CONFIG_INVALID" unless timeout.nil? || (timeout.is_a?(Numeric) && timeout.positive?)
        last_event_id = normalized["lastEventId"]
        raise ClientError, "CLIENT_CONFIG_INVALID" unless last_event_id.nil? || (last_event_id.is_a?(String) && !last_event_id.empty? && last_event_id.bytesize <= 1024)
        normalized
      end

      def perform(operation, options, cancellation, accept)
        raise ClientError, "CLIENT_OPERATION_INVALID" unless operation.respond_to?(:canonical_request)

        cancellation.raise_if_cancelled!
        headers = Transport.normalize_headers(options.fetch("headers", {})).dup
        headers["accept"] = accept
        headers["content-type"] = REQUEST_MEDIA_TYPE
        headers["last-event-id"] = options["lastEventId"] if accept == SSE_MEDIA_TYPE && options["lastEventId"]
        headers = authenticate(headers)
        request = Request.new(:method => "POST", :url => endpoint, :headers => headers, :body => operation.canonical_request, :timeout => options["timeout"])
        redirects = 0
        loop do
          cancellation.raise_if_cancelled!
          response = invoke_adapter(request, cancellation)
          status = response.status
          return response unless REDIRECT_STATUSES.include?(status) || REJECTED_REDIRECT_STATUSES.include?(status) || (status >= 300 && status < 400)

          response.close
          raise ClientError, "CLIENT_REDIRECT_DENIED" unless REDIRECT_STATUSES.include?(status)
          raise ClientError, "CLIENT_REDIRECT_LIMIT" if redirects >= limits.fetch("redirects")

          target = redirect_target(request.url, response.headers["location"])
          next_headers = request.headers.dup
          unless Transport.same_origin?(request.url, target)
            raise ClientError, "CLIENT_REDIRECT_DENIED" unless @redirect_origins.include?(Transport.origin(target))
            SENSITIVE_HEADERS.each { |name| next_headers.delete(name) }
          end
          redirects += 1
          request = request.redirect(target, next_headers)
        end
      end

      def authenticate(headers)
        return headers unless @authenticate

        result = @authenticate.call(headers.dup)
        Transport.normalize_headers(result)
      rescue StandardError
        raise ClientError, "CLIENT_AUTH_FAILED"
      end

      def invoke_adapter(request, cancellation)
        response = adapter.call(request, cancellation)
        return response if response.is_a?(Response)
        raise ClientError, "CLIENT_TRANSPORT_FAILED" unless response.respond_to?(:status) && response.respond_to?(:headers) && response.respond_to?(:body)

        Response.new(:status => response.status, :headers => response.headers, :body => response.body)
      rescue ClientError => error
        raise error if error.code == "CLIENT_CANCELED" && cancellation.cancelled?
        raise ClientError, "CLIENT_TRANSPORT_FAILED"
      rescue StandardError
        raise ClientError, "CLIENT_TRANSPORT_FAILED"
      end

      def redirect_target(current, location)
        raise ClientError, "CLIENT_REDIRECT_DENIED" unless location.is_a?(String) && !location.empty? && location.bytesize <= 4096

        target = URI.join(current, location)
        Transport.validate_uri(target)
        target.to_s
      rescue ClientError
        raise
      rescue StandardError
        raise ClientError, "CLIENT_REDIRECT_DENIED"
      end

      def validate_unary(response)
        Transport.validate_content_type(response.headers["content-type"], RESPONSE_MEDIA_TYPE, true)
        Transport.validate_identity_encoding(response.headers["content-encoding"])
        Transport.validate_declared_length(response.headers["content-length"], limits.fetch("responseBytes"))
      end

      def validate_stream(response)
        raise ClientError, "CLIENT_REMOTE_ERROR" unless response.status >= 200 && response.status < 300
        Transport.validate_content_type(response.headers["content-type"], SSE_MEDIA_TYPE, false)
        Transport.validate_identity_encoding(response.headers["content-encoding"])
        Transport.validate_declared_length(response.headers["content-length"], limits.fetch("responseBytes"))
      end

      def integer_limit(value, default, minimum)
        value = default if value.nil?
        raise ClientError, "CLIENT_CONFIG_INVALID" unless value.is_a?(Integer) && value >= minimum
        value
      end
    end

    class Stream
      include Enumerable

      def initialize(response, cancellation, limits)
        @response = response
        @cancellation = cancellation
        @limits = limits
        @mutex = Mutex.new
        @closed = false
        @detach_cancellation = cancellation.on_cancel { close }
      end

      def each
        return enum_for(:each) unless block_given?
        @cancellation.raise_if_cancelled!
        raise ClientError, "CLIENT_STREAM_TERMINAL" if closed?

        state = StreamState.new(@limits.fetch("frameBytes"))
        buffer = "".b
        total = 0
        Transport.each_chunk(@response.body, "CLIENT_STREAM_FAILED") do |chunk|
          @cancellation.raise_if_cancelled!
          bytes = String(chunk).b
          total += bytes.bytesize
          raise ClientError, "CLIENT_RESPONSE_LIMIT" if total > @limits.fetch("responseBytes")
          buffer << bytes
          raise ClientError, "CLIENT_FRAME_LIMIT" if buffer.bytesize > @limits.fetch("frameBytes") && !buffer.include?("\n\n") && !buffer.include?("\r\n\r\n")
          while (boundary = Transport.event_boundary(buffer))
            event = buffer.slice!(0, boundary.fetch(:length))
            raise ClientError, "CLIENT_FRAME_LIMIT" if boundary.fetch(:content) > @limits.fetch("frameBytes")
            data = Transport.event_data(event.slice(0, boundary.fetch(:content)))
            next if data.nil?

            yield state.accept(data)
          end
        end
        raise ClientError, "CLIENT_STREAM_TRUNCATED" unless buffer.empty?
        state.finish
        release
        self
      rescue ClientError
        raise
      rescue StandardError
        raise ClientError, "CLIENT_STREAM_FAILED"
      ensure
        close
      end

      def close
        closed = finish_response(true)
        @cancellation.cancel if closed
        closed
      end

      def closed?
        @mutex.synchronize { @closed }
      end

      private

      def release
        finish_response(false)
      end

      def finish_response(cancel)
        response = @mutex.synchronize do
          return false if @closed

          @closed = true
          @response
        end
        detach_cancellation
        cancel ? response.cancel : response.close
        true
      end

      def detach_cancellation
        callback = @detach_cancellation
        @detach_cancellation = nil
        callback.call if callback
      end
    end

    module_function

    def normalize_headers(value)
      normalized = Wire.normalize_keys(value)
      raise ClientError, "CLIENT_CONFIG_INVALID" unless normalized.is_a?(Hash)
      result = {}
      normalized.each do |name, header_value|
        key = name.downcase
        raise ClientError, "CLIENT_CONFIG_INVALID" unless /\A[a-z0-9!#$%&'*+.^_`|~-]+\z/.match?(key)
        raise ClientError, "CLIENT_KEY_COLLISION" if result.key?(key)
        raise ClientError, "CLIENT_CONFIG_INVALID" unless header_value.is_a?(String) && !header_value.include?("\r") && !header_value.include?("\n")
        result[key.freeze] = header_value.dup.freeze
      end
      result.freeze
    rescue ClientError
      raise
    rescue StandardError
      raise ClientError, "CLIENT_CONFIG_INVALID"
    end

    def endpoint(value)
      uri = URI.parse(String(value))
      valid = uri.absolute? && ["http", "https"].include?(uri.scheme) && uri.host && !uri.host.empty?
      raise ClientError, "CLIENT_CONFIG_INVALID" unless valid && uri.userinfo.nil? && uri.fragment.nil? && uri.query.nil?
      uri.to_s
    rescue ClientError
      raise
    rescue StandardError
      raise ClientError, "CLIENT_CONFIG_INVALID"
    end

    def validate_uri(uri)
      valid = uri.absolute? && ["http", "https"].include?(uri.scheme) && uri.host && !uri.host.empty?
      raise ClientError, "CLIENT_REDIRECT_DENIED" unless valid && uri.userinfo.nil? && uri.fragment.nil?
      uri
    end

    def origin(value)
      uri = URI.parse(value)
      port = uri.port == uri.default_port ? "" : ":#{uri.port}"
      "#{uri.scheme}://#{uri.host.downcase}#{port}"
    end

    def origins(values)
      raise ClientError, "CLIENT_CONFIG_INVALID" unless values.is_a?(Array)
      values.map { |value| origin(endpoint(value)) }.uniq.sort
    rescue ClientError
      raise
    rescue StandardError
      raise ClientError, "CLIENT_CONFIG_INVALID"
    end

    def same_origin?(left, right)
      origin(left) == origin(right)
    end

    def read_body(body, maximum_bytes, cancellation)
      result = "".b
      each_chunk(body, "CLIENT_TRANSPORT_FAILED") do |chunk|
        cancellation.raise_if_cancelled!
        raise ClientError, "CLIENT_TRANSPORT_FAILED" unless chunk.is_a?(String)
        result << chunk.b
        raise ClientError, "CLIENT_RESPONSE_LIMIT" if result.bytesize > maximum_bytes
      end
      result.force_encoding(Encoding::UTF_8)
    end

    def each_chunk(body, failure_code)
      if body.is_a?(String)
        yield body
      elsif body.respond_to?(:each)
        enumerator = body.to_enum(:each)
        loop do
          begin
            chunk = enumerator.next
          rescue StopIteration
            break
          rescue StandardError
            raise ClientError, failure_code
          end
          yield chunk
        end
      else
        raise ClientError, failure_code
      end
    end

    def validate_declared_length(value, maximum_bytes)
      return true if value.nil?
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless /\A[0-9]+\z/.match?(value)
      raise ClientError, "CLIENT_RESPONSE_LIMIT" if Integer(value, 10) > maximum_bytes
      true
    end

    def validate_content_type(value, expected, require_version)
      raise ClientError, "CLIENT_UNSUPPORTED_MEDIA_TYPE" unless value.is_a?(String)
      parts = value.downcase.split(";").map(&:strip)
      raise ClientError, "CLIENT_UNSUPPORTED_MEDIA_TYPE" unless parts.shift == expected
      parameters = parts.each_with_object({}) do |part, result|
        match = /\A([a-z0-9_-]+)=(?:"([^"]*)"|([^\s"]+))\z/.match(part)
        raise ClientError, "CLIENT_UNSUPPORTED_MEDIA_TYPE" unless match && !result.key?(match[1])
        result[match[1]] = match[2] || match[3]
      end
      raise ClientError, "CLIENT_UNSUPPORTED_MEDIA_TYPE" if require_version && parameters["version"] != "1"
      raise ClientError, "CLIENT_UNSUPPORTED_MEDIA_TYPE" if !require_version && parameters["charset"] && parameters["charset"] != "utf-8"
      true
    end

    def validate_identity_encoding(value)
      return true if value.nil? || value.empty? || value.downcase == "identity"

      raise ClientError, "CLIENT_UNSUPPORTED_ENCODING"
    end

    def event_boundary(buffer)
      lf = buffer.index("\n\n")
      crlf = buffer.index("\r\n\r\n")
      return nil unless lf || crlf
      if crlf && (!lf || crlf < lf)
        { :content => crlf, :length => crlf + 4 }
      else
        { :content => lf, :length => lf + 2 }
      end
    end

    def event_data(event)
      text = event.dup.force_encoding(Encoding::UTF_8)
      raise ClientError, "CLIENT_PROTOCOL_INVALID" unless text.valid_encoding?
      data = []
      text.split(/\r?\n/).each do |line|
        next if line.empty? || line.start_with?(":")
        field, value = line.split(":", 2)
        value = value ? value.sub(/\A /, "") : ""
        data << value if field == "data"
        raise ClientError, "CLIENT_PROTOCOL_INVALID" unless ["data", "event", "id", "retry"].include?(field)
      end
      data.empty? ? nil : data.join("\n")
    end
  end
end
