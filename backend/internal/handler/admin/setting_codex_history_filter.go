package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetCodexHistoryFilter(c *gin.Context) {
	status, err := h.settingService.GetCodexHistoryFilterStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

func (h *SettingHandler) UpdateCodexHistoryFilter(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "enabled must be a boolean")
		return
	}
	if err := h.settingService.SetCodexHistoryFilterEnabled(c.Request.Context(), *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.GetCodexHistoryFilter(c)
}
