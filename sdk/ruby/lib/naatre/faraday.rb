# frozen_string_literal: true

require_relative "../naatre"

module Naatre
  class FaradayAdapter
    def initialize(connection = nil)
      unless connection
        require "faraday"
        connection = ::Faraday.new
      end
      raise ClientError, "CLIENT_CONFIG_INVALID" unless connection.respond_to?(:run_request)

      @connection = connection
      freeze
    rescue LoadError
      raise ClientError, "CLIENT_CAPABILITY_UNSUPPORTED:faraday"
    end

    def call(request, cancellation)
      cancellation.raise_if_cancelled!
      response = @connection.run_request(request.method.downcase.to_sym, request.url, request.body, request.headers) do |faraday_request|
        if request.timeout && faraday_request.respond_to?(:options)
          faraday_request.options.timeout = request.timeout
          faraday_request.options.open_timeout = request.timeout
        end
      end
      if cancellation.cancelled?
        response.body.close if response.respond_to?(:body) && response.body.respond_to?(:close)
        raise ClientError, "CLIENT_CANCELED"
      end
      Transport::Response.new(:status => response.status, :headers => response.headers, :body => response.body)
    rescue ClientError
      raise
    rescue StandardError
      raise ClientError, "CLIENT_TRANSPORT_FAILED"
    end
  end
end
