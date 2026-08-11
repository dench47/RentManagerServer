package handler

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"

	"github.com/gin-gonic/gin"
)

type VersionResponse struct {
	VersionCode      int    `json:"version_code"`
	VersionName      string `json:"version_name"`
	MinClientVersion int    `json:"min_client_version"`
	APKURL           string `json:"apk_url"`
	ForceUpdate      bool   `json:"force_update"`
	ReleaseNotes     string `json:"release_notes"`
}

type VersionHandler struct {
	jsonPath   string
	apkBaseURL string
	mu         sync.RWMutex
	cached     *VersionResponse
}

func NewVersionHandler(jsonPath, apkBaseURL string) *VersionHandler {
	return &VersionHandler{jsonPath: jsonPath, apkBaseURL: apkBaseURL}
}

func (h *VersionHandler) GetVersion(c *gin.Context) {
	resp := h.load()
	c.JSON(http.StatusOK, resp)
}

func (h *VersionHandler) load() *VersionResponse {
	h.mu.RLock()
	if h.cached != nil {
		defer h.mu.RUnlock()
		return h.cached
	}
	h.mu.RUnlock()

	h.mu.Lock()
	defer h.mu.Unlock()

	data, err := os.ReadFile(h.jsonPath)
	if err != nil {
		// Fallback defaults
		return &VersionResponse{
			VersionCode:      1,
			VersionName:      "1.0.0",
			MinClientVersion: 1,
			APKURL:           h.apkBaseURL + "/downloads/app-release.apk",
			ForceUpdate:      false,
			ReleaseNotes:     "",
		}
	}

	var v VersionResponse
	if err := json.Unmarshal(data, &v); err != nil {
		return &VersionResponse{
			VersionCode:      1,
			VersionName:      "1.0.0",
			MinClientVersion: 1,
			APKURL:           h.apkBaseURL + "/downloads/app-release.apk",
			ForceUpdate:      false,
		}
	}

	if v.APKURL == "" {
		v.APKURL = h.apkBaseURL + "/downloads/app-release.apk"
	}
	if v.VersionCode == 0 {
		v.VersionCode = 1
	}
	if v.VersionName == "" {
		v.VersionName = "1.0.0"
	}
	if v.MinClientVersion == 0 {
		v.MinClientVersion = 1
	}

	h.cached = &v
	return h.cached
}
