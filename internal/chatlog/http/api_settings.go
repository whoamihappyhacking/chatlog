package http

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sjzar/chatlog/internal/chatlog/conf"
)

type settingRequest struct {
	HTTPAddr *string            `json:"http_addr"`
	WorkDir  *string            `json:"work_dir"`
	DataDir  *string            `json:"data_dir"`
	DataKey  *string            `json:"data_key"`
	ImgKey   *string            `json:"img_key"`
	Speech   *conf.SpeechConfig `json:"speech"`
}

type settingResponse struct {
	HTTPAddr    string             `json:"http_addr"`
	HTTPEnabled bool               `json:"http_enabled"`
	WorkDir     string             `json:"work_dir"`
	DataDir     string             `json:"data_dir"`
	DataKey     string             `json:"data_key"`
	ImgKey      string             `json:"img_key"`
	AutoDecrypt bool               `json:"auto_decrypt"`
	Speech      *conf.SpeechConfig `json:"speech"`
}

func (s *Service) handleGetSetting(c *gin.Context) {
	c.JSON(http.StatusOK, s.buildSettingResponse())
}

func (s *Service) handleUpdateSetting(c *gin.Context) {
	var req settingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload", "detail": err.Error()})
		return
	}

	if req.HTTPAddr != nil {
		if s.control == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
			return
		}
		trimmed := strings.TrimSpace(*req.HTTPAddr)
		if err := s.control.SetHTTPAddr(trimmed); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	if req.WorkDir != nil {
		s.conf.SetWorkDir(strings.TrimSpace(*req.WorkDir))
	}

	if req.DataDir != nil {
		s.conf.SetDataDir(strings.TrimSpace(*req.DataDir))
	}

	if req.DataKey != nil {
		s.conf.SetDataKey(strings.TrimSpace(*req.DataKey))
	}

	if req.ImgKey != nil {
		s.conf.SetImgKey(strings.TrimSpace(*req.ImgKey))
	}

	if req.Speech != nil {
		if s.control == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "control service unavailable"})
			return
		}
		speechCopy := *req.Speech
		if err := s.control.SaveSpeechConfig(&speechCopy); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, s.buildSettingResponse())
}

func (s *Service) buildSettingResponse() settingResponse {
	resp := settingResponse{
		HTTPAddr:    s.conf.GetHTTPAddr(),
		HTTPEnabled: s.conf.IsHTTPEnabled(),
		WorkDir:     s.conf.GetWorkDir(),
		DataDir:     s.conf.GetDataDir(),
		DataKey:     s.conf.GetDataKey(),
		ImgKey:      s.conf.GetImgKey(),
		AutoDecrypt: s.conf.IsAutoDecrypt(),
	}

	if cfg := s.conf.GetSpeech(); cfg != nil {
		copyCfg := *cfg
		resp.Speech = &copyCfg
	}

	return resp
}
