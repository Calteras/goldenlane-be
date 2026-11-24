package httpserver

import (
	"errors"
	"time"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v2"
)

const (
	_defaultAddr            = ":80"
	_defaultReadTimeout     = 5 * time.Second
	_defaultWriteTimeout    = 5 * time.Second
	_defaultShutdownTimeout = 3 * time.Second
)

// Server holds your Fiber app and config.
type Server struct {
	App    *fiber.App
	notify chan error

	address         string
	prefork         bool
	readTimeout     time.Duration
	writeTimeout    time.Duration
	shutdownTimeout time.Duration
}

// New creates the Server and injects the global ErrorHandler.
func New(opts ...Option) *Server {
	s := &Server{
		App:             nil,
		notify:          make(chan error, 1),
		address:         _defaultAddr,
		readTimeout:     _defaultReadTimeout,
		writeTimeout:    _defaultWriteTimeout,
		shutdownTimeout: _defaultShutdownTimeout,
	}

	// apply any custom options
	for _, opt := range opts {
		opt(s)
	}

	app := fiber.New(fiber.Config{
		Prefork:      s.prefork,
		ReadTimeout:  s.readTimeout,
		WriteTimeout: s.writeTimeout,
		JSONDecoder:  json.Unmarshal,
		JSONEncoder:  json.Marshal,

		ErrorHandler: errorHandler,
	})

	s.App = app
	return s
}

// Start and Shutdown remain the same...
func (s *Server) Start() {
	go func() {
		s.notify <- s.App.Listen(s.address)
		close(s.notify)
	}()
}

func (s *Server) Notify() <-chan error {
	return s.notify
}

func (s *Server) Shutdown() error {
	return s.App.ShutdownWithTimeout(s.shutdownTimeout)
}

// errorHandler renders every error as the JSON envelope. AppErrors (also when
// wrapped) and Fiber's own errors, such as 404 for an unknown route or 413 for an
// oversized body, keep their status; anything else is a 500 that hides the cause.
func errorHandler(c *fiber.Ctx, err error) error {
	code, message, reason := fiber.StatusInternalServerError, "internal server error", ""

	var data any

	var appErr *AppError

	var fiberErr *fiber.Error

	switch {
	case errors.As(err, &appErr):
		code, message, reason, data = appErr.Code, appErr.Message, appErr.Reason, appErr.Data
	case errors.As(err, &fiberErr):
		code, message = fiberErr.Code, fiberErr.Message
	}

	body := fiber.Map{
		"status":  false,
		"message": message,
		"data":    data,
	}
	if reason != "" {
		body["code"] = reason
	}

	return c.Status(code).JSON(body)
}
