package http

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/speech"
	"github.com/sjzar/chatlog/internal/speech/whispercpp"
)

type Service struct {
	conf Config
	db   *database.Service

	router *gin.Engine
	server *http.Server

	mcpServer           *server.MCPServer
	mcpSSEServer        *server.SSEServer
	mcpStreamableServer *server.StreamableHTTPServer

	speechCfg  *conf.SpeechConfig
	speechOpts speech.Options
	speech     speech.Transcriber
	speechErr  error
}

type Config interface {
	GetHTTPAddr() string
	GetDataDir() string
	GetSpeech() *conf.SpeechConfig
}

func NewService(conf Config, db *database.Service) *Service {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Handle error from SetTrustedProxies
	if err := router.SetTrustedProxies(nil); err != nil {
		log.Err(err).Msg("Failed to set trusted proxies")
	}

	// Middleware
	router.Use(
		errors.RecoveryMiddleware(),
		errors.ErrorHandlerMiddleware(),
		gin.LoggerWithWriter(log.Logger, "/health"),
		corsMiddleware(),
	)

	s := &Service{
		conf:   conf,
		db:     db,
		router: router,
	}

	s.initMCPServer()
	s.initRouter()
	s.initSpeech()
	return s
}

func (s *Service) Start() error {

	s.server = &http.Server{
		Addr:    s.conf.GetHTTPAddr(),
		Handler: s.router,
	}

	go func() {
		// Handle error from Run
		if err := s.server.ListenAndServe(); err != nil {
			log.Err(err).Msg("Failed to start HTTP server")
		}
	}()

	log.Info().Msg("Starting HTTP server on " + s.conf.GetHTTPAddr())

	return nil
}

func (s *Service) ListenAndServe() error {

	s.server = &http.Server{
		Addr:    s.conf.GetHTTPAddr(),
		Handler: s.router,
	}

	log.Info().Msg("Starting HTTP server on " + s.conf.GetHTTPAddr())
	return s.server.ListenAndServe()
}

func (s *Service) Stop() error {

	if s.server == nil {
		return nil
	}

	// 使用超时上下文优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.server.Shutdown(ctx); err != nil {
		log.Debug().Err(err).Msg("Failed to shutdown HTTP server")
		return nil
	}

	log.Info().Msg("HTTP server stopped")
	s.shutdownSpeech()
	return nil
}

func (s *Service) GetRouter() *gin.Engine {
	return s.router
}

func (s *Service) initSpeech() {
	cfg := s.conf.GetSpeech()
	if cfg == nil || !cfg.Enabled {
		return
	}

	modelPath := strings.TrimSpace(cfg.Model)
	if modelPath == "" {
		log.Warn().Msg("speech config enabled but model path is empty; disabling speech backend")
		return
	}

	opts := cfg.ToOptions()
	transcriber, err := whispercpp.New(whispercpp.Config{ModelPath: modelPath, DefaultOptions: opts})
	if err != nil {
		log.Err(err).Str("model", modelPath).Msg("failed to initialize whisper backend; speech disabled")
		s.speechErr = err
		return
	}

	s.speechCfg = cfg
	s.speechOpts = opts
	s.speech = transcriber
	log.Info().Str("model", modelPath).Msg("speech-to-text backend initialized")
}

func (s *Service) shutdownSpeech() {
	if s.speech != nil {
		s.speech.Close()
		s.speech = nil
		log.Info().Msg("speech-to-text backend stopped")
	}
}
