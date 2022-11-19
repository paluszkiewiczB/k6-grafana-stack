package main

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

func main() {
	back := context.Background()
	l := zap.NewExample()
	ctx := gracefulShutdown(back, l)

	logicSrv := cancellableServer(ctx, l)
	logicSrv.BaseContext = baseContext(ctx)
	logicSrv.Handler = newLogicHandler(l)
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
		l.Info("shutting down")
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

func newLogicHandler(l *zap.Logger) *logicHandler {
	stableRaw := &stableHandler{setupZap(l)}
	stable := &correlationIdMiddleware{
		Logger:  l,
		Handler: stableRaw,
	}
	unstableRaw := &unstableHandler{
		ctxLogger: setupZap(l),
		stabler: &httpStabler{
			ctxLogger: setupZap(l),
			Client:    http.Client{},
		},
	}
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
			return injectFails(log, next)
		}, func(next http.HandlerFunc) http.HandlerFunc {
			return slowDown(log, next)
		},
	)(writer, request)
}

func injectFails(l *zap.Logger, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if rand.Intn(2) == 0 {
			next(writer, request)
			return
		}

		l.Info("random fail")
		writer.WriteHeader(500)
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

	resp, err := http.DefaultClient.Do(&getReq)
	if err != nil {
		log.Error("could not send get request", zap.Error(err), zap.Any("request", getReq))
		return "", fmt.Errorf("could not send get request to %s. %v", parse.String(), err)
	}

	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("could not read response bytes", zap.Error(err))
		return "", fmt.Errorf("could not read response bytes. %v", err)
	}
	return string(bytes), nil
}
