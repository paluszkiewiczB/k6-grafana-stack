package main

import (
	"context"
	"go.uber.org/zap"
	stdlog "log"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

func main() {
	l, err := zap.NewProduction()
	if err != nil {
		stdlog.Fatal(err)
	}
	err = http.ListenAndServe("0.0.0.0:8080", newServer(l))
	if err != nil {
		l.Fatal("could not start http server", zap.Error(err))
	}
}

type ctxLogger func(ctx context.Context) *zap.Logger

type server struct {
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

func newServer(l *zap.Logger) *server {
	stableRaw := &stableHandler{setupZap(l)}
	stable := &correlationIdMiddleware{
		Logger:  l,
		Handler: stableRaw,
	}
	unstableRaw := &unstableHandler{
		ctxLogger:     setupZap(l),
		stableHandler: stableRaw,
	}
	unstable := &correlationIdMiddleware{
		Logger:  l,
		Handler: unstableRaw,
	}
	return &server{
		logger:   l,
		stable:   stable,
		unstable: unstable,
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("handling new request")
	switch r.URL.Path {
	case "/stable":
		s.stable.ServeHTTP(w, r)
		break
	case "/unstable":
		s.unstable.ServeHTTP(w, r)
		break
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not found"))
	}
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
	*stableHandler
}

func (u *unstableHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	log := u.ctxLogger(request.Context())
	wrap(u.stableHandler.ServeHTTP, func(next http.HandlerFunc) http.HandlerFunc {
		return injectFails(log, next)
	}, func(next http.HandlerFunc) http.HandlerFunc {
		return slowDown(log, next)
	})(writer, request)
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
