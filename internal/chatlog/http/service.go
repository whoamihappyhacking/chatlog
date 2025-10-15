package http

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/whisper"
)

type Service struct {
	conf Config
	db   *database.Service

	router *gin.Engine
	server *http.Server

	mcpServer           *server.MCPServer
	mcpSSEServer        *server.SSEServer
	mcpStreamableServer *server.StreamableHTTPServer

	speechTranscriber whisper.Transcriber
	speechOptions     whisper.Options
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
	s.initSpeech(conf)
	return s
}

func (s *Service) initSpeech(cfg Config) {
	speechCfg := cfg.GetSpeech()
	if speechCfg == nil || !speechCfg.Enabled {
		return
	}

	opts := speechCfg.ToOptions()
	scriptDir := speechCfg.ScriptDir
	if scriptDir == "" {
		scriptDir = filepath.Join(cfg.GetDataDir(), "whisper")
	}
	pTranscriber, err := whisper.NewPythonTranscriber(whisper.PythonConfig{
		ScriptDir:      scriptDir,
		PythonPath:     speechCfg.PythonExec,
		DefaultOptions: opts,
		Env:            speechCfg.Env,
	})
	if err != nil {
		log.Err(err).Msg("initialise python whisper transcriber failed")
		return
	}

	s.speechTranscriber = pTranscriber
	s.speechOptions = opts
	log.Info().Str("script", pTranscriber.ScriptPath()).Msg("speech transcription backend initialised via python whisper")
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

	if s.speechTranscriber != nil {
		s.speechTranscriber.Close()
		s.speechTranscriber = nil
	}

	log.Info().Msg("HTTP server stopped")
	return nil
}

func (s *Service) GetRouter() *gin.Engine {
	return s.router
}
