package runtime_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
)

func TestStreamSourceSessionClosesOnTerminalTruncationAndAbandonment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		frames []protocol.StreamFrame
		want   error
	}{
		{
			name: "terminal",
			frames: []protocol.StreamFrame{
				{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
				{Type: protocol.StreamComplete, Stream: "s", Sequence: 2},
			},
		},
		{
			name: "final-frame-lost",
			frames: []protocol.StreamFrame{
				{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
				{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: []byte(`true`)},
			},
			want: protocol.ErrStreamTruncated,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			source := &sliceStreamSource{frames: test.frames}
			session, err := runtime.NewStreamSourceSession(streamSessionConfig(source))
			if err != nil {
				t.Fatal(err)
			}
			var got error
			for got == nil {
				_, got = session.Next(context.Background())
			}
			if test.want == nil {
				if !errors.Is(got, io.EOF) {
					t.Fatalf("terminal error = %v", got)
				}
			} else if !errors.Is(got, test.want) {
				t.Fatalf("session error = %v, want %v", got, test.want)
			}
			if source.closed.Load() != 1 {
				t.Fatalf("source close count = %d", source.closed.Load())
			}
		})
	}

	source := &sliceStreamSource{}
	session, err := runtime.NewStreamSourceSession(streamSessionConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if source.closed.Load() != 1 {
		t.Fatalf("abandoned source close count = %d", source.closed.Load())
	}
}

func TestStreamSourceSessionClosesOnCancellation(t *testing.T) {
	t.Parallel()
	source := &blockingStreamSource{}
	session, err := runtime.NewStreamSourceSession(streamSessionConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if source.closed.Load() != 1 {
		t.Fatalf("cancel close count = %d", source.closed.Load())
	}
}

func TestStreamSourceSessionReauthorizesAndPinsSchema(t *testing.T) {
	t.Parallel()
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: []byte(`true`)},
	}
	for _, test := range []struct {
		name  string
		check runtime.StreamDeliveryCheck
		want  error
	}{
		{
			name: "authentication-expired",
			check: func(context.Context, protocol.StreamFrame) error {
				return runtime.ErrStreamAuthenticationExpired
			},
			want: runtime.ErrStreamAuthenticationExpired,
		},
		{
			name: "authorization-revoked",
			check: func(context.Context, protocol.StreamFrame) error {
				return runtime.ErrStreamAuthorizationRevoked
			},
			want: runtime.ErrStreamAuthorizationRevoked,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &sliceStreamSource{frames: frames}
			config := streamSessionConfig(source)
			config.Reauthorize = test.check
			session, err := runtime.NewStreamSourceSession(config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := session.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := session.Next(context.Background()); !errors.Is(err, test.want) {
				t.Fatalf("delivery error = %v", err)
			}
			if source.closed.Load() != 1 {
				t.Fatalf("source close count = %d", source.closed.Load())
			}
		})
	}

	source := &sliceStreamSource{frames: frames}
	checks := 0
	config := streamSessionConfig(source)
	config.ValidateSchema = func(context.Context, string) error {
		checks++
		if checks > 1 {
			return runtime.ErrStreamSchemaRetired
		}
		return nil
	}
	session, err := runtime.NewStreamSourceSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); !errors.Is(err, runtime.ErrStreamSchemaRetired) {
		t.Fatalf("schema retirement error = %v", err)
	}
}

func TestStreamSourceSessionRejectsSchemaMismatch(t *testing.T) {
	t.Parallel()
	source := &sliceStreamSource{frames: []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r2"},
	}}
	session, err := runtime.NewStreamSourceSession(streamSessionConfig(source))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); !errors.Is(err, runtime.ErrStreamSchemaMismatch) {
		t.Fatalf("schema mismatch error = %v", err)
	}
	if source.closed.Load() != 1 {
		t.Fatalf("source close count = %d", source.closed.Load())
	}
}

