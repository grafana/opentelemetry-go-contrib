// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otelgrpc // import "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

// gRPC tracing middleware
// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/trace/semantic_conventions/rpc.md
import (
	"context"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	grpc_codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/internal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

type messageType attribute.KeyValue

// Event adds an event of the messageType to the span associated with the
// passed context with a message id.
func (m messageType) Event(ctx context.Context, id int, _ interface{}) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.AddEvent("message", trace.WithAttributes(
		attribute.KeyValue(m),
		RPCMessageIDKey.Int(id),
	))
}

var (
	messageSent     = messageType(RPCMessageTypeSent)
	messageReceived = messageType(RPCMessageTypeReceived)
)

const (
	// ServerAddressKey is the attribute Key conforming to the "server.address"
	// semantic conventions. It represents the logical server hostname, matches
	// server FQDN if available, and IP or socket address if FQDN is not known.
	//
	// Type: string
	// RequirementLevel: Optional
	// Stability: stable
	// Examples: 'example.com'.
	ServerAddressKey = attribute.Key("server.address")

	// ServerPortKey is the attribute Key conforming to the "server.port"
	// semantic conventions. It represents the logical server port number
	//
	// Type: int
	// RequirementLevel: Optional
	// Stability: stable
	// Examples: 80, 8080, 443.
	ServerPortKey = attribute.Key("server.port")

	// ServerSocketDomainKey is the attribute Key conforming to the
	// "server.socket.domain" semantic conventions. It represents the domain
	// name of an immediate peer.
	//
	// Type: string
	// RequirementLevel: Recommended (If different than `server.address`.)
	// Stability: stable
	// Examples: 'proxy.example.com'
	// Note: Typically observed from the client side, and represents a proxy or
	// other intermediary domain name.
	ServerSocketDomainKey = attribute.Key("server.socket.domain")

	// ServerSocketAddressKey is the attribute Key conforming to the
	// "server.socket.address" semantic conventions. It represents the physical
	// server IP address or Unix socket address. If set from the client, should
	// simply use the socket's peer address, and not attempt to find any actual
	// server IP (i.e., if set from client, this may represent some proxy
	// server instead of the logical server).
	//
	// Type: string
	// RequirementLevel: Recommended (If different than `server.address`.)
	// Stability: stable
	// Examples: '10.5.3.2'.
	ServerSocketAddressKey = attribute.Key("server.socket.address")

	// ServerSocketPortKey is the attribute Key conforming to the
	// "server.socket.port" semantic conventions. It represents the physical
	// server port.
	//
	// Type: int
	// RequirementLevel: Recommended (If different than `server.port`.)
	// Stability: stable
	// Examples: 16456.
	ServerSocketPortKey = attribute.Key("server.socket.port")
)

const (
	// ClientAddressKey is the attribute Key conforming to the "client.address"
	// semantic conventions. It represents the client address - unix domain
	// socket name, IPv4 or IPv6 address.
	//
	// Type: string
	// RequirementLevel: Optional
	// Stability: stable
	// Examples: '/tmp/my.sock', '10.1.2.80'
	// Note: When observed from the server side, and when communicating through
	// an intermediary, `client.address` SHOULD represent client address behind
	// any intermediaries (e.g. proxies) if it's available.
	ClientAddressKey = attribute.Key("client.address")

	// ClientPortKey is the attribute Key conforming to the "client.port"
	// semantic conventions. It represents the client port number
	//
	// Type: int
	// RequirementLevel: Optional
	// Stability: stable
	// Examples: 65123
	// Note: When observed from the server side, and when communicating through
	// an intermediary, `client.port` SHOULD represent client port behind any
	// intermediaries (e.g. proxies) if it's available.
	ClientPortKey = attribute.Key("client.port")

	// ClientSocketAddressKey is the attribute Key conforming to the
	// "client.socket.address" semantic conventions. It represents the
	// immediate client peer address - unix domain socket name, IPv4 or IPv6
	// address.
	//
	// Type: string
	// RequirementLevel: Recommended (If different than `client.address`.)
	// Stability: stable
	// Examples: '/tmp/my.sock', '127.0.0.1'.
	ClientSocketAddressKey = attribute.Key("client.socket.address")

	// ClientSocketPortKey is the attribute Key conforming to the
	// "client.socket.port" semantic conventions. It represents the immediate
	// client peer port number
	//
	// Type: int
	// RequirementLevel: Recommended (If different than `client.port`.)
	// Stability: stable
	// Examples: 35555.
	ClientSocketPortKey = attribute.Key("client.socket.port")
)

