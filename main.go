package main

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	_ "go.opentelemetry.io/otel/attribute"
	_ "go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric/global"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	_ "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func initGrpcTracer(ctx context.Context, l *zap.Logger) (*sdktrace.TracerProvider, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if tracesGrpcUrl == "" {
		l.Warn("otel grpc url not specified, defaulting to localhost:4317")
		tracesGrpcUrl = "localhost:4317"
	}
	conn, err := grpc.DialContext(dialCtx, tracesGrpcUrl, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}

	// Set up a trace exporter
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(bsp),
		// dummy hardcoded attributes to allow for traces to logs correlation
		// span attributes are used in LogQL query and must match log labels
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("job", "promtail"),
			attribute.String("container", "k6-grafana-prometheus-tempo_app_1"),
		)),
	)
	otel.SetTracerProvider(tracerProvider)

	// set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Shutdown will flush any remaining spans and shut down the exporter.
	go func() {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(ctx); err != nil {
			l.Error("could not shutdown tracer provider", zap.Error(err))
		}
	}()
	return tracerProvider, nil
}

func initMeter(ctx context.Context, l *zap.Logger) (*sdkmetric.MeterProvider, error) {
	exp, err := stdoutmetric.New()
	if err != nil {
		return nil, err
	}

	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	global.SetMeterProvider(mp)
	go func() {
		<-ctx.Done()
		l.Info("shutting down otel meter provider")
		shutCtx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFunc()
		if err := mp.Shutdown(shutCtx); err != nil {
			l.Error("error shutting down meter provider", zap.Error(err))
		}
	}()

	return mp, nil
}

var (
	logicPort     = os.Getenv("LOGIC_PORT")
	tracesGrpcUrl = os.Getenv("TRACE_GRPC_URL")
)

func main() {
	l := zap.NewExample()
	ctx := gracefulShutdown(context.Background(), l)
	tp, err := initGrpcTracer(ctx, l)
	if err != nil {
		l.Error("could not init grpc tracer", zap.Error(err))
		return
	}
	l.Info("grpc tracer started")

	prometheus.MustRegister(stableSummary, unstableSummary)

	if err != nil {
		l.Fatal("could not init meter", zap.Error(err))
	}

	t := tp.Tracer(tracerName)

	logicSrv := cancellableServer(ctx, l)
	logicSrv.BaseContext = baseContext(ctx)
	logicSrv.Handler = otelhttp.NewHandler(newLogicHandler(l, t), "logic routing")
	if logicPort == "" {
		l.Warn("logic port not specified, defaulting to 8080")
		logicPort = "8080"
	}
	logicSrv.Addr = fmt.Sprintf("0.0.0.0:%s", logicPort)

	go func() {
		l.Info("starting logic server")
		err := logicSrv.ListenAndServe()
		if err != nil {
			l.Fatal("could not start logic server", zap.Error(err))
		}
	}()

	promSrv := cancellableServer(ctx, l)
	promSrv.BaseContext = baseContext(ctx)
	promSrv.Handler = &promHandler{
		ctxLogger: setupZap(l),
		Handler: promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		})}
	promSrv.Addr = "0.0.0.0:9090"
	go func() {
		l.Info("starting prometheus server")
		err := promSrv.ListenAndServe()
		if err != nil {
			l.Fatal("could not start prometheus server", zap.Error(err))
		}
	}()

	l.Info("application started")
	_ = <-ctx.Done()
}

type promHandler struct {
	ctxLogger
	http.Handler
}

func (p *promHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/metrics":
		p.Handler.ServeHTTP(w, r)
	default:
		notFoundResponse(w)
	}
}

func baseContext(ctx context.Context) func(listener net.Listener) context.Context {
	return func(listener net.Listener) context.Context {
		return ctx
	}
}

func cancellableServer(ctx context.Context, l *zap.Logger) *http.Server {
	s := http.Server{}
	go func() {
		_ = <-ctx.Done()
		timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := s.Shutdown(timeout)
		if err != nil {
			l.Error("could not gracefully shutdown the http logicHandler", zap.Error(err))
		}
	}()
	return &s
}

