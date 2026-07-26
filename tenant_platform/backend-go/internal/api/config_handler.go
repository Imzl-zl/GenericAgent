package api

import (
	"net/http"
)

// handleGetMykeyConfig 生成并返回 mykey.py 配置文件内容
// Worker 启动时调用此接口获取最新的 LLM Provider 配置
func (s *Server) handleGetMykeyConfig(w http.ResponseWriter, r *http.Request) {
	tid := traceID()
	if s.llmProviders == nil || s.cipher == nil {
		writeErr(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "LLM provider management not configured", tid)
		return
	}

	// 1. 从数据库读取所有 Provider
	providers, err := s.llmProviders.ListProviders(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_PROVIDERS_FAILED", err.Error(), tid)
		return
	}

	// 2. 生成 mykey.py 内容
	content, err := GenerateMykeyPy(providers, s.cipher)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "GENERATE_MYKEY_FAILED", err.Error(), tid)
		return
	}

	// 3. 返回纯文本
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(content))
}