func TestStreamSourceSessionReauthorizesBeforeResumeCursor(t *testing.T) {
	t.Parallel()
	source := &sliceStreamSource{frames: []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamResume, Stream: "s", Sequence: 2, Cursor: "opaque-cursor"},
	}}
	config := streamSessionConfig(source)
	config.Reauthorize = func(context.Context, protocol.StreamFrame) error { return runtime.ErrStreamAuthorizationRevoked }
	session, err := runtime.NewStreamSourceSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); !errors.Is(err, runtime.ErrStreamAuthorizationRevoked) {
		t.Fatalf("resume authorization error = %v", err)
	}
}

func TestStreamSourceSessionRequiresVersionAndDeliveryGuards(t *testing.T) {
	t.Parallel()
	source := &sliceStreamSource{}
	config := streamSessionConfig(source)
	config.ProfileVersion = "2"
	if _, err := runtime.NewStreamSourceSession(config); !errors.Is(err, protocol.ErrUnsupportedStreamVersion) {
		t.Fatalf("unsupported profile error = %v", err)
	}
	config = streamSessionConfig(source)
	config.Reauthorize = nil
	if _, err := runtime.NewStreamSourceSession(config); !errors.Is(err, runtime.ErrInvalidStreamSource) {
		t.Fatalf("missing authorization guard error = %v", err)
	}
	config = streamSessionConfig(source)
	config.ValidateSchema = nil
	if _, err := runtime.NewStreamSourceSession(config); !errors.Is(err, runtime.ErrInvalidStreamSource) {
		t.Fatalf("missing schema guard error = %v", err)
	}
}