// UnaryClientInterceptor returns a grpc.UnaryClientInterceptor suitable
// for use in a grpc.Dial call.
func UnaryClientInterceptor(opts ...Option) grpc.UnaryClientInterceptor {
	cfg := newConfig(opts)
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		trace.WithInstrumentationVersion(Version()),
	)

	return func(
		ctx context.Context,
		method string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		callOpts ...grpc.CallOption,
	) error {
		i := &InterceptorInfo{
			Method: method,
			Type:   UnaryClient,
		}
		if cfg.Filter != nil && !cfg.Filter(i) {
			return invoker(ctx, method, req, reply, cc, callOpts...)
		}

		name, attr := clientSpanInfo(method, cc.Target())

		startOpts := append([]trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attr...)},
			cfg.SpanStartOptions...,
		)

		ctx, span := tracer.Start(
			ctx,
			name,
			startOpts...,
		)
		defer span.End()

		ctx = inject(ctx, cfg.Propagators)

		if cfg.SentEvent {
			messageSent.Event(ctx, 1, req)
		}

		err := invoker(ctx, method, req, reply, cc, callOpts...)

		if cfg.ReceivedEvent {
			messageReceived.Event(ctx, 1, reply)
		}

		if err != nil {
			s, _ := status.FromError(err)
			span.SetStatus(codes.Error, s.Message())
			span.SetAttributes(statusCodeAttr(s.Code()))
		} else {
			span.SetAttributes(statusCodeAttr(grpc_codes.OK))
		}

		return err
	}
}

type streamEventType int

type streamEvent struct {
	Type streamEventType
	Err  error
}

const (
	receiveEndEvent streamEventType = iota
	errorEvent
)

// clientStream  wraps around the embedded grpc.ClientStream, and intercepts the RecvMsg and
// SendMsg method call.
type clientStream struct {
	grpc.ClientStream

	desc       *grpc.StreamDesc
	events     chan streamEvent
	eventsDone chan struct{}
	finished   chan error

	receivedEvent bool
	sentEvent     bool

	receivedMessageID int
	sentMessageID     int
}

var _ = proto.Marshal

func (w *clientStream) RecvMsg(m interface{}) error {
	err := w.ClientStream.RecvMsg(m)

	if err == nil && !w.desc.ServerStreams {
		w.sendStreamEvent(receiveEndEvent, nil)
	} else if err == io.EOF {
		w.sendStreamEvent(receiveEndEvent, nil)
	} else if err != nil {
		w.sendStreamEvent(errorEvent, err)
	} else {
		w.receivedMessageID++

		if w.receivedEvent {
			messageReceived.Event(w.Context(), w.receivedMessageID, m)
		}
	}

	return err
}

func (w *clientStream) SendMsg(m interface{}) error {
	err := w.ClientStream.SendMsg(m)

	w.sentMessageID++

	if w.sentEvent {
		messageSent.Event(w.Context(), w.sentMessageID, m)
	}

	if err != nil {
		w.sendStreamEvent(errorEvent, err)
	}

	return err
}

func (w *clientStream) Header() (metadata.MD, error) {
	md, err := w.ClientStream.Header()

	if err != nil {
		w.sendStreamEvent(errorEvent, err)
	}

	return md, err
}

func (w *clientStream) CloseSend() error {
	err := w.ClientStream.CloseSend()

	if err != nil {
		w.sendStreamEvent(errorEvent, err)
	}

	return err
}

func wrapClientStream(ctx context.Context, s grpc.ClientStream, desc *grpc.StreamDesc, cfg *config) *clientStream {
	events := make(chan streamEvent)
	eventsDone := make(chan struct{})
	finished := make(chan error)

	go func() {
		defer close(eventsDone)

		for {
			select {
			case event := <-events:
				switch event.Type {
				case receiveEndEvent:
					finished <- nil
					return
				case errorEvent:
					finished <- event.Err
					return
				}
			case <-ctx.Done():
				finished <- ctx.Err()
				return
			}
		}
	}()

	return &clientStream{
		ClientStream:  s,
		desc:          desc,
		events:        events,
		eventsDone:    eventsDone,
		finished:      finished,
		receivedEvent: cfg.ReceivedEvent,
		sentEvent:     cfg.SentEvent,
	}
}

func (w *clientStream) sendStreamEvent(eventType streamEventType, err error) {
	select {
	case <-w.eventsDone:
	case w.events <- streamEvent{Type: eventType, Err: err}:
	}
}

