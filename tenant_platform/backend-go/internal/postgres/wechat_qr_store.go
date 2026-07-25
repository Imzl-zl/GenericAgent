package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// CreateWechatQRSession inserts a new QR login session.
func (s *Store) CreateWechatQRSession(ctx context.Context, userID int64, ilinkQRCode, imgURL string, expiresAt time.Time) (domain.WechatQRSession, error) {
	if userID <= 0 {
		return domain.WechatQRSession{}, fmt.Errorf("user id must be positive")
	}
	if ilinkQRCode == "" {
		return domain.WechatQRSession{}, fmt.Errorf("ilink qrcode is required")
	}
	if imgURL == "" {
		return domain.WechatQRSession{}, fmt.Errorf("qrcode img url is required")
	}
	var sess domain.WechatQRSession
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanWechatQRSession(tx.QueryRow(ctx, `
INSERT INTO wechat_qr_sessions (id, user_id, ilink_qrcode, qrcode_img_url, status, expires_at)
VALUES ($1::uuid, $2, $3, $4, 'wait', $5)
RETURNING id, user_id, ilink_qrcode, qrcode_img_url, status, ilink_bot_id, ilink_user_id,
          bot_token_ciphertext, baseurl, expires_at, created_at, updated_at
`, uuid.New().String(), userID, ilinkQRCode, imgURL, expiresAt), &sess)
	})
	return sess, err
}

// GetWechatQRSessionByQRCode returns a session by its iLink QR code token.
func (s *Store) GetWechatQRSessionByQRCode(ctx context.Context, qrCode string) (domain.WechatQRSession, error) {
	if qrCode == "" {
		return domain.WechatQRSession{}, fmt.Errorf("ilink qrcode is required")
	}
	var sess domain.WechatQRSession
	err := scanWechatQRSession(s.pool.QueryRow(ctx, `
SELECT id, user_id, ilink_qrcode, qrcode_img_url, status, ilink_bot_id, ilink_user_id,
       bot_token_ciphertext, baseurl, expires_at, created_at, updated_at
FROM wechat_qr_sessions WHERE ilink_qrcode = $1
`, qrCode), &sess)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.WechatQRSession{}, fmt.Errorf("qr session not found")
	}
	return sess, err
}

// UpdateWechatQRSessionStatus updates the session status and, on confirmation, stores credentials.
func (s *Store) UpdateWechatQRSessionStatus(ctx context.Context, id string, status domain.WechatQRStatus,
	ilinkBotID, ilinkUserID, baseurl string, tokenCiphertext []byte) (domain.WechatQRSession, error) {
	if id == "" {
		return domain.WechatQRSession{}, fmt.Errorf("session id is required")
	}
	var sess domain.WechatQRSession
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		return scanWechatQRSession(tx.QueryRow(ctx, `
UPDATE wechat_qr_sessions
SET status = $2, ilink_bot_id = $3, ilink_user_id = $4, baseurl = $5,
    bot_token_ciphertext = $6, updated_at = $7
WHERE id = $1::uuid
RETURNING id, user_id, ilink_qrcode, qrcode_img_url, status, ilink_bot_id, ilink_user_id,
          bot_token_ciphertext, baseurl, expires_at, created_at, updated_at
`, id, status, nullString(ilinkBotID), nullString(ilinkUserID), nullString(baseurl), tokenCiphertext, time.Now().UTC()), &sess)
	})
	return sess, err
}

func scanWechatQRSession(row pgx.Row, s *domain.WechatQRSession) error {
	var ilinkBotID, ilinkUserID, baseurl *string
	var tokenCiphertext []byte
	err := row.Scan(&s.ID, &s.UserID, &s.ILINKQRCode, &s.QRCodeImgURL, &s.Status, &ilinkBotID, &ilinkUserID,
		&tokenCiphertext, &baseurl, &s.ExpiresAt, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return err
	}
	if ilinkBotID != nil {
		s.ILINKBotID = *ilinkBotID
	}
	if ilinkUserID != nil {
		s.ILINKUserID = *ilinkUserID
	}
	if baseurl != nil {
		s.BaseURL = *baseurl
	}
	s.BotTokenCiphertext = tokenCiphertext
	return nil
}