func TestStreamSourceSessionFailsClosedOnGuardPanicCancellationAndTimeout(t *testing.T) {
	frames := []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: []byte(`true`)},
	}
	t.Run("panic", func(t *testing.T) {
		source := &sliceStreamSource{frames: frames}
		config := streamSessionConfig(source)
		config.Reauthorize = func(context.Context, protocol.StreamFrame) error { panic("boom") }
		session, err := runtime.NewStreamSourceSession(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Next(context.Background()); !errors.Is(err, runtime.ErrStreamAuthorizationRevoked) {
			t.Fatalf("panic guard error = %v", err)
		}
	})
	t.Run("cancel-before-delivery", func(t *testing.T) {
		source := &sliceStreamSource{frames: frames}
		ctx, cancel := context.WithCancel(context.Background())
		config := streamSessionConfig(source)
		config.Reauthorize = func(context.Context, protocol.StreamFrame) error { cancel(); return nil }
		session, err := runtime.NewStreamSourceSession(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Next(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Next(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled delivery error = %v", err)
		}
	})
	t.Run("bounded-expired-check", func(t *testing.T) {
		source := &sliceStreamSource{frames: frames[:1]}
		config := streamSessionConfig(source)
		config.Session.CheckTimeout = 20 * time.Millisecond
		config.ValidateSchema = func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return nil
		}
		session, err := runtime.NewStreamSourceSession(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Next(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded check error = %v", err)
		}
	})
	t.Run("session-duration", func(t *testing.T) {
		source := &blockingStreamSource{}
		config := streamSessionConfig(source)
		config.Session.MaxDuration = 10 * time.Millisecond
		session, err := runtime.NewStreamSourceSession(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.Next(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("duration error = %v", err)
		}
		if source.closed.Load() != 1 {
			t.Fatalf("duration close count = %d", source.closed.Load())
		}
	})
}

func TestStreamSourceSessionAuthenticationDeadlineBoundsKeepaliveOnlySource(t *testing.T) {
	past := &sliceStreamSource{}
	pastConfig := streamSessionConfig(past)
	pastConfig.AuthenticationExpiresAt = time.Now().Add(-time.Second)
	if _, err := runtime.NewStreamSourceSession(pastConfig); !errors.Is(err, runtime.ErrStreamAuthenticationExpired) {
		t.Fatalf("past authentication construction = %v", err)
	}
	if past.closed.Load() != 0 {
		t.Fatalf("rejected source was acquired and closed %d times", past.closed.Load())
	}

	source := &keepaliveStreamSource{}
	config := streamSessionConfig(source)
	config.AuthenticationExpiresAt = time.Now().Add(20 * time.Millisecond)
	session, err := runtime.NewStreamSourceSession(config)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = session.Next(context.Background())
		if err != nil {
			break
		}
	}
	if !errors.Is(err, runtime.ErrStreamAuthenticationExpired) {
		t.Fatalf("keepalive authentication expiry = %v", err)
	}
	if source.closed.Load() != 1 {
		t.Fatalf("expired source close count = %d", source.closed.Load())
	}
}

func TestStreamSourceSessionNormalizesGuardErrorsAndEnforcesWorkBudget(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtime.StreamSourceSessionConfig)
		want   error
	}{
		{
			name: "authorization-secret",
			mutate: func(config *runtime.StreamSourceSessionConfig) {
				config.Reauthorize = func(context.Context, protocol.StreamFrame) error { return errors.New("secret credential detail") }
			},
			want: runtime.ErrStreamAuthorizationRevoked,
		},
		{
			name: "authorization-eof",
			mutate: func(config *runtime.StreamSourceSessionConfig) {
				config.Reauthorize = func(context.Context, protocol.StreamFrame) error { return io.EOF }
			},
			want: runtime.ErrStreamAuthorizationRevoked,
		},
		{
			name: "schema-secret",
			mutate: func(config *runtime.StreamSourceSessionConfig) {
				config.ValidateSchema = func(context.Context, string) error { return errors.New("secret schema detail") }
			},
			want: runtime.ErrStreamSchemaRetired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &sliceStreamSource{frames: []protocol.StreamFrame{
				{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
				{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: []byte(`true`)},
			}}
			config := streamSessionConfig(source)
			test.mutate(&config)
			session, err := runtime.NewStreamSourceSession(config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = session.Next(context.Background())
			if err == nil && errors.Is(test.want, runtime.ErrStreamAuthorizationRevoked) {
				_, err = session.Next(context.Background())
			}
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("normalized guard error = %v, want %v", err, test.want)
			}
		})
	}

	source := &sliceStreamSource{frames: []protocol.StreamFrame{
		{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"},
		{Type: protocol.StreamKeepalive, Stream: "s"},
	}}
	config := streamSessionConfig(source)
	config.Session.MaxEvents = 1
	session, err := runtime.NewStreamSourceSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Next(context.Background()); !errors.Is(err, runtime.ErrStreamSessionLimit) {
		t.Fatalf("event budget error = %v", err)
	}
}

func TestStreamSourceSessionCloseCancelsInFlightGuardsWithLifecycleCause(t *testing.T) {
	for _, auth := range []bool{false, true} {
		name := "schema"
		if auth {
			name = "authorization"
		}
		t.Run(name, func(t *testing.T) {
			frames := []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}}
			if auth {
				frames = append(frames, protocol.StreamFrame{Type: protocol.StreamData, Stream: "s", Sequence: 2, Data: []byte(`true`)})
			}
			source := &sliceStreamSource{frames: frames}
			config := streamSessionConfig(source)
			started := make(chan struct{})
			guard := func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return nil
			}
			if auth {
				config.Reauthorize = func(ctx context.Context, _ protocol.StreamFrame) error { return guard(ctx) }
			} else {
				config.ValidateSchema = func(ctx context.Context, _ string) error { return guard(ctx) }
			}
			session, err := runtime.NewStreamSourceSession(config)
			if err != nil {
				t.Fatal(err)
			}
			if auth {
				if _, err := session.Next(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			result := make(chan error, 1)
			go func() {
				_, nextErr := session.Next(context.Background())
				result <- nextErr
			}()
			<-started
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, io.EOF) {
				t.Fatalf("close during guard = %v", err)
			}
		})
	}
}

