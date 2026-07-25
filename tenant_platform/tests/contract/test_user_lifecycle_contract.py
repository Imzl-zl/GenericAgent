"""Contract-source tests for Slice 3a: user lifecycle + binding + router.

These tests read the contract sources (SQL migration, OpenAPI, Go domain types)
and assert they declare the tables, paths, and types required by architecture
spec §5–§6. They fail red until the sources are written.
"""
import re
from pathlib import Path

ROOT = Path(__file__).parents[2]

MIGRATION_PATH = ROOT / "infra/postgres/migrations/0003_user_lifecycle.sql"
OPENAPI_PATH = ROOT / "contracts/openapi/platform.yaml"
DOMAIN_BINDING_PATH = ROOT / "backend-go/internal/domain/binding.go"
TRANSPORT_ADAPTER_PATH = ROOT / "backend-go/internal/transport/adapter.go"


def test_migration_creates_user_lifecycle_tables():
    """Migration 0003 must create the 5 tables from spec §5."""
    text = MIGRATION_PATH.read_text(encoding="utf-8")
    for table in ("binding_attempts", "bots", "bot_transport_state", "context_tokens", "audit_events"):
        assert f"CREATE TABLE {table}" in text, f"migration missing CREATE TABLE {table}"


def test_migration_binding_attempts_stores_code_hash_not_plaintext():
    """Binding code must be stored as a hash, never plaintext (spec §5)."""
    text = MIGRATION_PATH.read_text(encoding="utf-8")
    assert "code_hash" in text, "binding_attempts must have code_hash column"
    # Reject a plaintext code column.
    assert "    code " not in text and "code TEXT" not in text, \
        "binding_attempts must not store plaintext code"


def test_migration_bots_owner_unique_and_ilink_bindable():
    """bots.owner_id is unique; ilink_user_id is bound after activation (spec §5)."""
    text = MIGRATION_PATH.read_text(encoding="utf-8")
    assert "owner_id" in text, "bots must have owner_id"
    assert "bot_uuid" in text, "bots must have bot_uuid"
    assert "ilink_user_id" in text, "bots must have ilink_user_id"
    assert "UNIQUE" in text, "bots must enforce uniqueness"


def test_migration_context_tokens_unique_per_bot_and_user():
    """context_tokens (bot_id, ilink_user_id) is unique (spec §5)."""
    text = MIGRATION_PATH.read_text(encoding="utf-8")
    assert "bot_id" in text, "context_tokens must reference bot_id"
    assert "ilink_user_id" in text, "context_tokens must reference ilink_user_id"


def test_migration_audit_events_covers_lifecycle_actions():
    """audit_events must record lifecycle actions (spec §5)."""
    text = MIGRATION_PATH.read_text(encoding="utf-8")
    assert "actor_user_id" in text or "actor" in text, "audit_events must have actor"
    assert "action" in text or "event_type" in text, "audit_events must have action/event_type"


def test_openapi_exposes_admin_user_management_paths():
    """OpenAPI must declare admin user management paths."""
    text = OPENAPI_PATH.read_text(encoding="utf-8")
    for path in ("/v1/admin/users", "/v1/admin/users/{user_id}/approve", "/v1/admin/users/{user_id}/block"):
        assert path in text, f"OpenAPI missing path {path}"


def test_openapi_exposes_binding_paths():
    """OpenAPI must declare binding generation and activation paths."""
    text = OPENAPI_PATH.read_text(encoding="utf-8")
    assert "/v1/bindings" in text, "OpenAPI missing /v1/bindings"
    assert "/v1/activate" in text, "OpenAPI missing /v1/activate"


def test_openapi_declares_router_message_path():
    """OpenAPI must declare the router message ingestion path."""
    text = OPENAPI_PATH.read_text(encoding="utf-8")
    assert "/v1/router/messages" in text, "OpenAPI missing /v1/router/messages"


def test_domain_binding_go_declares_lifecycle_types():
    """domain/binding.go must declare the 5 lifecycle types."""
    text = DOMAIN_BINDING_PATH.read_text(encoding="utf-8")
    for typename in ("BindingAttempt", "Bot", "BotTransportState", "ContextToken", "AuditEvent"):
        assert f"type {typename} struct" in text, f"domain missing type {typename}"


def test_domain_binding_states_enforce_lifecycle():
    """Binding states must follow spec §5.1: requested→awaiting_activation→active|expired|revoked."""
    text = DOMAIN_BINDING_PATH.read_text(encoding="utf-8")
    for state in ("Requested", "AwaitingActivation", "Active", "Expired", "Revoked"):
        assert state in text, f"domain missing binding state {state}"


def test_domain_user_status_constants():
    """domain must declare UserStatus constants pending/approved/blocked."""
    text = (ROOT / "backend-go/internal/domain/binding.go").read_text(encoding="utf-8")
    for status in ("Pending", "Approved", "Blocked"):
        assert status in text, f"domain missing user status {status}"


def test_transport_adapter_interface_exists():
    """transport/adapter.go must define the BotTransportAdapter interface."""
    text = TRANSPORT_ADAPTER_PATH.read_text(encoding="utf-8")
    assert "BotTransportAdapter" in text, "transport adapter interface missing"
    assert "interface" in text, "transport adapter must be an interface"


def test_transport_adapter_no_real_key_field():
    """Transport adapter must never carry a real API key field (spec §3.3)."""
    text = TRANSPORT_ADAPTER_PATH.read_text(encoding="utf-8")
    lowered = text.lower()
    for forbidden in ("api_key", "apikey", "secret_key", "bearer_token"):
        assert forbidden not in lowered, f"transport adapter must not declare {forbidden}"