func gracefulShutdown(parent context.Context, l *zap.Logger) context.Context {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	ctx, cancelFunc := context.WithCancel(parent)
	go func() {
		_ = <-c
		l.Info("received shutdown signal")
		cancelFunc()
	}()
	return ctx
}

type ctxLogger func(ctx context.Context) *zap.Logger

type logicHandler struct {
	logger           *zap.Logger
	stable, unstable http.Handler
}

func setupZap(logger *zap.Logger) func(ctx context.Context) *zap.Logger {
	return func(ctx context.Context) *zap.Logger {
		l := logger
		rawCid := ctx.Value("correlationId")
		if cId, ok := rawCid.(string); ok {
			l = l.With(zap.String("correlationId", cId))
		}

		span := trace.SpanFromContext(ctx)
		l = l.With(zap.String("TraceID", span.SpanContext().TraceID().String()))
		l = l.With(zap.String("SpanID", span.SpanContext().SpanID().String()))

		return l
	}
}

var (
	stableSummary = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "k6gpt",
		Subsystem: "stable",
		Name:      "http_requests_duration_millis",
		Help:      "Duration of http request for endpoint /stable",
		Buckets:   []float64{50, 150, 500, 1000, 5000},
	})
	unstableSummary = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "k6gpt",
		Subsystem: "unstable",
		Name:      "http_requests_duration_millis",
		Help:      "Duration of http request for endpoint /unstable",
		Buckets:   []float64{50, 150, 500, 1000, 5000},
	})
	tracerName = "k6gpt"
)

func newLogicHandler(l *zap.Logger, t trace.Tracer) *logicHandler {
	stable := otelhttp.NewHandler(
		&timingMiddleware{
			ctxLogger: setupZap(l),
			Histogram: stableSummary,
			Handler: &correlationIdMiddleware{
				Logger:  l,
				Handler: &stableHandler{setupZap(l)},
			},
		},
		"http-stable",
	)
	unstableRaw := otelhttp.NewHandler(
		&timingMiddleware{
			ctxLogger: setupZap(l),
			Histogram: unstableSummary,
			Handler: &unstableHandler{
				ctxLogger: setupZap(l),
				stabler: &tracingStabler{
					opName: "stabler",
					Tracer: t,
					stabler: &httpStabler{
						ctxLogger: setupZap(l),
						Client: http.Client{
							Transport: otelhttp.NewTransport(http.DefaultTransport),
						},
					},
				},
			},
		},
		"http-unstable",
	)
	unstable := &correlationIdMiddleware{
		Logger:  l,
		Handler: unstableRaw,
	}
	return &logicHandler{
		logger:   l,
		stable:   stable,
		unstable: unstable,
	}
}

func (s *logicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("handling new request", zap.String("uri", r.URL.String()))
	switch r.URL.Path {
	case "/stable":
		s.stable.ServeHTTP(w, r)
		break
	case "/unstable":
		s.unstable.ServeHTTP(w, r)
		break
	default:
		notFoundResponse(w)
	}
}

func notFoundResponse(w http.ResponseWriter) {
	w.WriteHeader(404)
	_, _ = w.Write([]byte("not found"))
}

type stableHandler struct {
	ctxLogger
}

func (s *stableHandler) ServeHTTP(writer http.ResponseWriter, r *http.Request) {
	log := s.ctxLogger(r.Context())
	log.Info("handling stable")
	writer.WriteHeader(200)
	_, _ = writer.Write([]byte("hello world"))
}

type unstableHandler struct {
	ctxLogger
	stabler
}

func (u *unstableHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	log := u.ctxLogger(request.Context())
	wrap(
		func(writer http.ResponseWriter, request *http.Request) {
			resp, err := u.stable(request.Context())
			if err != nil {
				log.Error("could not get stable response", zap.Error(err))
				writer.WriteHeader(500)
				log.Debug("sent response", zap.Int("code", 500))
				return
			}

			writer.WriteHeader(200)
			b, err := writer.Write([]byte(resp))
			if err != nil {
				log.Error("could not write response bytes", zap.Error(err))
			}

			log.Debug("sent response", zap.Int("code", 200), zap.Int("body_bytes", b))
		},
		func(next http.HandlerFunc) http.HandlerFunc {
			return injectFails(next)
		}, func(next http.HandlerFunc) http.HandlerFunc {
			return slowDown(log, next)
		},
	)(writer, request)
}

