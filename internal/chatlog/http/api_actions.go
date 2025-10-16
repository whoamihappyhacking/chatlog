package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

func (s *Service) handleActionGetDataKey(c *gin.Context) {
	if s.control == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
		return
	}
	if err := s.control.GetDataKey(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Service) handleActionDecrypt(c *gin.Context) {
	if s.control == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
		return
	}
	if err := s.control.DecryptDBFiles(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Service) handleActionStartHTTP(c *gin.Context) {
	if s.control == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
		return
	}
	if s.conf.IsHTTPEnabled() {
		c.JSON(http.StatusOK, gin.H{"status": "already_running"})
		return
	}
	go func() {
		if err := s.control.StartService(); err != nil {
			log.Err(err).Msg("failed to start http service via api")
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"status": "starting"})
}

func (s *Service) handleActionStopHTTP(c *gin.Context) {
	if s.control == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
		return
	}
	if !s.conf.IsHTTPEnabled() {
		c.JSON(http.StatusOK, gin.H{"status": "already_stopped"})
		return
	}
	go func() {
		if err := s.control.StopService(); err != nil {
			log.Err(err).Msg("failed to stop http service via api")
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"status": "stopping"})
}

func (s *Service) handleActionStartAutoDecrypt(c *gin.Context) {
	if s.control == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
		return
	}
	if err := s.control.StartAutoDecrypt(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Service) handleActionStopAutoDecrypt(c *gin.Context) {
	if s.control == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
		return
	}
	if err := s.control.StopAutoDecrypt(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