func TestStreamCheckExecutorBoundsUncooperativeCallbacks(t *testing.T) {
	executor, err := runtime.NewStreamCheckExecutor(1)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	firstSource := &sliceStreamSource{frames: []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}}}
	firstConfig := streamSessionConfig(firstSource)
	firstConfig.CheckExecutor = executor
	firstConfig.Session.CheckTimeout = 20 * time.Millisecond
	firstConfig.ValidateSchema = func(context.Context, string) error {
		close(started)
		<-release
		close(finished)
		return nil
	}
	first, err := runtime.NewStreamSourceSession(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Next(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncooperative callback error = %v", err)
	}
	<-started
	if active := executor.Active(); active != 1 {
		t.Fatalf("active callbacks = %d, want 1", active)
	}

	var secondStarted atomic.Bool
	secondSource := &sliceStreamSource{frames: []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}}}
	secondConfig := streamSessionConfig(secondSource)
	secondConfig.CheckExecutor = executor
	secondConfig.Session.CheckTimeout = 20 * time.Millisecond
	secondConfig.ValidateSchema = func(context.Context, string) error {
		secondStarted.Store(true)
		return nil
	}
	second, err := runtime.NewStreamSourceSession(secondConfig)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if _, err := second.Next(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("saturated executor error = %v", err)
	}
	if secondStarted.Load() {
		close(release)
		t.Fatal("callback started without an executor slot")
	}
	close(release)
	<-finished
	deadline := time.Now().Add(time.Second)
	for executor.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active := executor.Active(); active != 0 {
		t.Fatalf("active callbacks after release = %d", active)
	}

	var cancelledStarted atomic.Bool
	cancelledSource := &sliceStreamSource{frames: []protocol.StreamFrame{{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}}}
	cancelledConfig := streamSessionConfig(cancelledSource)
	cancelledConfig.CheckExecutor = executor
	cancelledConfig.ValidateSchema = func(context.Context, string) error {
		cancelledStarted.Store(true)
		return nil
	}
	cancelled, err := runtime.NewStreamSourceSession(cancelledConfig)
	if err != nil {
		t.Fatal(err)
	}
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cancelled.Next(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled callback admission error = %v", err)
	}
	if cancelledStarted.Load() {
		t.Fatal("callback started after cancellation")
	}
}

type sliceStreamSource struct {
	frames []protocol.StreamFrame
	index  int
	closed atomic.Int32
}

func streamSessionConfig(source runtime.StreamSource) runtime.StreamSourceSessionConfig {
	executor, _ := runtime.NewStreamCheckExecutor(4)
	return runtime.StreamSourceSessionConfig{
		Stream: "s", ProfileVersion: protocol.StreamProfileVersion, SchemaRevision: "schema-r1", Source: source,
		Advertisement: protocol.StreamSourceAdvertisement{
			ProfileVersion: protocol.StreamProfileVersion, Replay: protocol.StreamReplayNone,
			Consistency: protocol.StreamLiveBestEffort, RetentionPolicy: "none", HistoryRecovery: protocol.StreamRecoveryRestart,
		},
		Reauthorize:             func(context.Context, protocol.StreamFrame) error { return nil },
		ValidateSchema:          func(context.Context, string) error { return nil },
		CheckExecutor:           executor,
		AuthenticationExpiresAt: time.Now().Add(time.Hour),
	}
}

func (s *sliceStreamSource) Next(context.Context) (protocol.StreamFrame, error) {
	if s.index == len(s.frames) {
		return protocol.StreamFrame{}, io.EOF
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

func (s *sliceStreamSource) Close() error {
	s.closed.Add(1)
	return nil
}

type blockingStreamSource struct{ closed atomic.Int32 }

type keepaliveStreamSource struct {
	closed atomic.Int32
	opened bool
}

func (s *keepaliveStreamSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	if !s.opened {
		s.opened = true
		return protocol.StreamFrame{Type: protocol.StreamOpen, Stream: "s", Sequence: 1, SchemaRevision: "schema-r1"}, nil
	}
	select {
	case <-ctx.Done():
		return protocol.StreamFrame{}, context.Cause(ctx)
	case <-time.After(time.Millisecond):
		return protocol.StreamFrame{Type: protocol.StreamKeepalive, Stream: "s"}, nil
	}
}

func (s *keepaliveStreamSource) Close() error {
	s.closed.Add(1)
	return nil
}

func (*blockingStreamSource) Next(ctx context.Context) (protocol.StreamFrame, error) {
	<-ctx.Done()
	return protocol.StreamFrame{}, context.Cause(ctx)
}

func (s *blockingStreamSource) Close() error {
	s.closed.Add(1)
	return nil
}