func injectFails(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		next(writer, request)
		return
	}
}

func slowDown(l *zap.Logger, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ms := rand.Intn(1000)
		span := trace.SpanFromContext(request.Context())
		span.AddEvent("slow down", trace.WithAttributes(attribute.Int("ms", ms)))
		l.Info("slowing down", zap.Int("ms", ms))
		time.Sleep(time.Duration(ms * int(time.Millisecond)))
		next.ServeHTTP(writer, request)
	}
}

type middleware func(next http.HandlerFunc) http.HandlerFunc

func wrap(f http.HandlerFunc, m ...middleware) http.HandlerFunc {
	handler := f
	for _, mid := range m {
		handler = mid(handler)
	}
	return handler
}

type correlationIdMiddleware struct {
	*zap.Logger
	http.Handler
}

func (m *correlationIdMiddleware) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctxCidMaybe := request.Context().Value("correlationId")
	ctxCid, ok := ctxCidMaybe.(string)
	if ok && len(ctxCid) != 0 {
		m.Logger.Debug("correlation id present in the context, not generating new one")
		m.Handler.ServeHTTP(writer, request)
		return
	}

	correlationId := request.Header.Get("correlation-id")
	if len(correlationId) == 0 {
		correlationId = strconv.Itoa(time.Now().Nanosecond())
	}

	withCid := context.WithValue(request.Context(), "correlationId", correlationId)
	m.Handler.ServeHTTP(writer, request.WithContext(withCid))
}

type stabler interface {
	stable(ctx context.Context) (string, error)
}

type tracingStabler struct {
	opName string
	trace.Tracer
	stabler
}

func (t *tracingStabler) stable(ctx context.Context) (string, error) {
	newCtx, span := otel.Tracer(tracerName).Start(ctx, t.opName)
	defer span.End()
	return t.stabler.stable(newCtx)
}

type httpStabler struct {
	ctxLogger
	http.Client
}

func (s *httpStabler) stable(ctx context.Context) (string, error) {
	log := s.ctxLogger(ctx)
	cId := ctx.Value("correlationId").(string)
	rawUrl := fmt.Sprintf("http://localhost:%s/stable", logicPort)
	parse, err := url.Parse(rawUrl)
	if err != nil {
		log.Fatal("invalid url", zap.String("url", rawUrl), zap.Error(err))
	}
	getReq := http.Request{
		Method: "GET",
		URL:    parse,
		Header: http.Header{
			"correlation-id": []string{cId},
		},
	}

	resp, err := s.Client.Do(getReq.WithContext(ctx))
	if err != nil {
		log.Error("could not send get request", zap.Error(err), zap.Any("request", getReq))
		return "", fmt.Errorf("could not send get request to %s. %v", parse.String(), err)
	}
	defer logErr(log, resp.Body.Close)
	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("could not read response bytes", zap.Error(err))
		return "", fmt.Errorf("could not read response bytes. %v", err)
	}
	return string(bytes), nil
}

type timingMiddleware struct {
	ctxLogger
	prometheus.Histogram
	http.Handler
}

func (t *timingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		ms := float64(time.Since(start).Milliseconds())
		l := t.ctxLogger(r.Context())
		l.Debug("observing", zap.Float64("ms", ms))
		span := trace.SpanFromContext(r.Context())
		tId := span.SpanContext().TraceID().String()
		t.Histogram.(prometheus.ExemplarObserver).ObserveWithExemplar(ms, prometheus.Labels{"traceId": tId})
	}()
	t.Handler.ServeHTTP(w, r)
}

func logErr(l *zap.Logger, f func() error) {
	err := f()
	if err != nil {
		l.Error("unexpected error: %v", zap.Error(err))
	}
}