// StreamClientInterceptor returns a grpc.StreamClientInterceptor suitable
// for use in a grpc.Dial call.
func StreamClientInterceptor(opts ...Option) grpc.StreamClientInterceptor {
	cfg := newConfig(opts)
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		trace.WithInstrumentationVersion(Version()),
	)

	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		callOpts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		i := &InterceptorInfo{
			Method: method,
			Type:   StreamClient,
		}
		if cfg.Filter != nil && !cfg.Filter(i) {
			return streamer(ctx, desc, cc, method, callOpts...)
		}

		name, attr := clientSpanInfo(method, cc.Target())

		startOpts := append([]trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attr...)},
			cfg.SpanStartOptions...,
		)

		ctx, span := tracer.Start(
			ctx,
			name,
			startOpts...,
		)

		ctx = inject(ctx, cfg.Propagators)

		s, err := streamer(ctx, desc, cc, method, callOpts...)
		if err != nil {
			grpcStatus, _ := status.FromError(err)
			span.SetStatus(codes.Error, grpcStatus.Message())
			span.SetAttributes(statusCodeAttr(grpcStatus.Code()))
			span.End()
			return s, err
		}
		stream := wrapClientStream(ctx, s, desc, cfg)

		go func() {
			err := <-stream.finished

			if err != nil {
				s, _ := status.FromError(err)
				span.SetStatus(codes.Error, s.Message())
				span.SetAttributes(statusCodeAttr(s.Code()))
			} else {
				span.SetAttributes(statusCodeAttr(grpc_codes.OK))
			}

			span.End()
		}()

		return stream, nil
	}
}

// UnaryServerInterceptor returns a grpc.UnaryServerInterceptor suitable
// for use in a grpc.NewServer call.
func UnaryServerInterceptor(opts ...Option) grpc.UnaryServerInterceptor {
	cfg := newConfig(opts)
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		trace.WithInstrumentationVersion(Version()),
	)

	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		i := &InterceptorInfo{
			UnaryServerInfo: info,
			Type:            UnaryServer,
		}
		if cfg.Filter != nil && !cfg.Filter(i) {
			return handler(ctx, req)
		}

		ctx = extract(ctx, cfg.Propagators)
		name, attr := spanInfo(info.FullMethod, peerFromCtx(ctx))

		startOpts := append([]trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attr...)},
			cfg.SpanStartOptions...,
		)

		ctx, span := tracer.Start(
			trace.ContextWithRemoteSpanContext(ctx, trace.SpanContextFromContext(ctx)),
			name,
			startOpts...,
		)
		defer span.End()

		if cfg.ReceivedEvent {
			messageReceived.Event(ctx, 1, req)
		}

		var statusCode grpc_codes.Code
		defer func(t time.Time) {
			elapsedTime := time.Since(t) / time.Millisecond
			attr = append(attr, semconv.RPCGRPCStatusCodeKey.Int64(int64(statusCode)))
			o := metric.WithAttributes(attr...)
			cfg.rpcServerDuration.Record(ctx, int64(elapsedTime), o)
		}(time.Now())

		resp, err := handler(ctx, req)
		if err != nil {
			s, _ := status.FromError(err)
			statusCode, msg := serverStatus(s)
			span.SetStatus(statusCode, msg)
			span.SetAttributes(statusCodeAttr(s.Code()))
			if cfg.SentEvent {
				messageSent.Event(ctx, 1, s.Proto())
			}
		} else {
			statusCode = grpc_codes.OK
			span.SetAttributes(statusCodeAttr(grpc_codes.OK))
			if cfg.SentEvent {
				messageSent.Event(ctx, 1, resp)
			}
		}

		return resp, err
	}
}

// serverStream wraps around the embedded grpc.ServerStream, and intercepts the RecvMsg and
// SendMsg method call.
type serverStream struct {
	grpc.ServerStream
	ctx context.Context

	receivedMessageID int
	sentMessageID     int

	receivedEvent bool
	sentEvent     bool
}

func (w *serverStream) Context() context.Context {
	return w.ctx
}

func (w *serverStream) RecvMsg(m interface{}) error {
	err := w.ServerStream.RecvMsg(m)

	if err == nil {
		w.receivedMessageID++
		if w.receivedEvent {
			messageReceived.Event(w.Context(), w.receivedMessageID, m)
		}
	}

	return err
}

func (w *serverStream) SendMsg(m interface{}) error {
	err := w.ServerStream.SendMsg(m)

	w.sentMessageID++
	if w.sentEvent {
		messageSent.Event(w.Context(), w.sentMessageID, m)
	}

	return err
}

func wrapServerStream(ctx context.Context, ss grpc.ServerStream, cfg *config) *serverStream {
	return &serverStream{
		ServerStream:  ss,
		ctx:           ctx,
		receivedEvent: cfg.ReceivedEvent,
		sentEvent:     cfg.SentEvent,
	}
}

