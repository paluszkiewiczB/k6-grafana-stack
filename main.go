package main

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	_ "go.opentelemetry.io/otel/attribute"
	_ "go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric/global"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	"go.opentelemetry.io/otel/trace"
	_ "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
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

func initTracer(ctx context.Context, l *zap.Logger) (*sdktrace.TracerProvider, error) {
	exporter, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceNameKey.String(tracerName))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	go func() {
		<-ctx.Done()
		l.Info("shutting down otel tracer provider")
		shutCtx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFunc()
		if err := tp.Shutdown(shutCtx); err != nil {
			l.Error("error shutting down tracer provider", zap.Error(err))
		}
	}()

	return tp, err
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

func main() {
	l := zap.NewExample()
	back := context.Background()
	ctx := gracefulShutdown(back, l)

	tp, err := initTracer(ctx, l)
	if err != nil {
		l.Fatal("could not init tracer", zap.Error(err))
	}

	_, err = initMeter(ctx, l)
	if err != nil {
		l.Fatal("could not init meter", zap.Error(err))
	}

	t := tp.Tracer(tracerName)

	logicSrv := cancellableServer(ctx, l)
	logicSrv.BaseContext = baseContext(ctx)
	logicSrv.Handler = otelhttp.NewHandler(newLogicHandler(l, t), "logic routing")
	logicSrv.Addr = "0.0.0.0:8080"

	go func() {
		l.Info("starting logic server")
		err := logicSrv.ListenAndServe()
		if err != nil {
			l.Fatal("could not start logic server", zap.Error(err))
		}
	}()

	promSrv := cancellableServer(ctx, l)
	promSrv.BaseContext = baseContext(ctx)
	promSrv.Handler = &promHandler{promhttp.Handler()}
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
		return l
	}
}

var (
	stableOk = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "k6gpt",
		Subsystem: "stable",
		Name:      "http_requests_success_total",
		Help:      "Total number of successfully handled http requests",
	})
	stableTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "k6gpt",
		Subsystem: "stable",
		Name:      "http_requests_total",
		Help:      "Total number of handled http requests",
	})
	unstableOk = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "k6gpt",
		Subsystem: "unstable",
		Name:      "http_requests_success_total",
		Help:      "Total number of successfully handled http requests",
	})
	unstableTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "k6gpt",
		Subsystem: "unstable",
		Name:      "http_requests_total",
		Help:      "Total number of handled http requests",
	})
	tracerName = "k6gpt"
)

func newLogicHandler(l *zap.Logger, t trace.Tracer) *logicHandler {
	stable := otelhttp.NewHandler(
		&countingMiddleware{
			ok:    stableOk,
			total: stableTotal,
			Handler: &correlationIdMiddleware{
				Logger:  l,
				Handler: &stableHandler{setupZap(l)},
			},
		},
		"http-stable",
	)
	unstableRaw := otelhttp.NewHandler(
		&countingMiddleware{
			ok:    unstableOk,
			total: unstableTotal,
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
	ms := rand.Intn(1000)
	l.Info("slowing down", zap.Int("ms", ms))
	time.Sleep(time.Duration(ms * int(time.Millisecond)))
	return next
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
	rawUrl := "http://localhost:8080/stable"
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

type countingMiddleware struct {
	ok, total prometheus.Counter
	http.Handler
}

func (m *countingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer m.total.Inc()
	writer := &statusHoldingResponseWriter{ResponseWriter: w}
	m.Handler.ServeHTTP(writer, r)
	if writer.status < 500 {
		m.ok.Inc()
	}
}

func logErr(l *zap.Logger, f func() error) {
	err := f()
	if err != nil {
		l.Error("unexpected error: %v", zap.Error(err))
	}
}

type statusHoldingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusHoldingResponseWriter) WriteHeader(statusCode int) {
	s.status = statusCode
	s.ResponseWriter.WriteHeader(statusCode)
}
