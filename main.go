package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	pkgerr "github.com/pkg/errors"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"gopkg.in/natefinch/lumberjack.v2"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

var tracer trace.Tracer

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {

	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tracing: %v\n", err)
		return 1
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "failed to shutdown tracing: %v\n", err)
		}
	}()

	logger, cleanup, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := cleanup(); err != nil {
			logger.Error("failed to close logger", "error", err)
		}
	}()
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", "error", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", "error", serverErr)
		return 1
	}
	return 0
}

type closeFunc func() error

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var (
		handlers []slog.Handler
		closers  []closeFunc
	)

	logger := &lumberjack.Logger{
	Filename:   logFile,
	MaxSize:    1,
	MaxAge:     28,
	MaxBackups: 10,
	LocalTime:  false,
	Compress:   true,
	}

	// First initialize the console logger
	handlers = append(handlers, tint.NewTextHandler(os.Stderr, &tint.Options{
		ReplaceAttr: replaceAttr,
		NoColor:     !(isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())),
	}))

	handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
		ReplaceAttr: replaceAttr,
	}))

	closers = append(closers, func() error {
			if err := logger.Close(); err != nil {
				return fmt.Errorf("failed to close log file: %w", err)
			}
			return nil
		})

	close := func() error {
		var errs []error
		for _, closer := range closers {
			errs = append(errs, closer())
		}
		return errors.Join(errs...)
	}
	return slog.New(slog.NewMultiHandler(handlers...)), close, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr{
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		if multiErr, ok := errors.AsType[multiError](err); ok {
			attrs := []slog.Attr{}
			count := 1
			for _, e := range multiErr.Unwrap() {
				attrs = append(attrs, slog.GroupAttrs(fmt.Sprintf("error_%v", count), errorAttrs(e)...))
				count++
			}
			return slog.GroupAttrs("error", attrs...)
		}

		return slog.GroupAttrs("error", errorAttrs(err)...)
	}

	var sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "user"}
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}

	u, err := url.Parse(a.Value.String())
	if err != nil {
		return a
	}

	if u.User == nil {
		return a
	}

	if p, ok := u.User.Password(); ok && p != "" {
		u.User = url.UserPassword(u.User.Username(), "REDACTED")
		return slog.String(a.Key, u.String())
	}

	return a
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
			{
				Key: "message",
				Value: slog.StringValue(err.Error()),
			},
		}

		attrs = append(attrs, linkoerr.Attrs(err)...)

		if stackErr, ok := errors.AsType[stackTracer](err); ok {
			attrs = append(attrs, slog.Attr{
				Key: "stack_trace",
				Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
			})
		}
	return attrs
}

func initTracing(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(resource.Default()),
	)

	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("boot.dev/linko")
	return tp.Shutdown, nil
}