// StreamServerInterceptor returns a grpc.StreamServerInterceptor suitable
// for use in a grpc.NewServer call.
func StreamServerInterceptor(opts ...Option) grpc.StreamServerInterceptor {
	cfg := newConfig(opts)
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		trace.WithInstrumentationVersion(Version()),
	)

	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		ctx := ss.Context()
		i := &InterceptorInfo{
			StreamServerInfo: info,
			Type:             StreamServer,
		}
		if cfg.Filter != nil && !cfg.Filter(i) {
			return handler(srv, wrapServerStream(ctx, ss, cfg))
		}

		ctx = extract(ctx, cfg.Propagators)
		name, attr := spanInfo(info.FullMethod, peerFromCtx(ctx))

		startOpts := append([]trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attr...)},
			cfg.SpanStartOptions...,
		)

		ctx, span := tracer.Start(
			trace.ContextWithRemoteSpanContext(ctx, trace.SpanContextFromContext(ctx)),
			name,
			startOpts...,
		)
		defer span.End()

		err := handler(srv, wrapServerStream(ctx, ss, cfg))
		if err != nil {
			s, _ := status.FromError(err)
			statusCode, msg := serverStatus(s)
			span.SetStatus(statusCode, msg)
			span.SetAttributes(statusCodeAttr(s.Code()))
		} else {
			span.SetAttributes(statusCodeAttr(grpc_codes.OK))
		}

		return err
	}
}

// spanInfo returns a span name and all appropriate attributes from the gRPC
// method and peer address.
func spanInfo(fullMethod, peerAddress string) (string, []attribute.KeyValue) {
	attrs := []attribute.KeyValue{RPCSystemGRPC}
	name, mAttrs := internal.ParseFullMethod(fullMethod)
	attrs = append(attrs, mAttrs...)
	attrs = append(attrs, peerAttr(peerAddress)...)
	return name, attrs
}

// spanInfo returns a span name and all appropriate attributes from the gRPC
// method and peer address.
func clientSpanInfo(fullMethod, peerAddress string) (string, []attribute.KeyValue) {
	attrs := []attribute.KeyValue{RPCSystemGRPC}
	name, mAttrs := internal.ParseFullMethod(fullMethod)
	attrs = append(attrs, mAttrs...)
	attrs = append(attrs, clientPeerAttr(peerAddress)...)
	return name, attrs
}

// peerAttr returns attributes about the peer address.
func peerAttr(addr string) []attribute.KeyValue {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return []attribute.KeyValue(nil)
	}

	if host == "" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return []attribute.KeyValue(nil)
	}

	var attr []attribute.KeyValue
	if ip := net.ParseIP(host); ip != nil {
		attr = []attribute.KeyValue{
			ClientSocketAddressKey.String(host),
			ClientSocketPortKey.Int(port),
		}
	} else {
		attr = []attribute.KeyValue{
			ClientAddressKey.String(host),
			ClientPortKey.Int(port),
		}
	}

	return attr
}

func clientPeerAttr(addr string) []attribute.KeyValue {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return []attribute.KeyValue(nil)
	}

	if host == "" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return []attribute.KeyValue(nil)
	}

	var attr []attribute.KeyValue
	if ip := net.ParseIP(host); ip != nil {
		attr = []attribute.KeyValue{
			ServerSocketAddressKey.String(host),
			ServerSocketPortKey.Int(port),
		}
	} else {
		attr = []attribute.KeyValue{
			ServerAddressKey.String(host),
			ServerPortKey.Int(port),
		}
	}

	return attr
}

// peerFromCtx returns a peer address from a context, if one exists.
func peerFromCtx(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	return p.Addr.String()
}

// statusCodeAttr returns status code attribute based on given gRPC code.
func statusCodeAttr(c grpc_codes.Code) attribute.KeyValue {
	return GRPCStatusCodeKey.Int64(int64(c))
}

// serverStatus returns a span status code and message for a given gRPC
// status code. It maps specific gRPC status codes to a corresponding span
// status code and message. This function is intended for use on the server
// side of a gRPC connection.
//
// If the gRPC status code is Unknown, DeadlineExceeded, Unimplemented,
// Internal, Unavailable, or DataLoss, it returns a span status code of Error
// and the message from the gRPC status. Otherwise, it returns a span status
// code of Unset and an empty message.
func serverStatus(grpcStatus *status.Status) (codes.Code, string) {
	switch grpcStatus.Code() {
	case grpc_codes.Unknown,
		grpc_codes.DeadlineExceeded,
		grpc_codes.Unimplemented,
		grpc_codes.Internal,
		grpc_codes.Unavailable,
		grpc_codes.DataLoss:
		return codes.Error, grpcStatus.Message()
	default:
		return codes.Unset, ""
	}
}
