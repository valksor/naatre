# frozen_string_literal: true

require_relative "../naatre"

module Naatre
  module Rails
    CANCELLATION_ENV = "naatre.cancellation".freeze

    class ResponseBody
      def initialize(body, cancellation)
        @body = body
        @cancellation = cancellation
        @mutex = Mutex.new
        @closed = false
      end

      def each(&block)
        @body.each(&block)
      ensure
        close
      end

      def close
        body = @mutex.synchronize do
          return false if @closed

          @closed = true
          @body
        end
        body.close if body.respond_to?(:close)
        @cancellation.cancel
        true
      end
    end

    class Middleware
      def initialize(app)
        @app = app
      end

      def call(environment)
        cancellation = Transport::Cancellation.new
        environment[CANCELLATION_ENV] = cancellation
        status, headers, body = @app.call(environment)
        [status, headers, ResponseBody.new(body, cancellation)]
      rescue Exception # rubocop:disable Lint/RescueException -- request cleanup must cover every Rack unwind
        cancellation.cancel if cancellation
        raise
      end
    end

    def self.install!(application)
      middleware = application.respond_to?(:middleware) ? application.middleware : nil
      raise ClientError, "CLIENT_CONFIG_INVALID" unless middleware && middleware.respond_to?(:use)

      middleware.use(Middleware)
      application
    end

    if defined?(::Rails::Railtie)
      class Railtie < ::Rails::Railtie
        initializer "naatre.request_cancellation" do |application|
          Naatre::Rails.install!(application)
        end
      end
    end
  end
end
