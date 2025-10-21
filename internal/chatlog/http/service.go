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
)

type Service struct {
	conf    Config
	db      *database.Service
	control Control

	router *gin.Engine
	server *http.Server

	mcpServer           *server.MCPServer
	mcpSSEServer        *server.SSEServer
	mcpStreamableServer *server.StreamableHTTPServer

}

type Config interface {
	GetHTTPAddr() string
	SetHTTPAddr(string)
	GetDataDir() string
	SetDataDir(string)
	GetWorkDir() string
	SetWorkDir(string)
	GetDataKey() string
	SetDataKey(string)
	GetImgKey() string
	SetImgKey(string)
	IsHTTPEnabled() bool
	IsAutoDecrypt() bool
	GetSpeech() *conf.SpeechConfig
}

type Control interface {
	GetDataKey() error
	DecryptDBFiles() error
	StartService() error
	StopService() error
	StartAutoDecrypt() error
	StopAutoDecrypt() error
	SaveSpeechConfig(cfg *conf.SpeechConfig) error
	SetHTTPAddr(addr string) error
}

func NewService(conf Config, db *database.Service, control Control) *Service {
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
		conf:    conf,
		db:      db,
		control: control,
		router:  router,
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

	speechCfg.Normalize()

	provider := strings.ToLower(speechCfg.Provider)
	switch provider {
	default:
		log.Warn().Str("provider", speechCfg.Provider).Msg("unsupported speech provider; speech transcription disabled")
	}
}

func (s *Service) ReloadSpeech() {
	s.initSpeech(s.conf)
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
	return nil
}

func (s *Service) GetRouter() *gin.Engine {
	return s.router
}
