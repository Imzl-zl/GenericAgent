package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 出站 outbox 管理端点(2026-08-14 审查 E2): 08-14 事故恢复曾靠手动 SQL
// 重投死信; 管理员需要可审计的查询与重投能力。均为 AdminToken 门禁。

const (
	maxAdminDeliveryListLimit = 200
)

// handleAdminListDeliveries: GET /v1/admin/deliveries?status=dead_letter&limit=100
// 返回 delivery 管理视图(默认只列死信, 最新在前)。
func (s *Server) handleAdminListDeliveries(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "dead_letter"
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "INVALID_LIMIT", "limit must be a positive integer", traceID())
			return
		}
		limit = n
	}
	if limit > maxAdminDeliveryListLimit {
		limit = maxAdminDeliveryListLimit
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := s.deliveries.ListDeliveries(ctx, status, limit)
	if err != nil {
		writeStoreError(w, err, "DELIVERY_LIST_FAILED", traceID())
		return
	}
	type rowView struct {
		DeliveryID   string  `json:"delivery_id"`
		TaskID       string  `json:"task_id"`
		DeliveryType string  `json:"delivery_type"`
		Status       string  `json:"status"`
		ErrorCode    string  `json:"error_code,omitempty"`
		ErrorMessage string  `json:"error_message,omitempty"`
		AttemptCount int     `json:"attempt_count"`
		CreatedAt    string  `json:"created_at"`
		TerminalAt   *string `json:"terminal_at,omitempty"`
		RequeuedAt   *string `json:"requeued_at,omitempty"`
	}
	views := make([]rowView, 0, len(rows))
	for _, rw := range rows {
		v := rowView{
			DeliveryID:   rw.DeliveryID,
			TaskID:       rw.TaskID,
			DeliveryType: string(rw.DeliveryType),
			Status:       string(rw.Status),
			ErrorCode:    rw.ErrorCode,
			ErrorMessage: rw.ErrorMessage,
			AttemptCount: rw.AttemptCount,
			CreatedAt:    rw.CreatedAt.UTC().Format(time.RFC3339),
		}
		if rw.TerminalAt != nil {
			ts := rw.TerminalAt.UTC().Format(time.RFC3339)
			v.TerminalAt = &ts
		}
		if rw.RequeuedAt != nil {
			rs := rw.RequeuedAt.UTC().Format(time.RFC3339)
			v.RequeuedAt = &rs
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": views})
}

// handleAdminRequeueDelivery: POST /v1/admin/deliveries/{delivery_id}/requeue
// 把死信行重置为 pending 供 delivery 循环重投(attempt_count 归零)。
func (s *Server) handleAdminRequeueDelivery(w http.ResponseWriter, r *http.Request) {
	deliveryID := r.PathValue("delivery_id")
	if strings.TrimSpace(deliveryID) == "" {
		writeErr(w, http.StatusBadRequest, "MISSING_DELIVERY_ID", "delivery_id is required", traceID())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	found, err := s.deliveries.RequeueDeadLetterDelivery(ctx, deliveryID, time.Now().UTC())
	if err != nil {
		writeStoreError(w, err, "DELIVERY_REQUEUE_FAILED", traceID())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "DELIVERY_NOT_FOUND",
			"no dead_letter delivery with this id (only dead_letter rows can be requeued)", traceID())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requeued": true})
}
