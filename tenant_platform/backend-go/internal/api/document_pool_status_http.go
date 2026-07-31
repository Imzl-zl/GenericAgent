package api

import "net/http"

func (s *Server) handleGetDocumentPoolStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.documentPoolStatus.GetDocumentPoolStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DOCUMENT_POOL_STATUS_FAILED", "document pool status lookup failed", traceID())
		return
	}
	writeJSON(w, http.StatusOK, status)
}
