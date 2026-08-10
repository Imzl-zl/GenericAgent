package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// TestChannelConfigCRUD covers the 0053 统一渠道模型: 微信行默认 channel_type,
// 每用户每渠道唯一, 凭据 upsert/解绑状态机。
func TestChannelConfigCRUD(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	// 微信扫码落库(存量语义): CreateChannelConfigFromQRSession。
	sess := domain.WechatQRSession{
		ID:                 "qr-1",
		UserID:             dev.UserID,
		ILINKBotID:         "ilink-bot-1",
		ILINKUserID:        "ilink-user-1",
		BotTokenCiphertext: []byte("ct-wechat"),
	}
	cfg, err := store.CreateChannelConfigFromQRSession(ctx, sess, 1)
	if err != nil {
		t.Fatalf("create wechat config: %v", err)
	}
	if cfg.ChannelType != domain.ChannelWechat || !cfg.IsBound() {
		t.Fatalf("wechat config: type=%s bound=%v", cfg.ChannelType, cfg.IsBound())
	}
	if cfg.IlinkUserID != "ilink-user-1" {
		t.Fatalf("ilink user id = %q", cfg.IlinkUserID)
	}

	// 每用户每渠道一行: 同渠道重复绑定走 upsert(新 UUID), 不报唯一冲突。
	rebound, err := store.CreateChannelConfigFromQRSession(ctx, domain.WechatQRSession{
		ID: "qr-2", UserID: dev.UserID,
		ILINKBotID: "ilink-bot-2", ILINKUserID: "ilink-user-2",
		BotTokenCiphertext: []byte("ct-wechat-2"),
	}, 1)
	if err != nil {
		t.Fatalf("rebind wechat: %v", err)
	}
	if rebound.BotUUID == cfg.BotUUID {
		t.Fatal("rebind must generate a fresh bot_uuid")
	}
	byOwner, err := store.GetChannelConfigByOwnerAndType(ctx, dev.UserID, domain.ChannelWechat)
	if err != nil {
		t.Fatalf("get by owner: %v", err)
	}
	if byOwner.BotUUID != rebound.BotUUID {
		t.Fatalf("owner lookup returned stale config %s != %s", byOwner.BotUUID, rebound.BotUUID)
	}

	// 新渠道凭据: feishu + qq 同 owner 并存。
	feishu, err := store.UpsertChannelConfigCredentials(ctx, dev.UserID, domain.ChannelFeishu, []byte("ct-feishu"), 1)
	if err != nil {
		t.Fatalf("upsert feishu: %v", err)
	}
	qq, err := store.UpsertChannelConfigCredentials(ctx, dev.UserID, domain.ChannelQQ, []byte("ct-qq"), 1)
	if err != nil {
		t.Fatalf("upsert qq: %v", err)
	}
	if feishu.BotUUID == qq.BotUUID {
		t.Fatal("different channels must not share bot_uuid")
	}
	// 凭据更新: 同渠道 upsert 保留 UUID, 刷新密文。
	feishu2, err := store.UpsertChannelConfigCredentials(ctx, dev.UserID, domain.ChannelFeishu, []byte("ct-feishu-2"), 2)
	if err != nil {
		t.Fatalf("re-upsert feishu: %v", err)
	}
	if feishu2.BotUUID != feishu.BotUUID {
		t.Fatalf("credential update must keep bot_uuid: %s != %s", feishu2.BotUUID, feishu.BotUUID)
	}
	if string(feishu2.ConfigCiphertext) != "ct-feishu-2" || feishu2.ConfigKeyVersion != 2 {
		t.Fatalf("config ciphertext not refreshed: %s v%d", feishu2.ConfigCiphertext, feishu2.ConfigKeyVersion)
	}
	if !feishu2.IsBound() {
		t.Fatal("active feishu config must be bound")
	}

	// 列表: 每渠道一行。
	all, err := store.GetChannelConfigsByOwner(ctx, dev.UserID)
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	types := map[domain.ChannelType]int{}
	for _, c := range all {
		types[c.ChannelType]++
	}
	if types[domain.ChannelWechat] != 1 || types[domain.ChannelFeishu] != 1 || types[domain.ChannelQQ] != 1 {
		t.Fatalf("expected one row per channel, got %+v", types)
	}

	// 解绑: state=disabled; 重复解绑幂等; 未知渠道行报 ErrNoRows。
	if err := store.DisableChannelConfig(ctx, dev.UserID, domain.ChannelFeishu); err != nil {
		t.Fatalf("disable feishu: %v", err)
	}
	feishuDisabled, err := store.GetChannelConfigByOwnerAndType(ctx, dev.UserID, domain.ChannelFeishu)
	if err != nil {
		t.Fatalf("get disabled: %v", err)
	}
	if feishuDisabled.State != domain.ChannelDisabled {
		t.Fatalf("state = %s, want disabled", feishuDisabled.State)
	}
	if feishuDisabled.IsBound() {
		t.Fatal("disabled config must not be bound")
	}
	if err := store.DisableChannelConfig(ctx, dev.UserID, domain.ChannelFeishu); err != nil {
		t.Fatalf("disable twice must be idempotent: %v", err)
	}
	if err := store.DisableChannelConfig(ctx, dev.UserID, domain.ChannelDingTalk); err == nil {
		t.Fatal("disabling a missing channel must error")
	} else if !isNoRows(err) {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}

	// 活跃列表: wechat(已绑定) + qq(active); feishu(disabled) 不在。
	active, err := store.ListActiveChannelConfigs(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	activeUUIDs := map[string]bool{}
	for _, c := range active {
		activeUUIDs[c.BotUUID] = true
	}
	if !activeUUIDs[rebound.BotUUID] || !activeUUIDs[qq.BotUUID] || activeUUIDs[feishu.BotUUID] {
		t.Fatalf("active list mismatch: wechat=%v qq=%v feishu(disabled)=%v",
			activeUUIDs[rebound.BotUUID], activeUUIDs[qq.BotUUID], activeUUIDs[feishu.BotUUID])
	}

	// UUID 查找 + 状态迁移。
	got, err := store.GetChannelConfigByUUID(ctx, qq.BotUUID)
	if err != nil {
		t.Fatalf("get by uuid: %v", err)
	}
	if got.ChannelType != domain.ChannelQQ {
		t.Fatalf("uuid lookup type = %s", got.ChannelType)
	}
	if err := store.UpdateChannelConfigState(ctx, qq.BotUUID, domain.ChannelExpired); err != nil {
		t.Fatalf("update state: %v", err)
	}
	if got, err := store.GetChannelConfigByUUID(ctx, qq.BotUUID); err != nil || got.State != domain.ChannelExpired {
		t.Fatalf("state after update: %v state=%s", err, got.State)
	}
	// 非法 UUID 按不存在返回(不产生 22P02 永久错误)。
	if _, err := store.GetChannelConfigByUUID(ctx, "not-a-uuid"); err != pgx.ErrNoRows {
		t.Fatalf("invalid uuid must map to ErrNoRows, got %v", err)
	}
}

// TestMigration0053RenamePreservesForeignKeys 验证 bots → channel_configs
// 改名后 messages 外键仍指向新表(0036 channel_bindings 无关; 0036 是
// channel_bindings 表, 勿混淆)。
func TestMigration0053RenamePreservesForeignKeys(t *testing.T) {
	pool := requireDB(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dev := seedDev(t, store)

	cfg, err := store.CreateChannelConfig(ctx, "ilink-bot-x", dev.UserID, []byte("ct"))
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	// messages.bot_id 外键跟随 RENAME: 引用 channel_configs(id)。
	msg := domain.Message{
		UserID: dev.UserID, BotID: cfg.ID, SessionKey: dev.SessionKey,
		MessageID: "fk-follow-1", MessageType: domain.MessageTypeText, Content: "hi",
	}
	if _, err := store.InsertInboundMessage(ctx, msg); err != nil {
		t.Fatalf("insert message with renamed FK: %v", err)
	}
}

func isNoRows(err error) bool {
	return err == pgx.ErrNoRows
}

// TestMigration0053ReplayIdempotent 验证 0053 重放幂等: 应用后删除其 marker
// 再走 EnsureSchema(测试库重建场景), 必须成功且 0003 不被重放(stub bots
// 表仍无 bot_uuid 列——0003 的 marker 就是 bots 表本身)。
func TestMigration0053ReplayIdempotent(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()

	// OpenTestPool 已全量重建(含 0053)。删除 0053 marker 模拟"已应用但
	// marker 缺失"重放。
	if _, err := pool.Exec(ctx, `DROP TABLE migration_0053_channel_configs_marker`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool, ""); err != nil {
		t.Fatalf("replay after marker drop: %v", err)
	}
	// 0003 未被重放: bots 仍是 stub marker(无 bot_uuid 列)。
	var hasBotUUID bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM information_schema.columns WHERE table_name='bots' AND column_name='bot_uuid')`).Scan(&hasBotUUID); err != nil {
		t.Fatal(err)
	}
	if hasBotUUID {
		t.Fatal("0003 was replayed: bots has bot_uuid column; must remain stub marker")
	}
	// 渠道语义完好: state check + 唯一索引 + 新列都在。
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_constraint WHERE conname='channel_configs_state_check')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("channel_configs_state_check missing after replay")
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_indexes WHERE indexname='channel_configs_owner_type_uq')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("channel_configs_owner_type_uq missing after replay")
	}
	var hasChannelType bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM information_schema.columns WHERE table_name='channel_configs' AND column_name='channel_type')`).Scan(&hasChannelType); err != nil {
		t.Fatal(err)
	}
	if !hasChannelType {
		t.Fatal("channel_type column missing after replay")
	}
	// 0053 marker 已重建(后续 EnsureSchema 不再重放)。
	if err := EnsureSchema(ctx, pool, ""); err != nil {
		t.Fatalf("second replay: %v", err)
	}
}
