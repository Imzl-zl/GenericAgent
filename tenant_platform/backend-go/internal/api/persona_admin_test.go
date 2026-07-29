package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/application"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/policy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
)

// personaAdminFixture builds a server wired with a real persona service backed
// by the test Postgres pool. DevUserID 9 stands in for the admin identity used
// by the dev-token (s.auth) admin endpoints.
func personaAdminFixture(t *testing.T) (*Server, *postgres.Store) {
	t.Helper()
	pool := postgres.OpenTestPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.EnsureDevelopmentContext(context.Background(), 9, "persona-admin-dev")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a second user (id 42) so tests can author personas under a
	// non-admin author and verify the ?mine=true filter.
	if _, err := store.EnsureDevelopmentContext(context.Background(), 42, "persona-other-user"); err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0)
	polPath := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "policy", "foundation.v1.json"))
	reg, err := policy.LoadRegistry(polPath)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := application.NewTaskService(application.TaskServiceConfig{
		Store: store, Registry: reg, PlatformInstanceID: "persona-admin-inst", ClaimLease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	personas, err := application.NewPersonaService(store)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(ServerConfig{
		Service:    svc,
		Registry:   reg,
		Personas:   personas,
		DevToken:   "test-dev-token",
		DevUserID:  9,
		SessionKey: dev.SessionKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, store
}

func adminReq(t *testing.T, srv *Server, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platform-Dev-Token", "test-dev-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

// TestAdminPersonaLifecycle exercises the admin public-pool management surface:
// create (public → auto-approved), list, update (status preserved), moderate
// takedown/relist, and delete.
func TestAdminPersonaLifecycle(t *testing.T) {
	srv, _ := personaAdminFixture(t)

	// 1. Admin creates a public persona → should be published straight to the pool.
	rr, body := adminReq(t, srv, http.MethodPost, "/v1/admin/personas", map[string]any{
		"name":          "Admin Persona",
		"description":   "created by admin",
		"system_prompt": "you are helpful",
		"is_public":     true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", rr.Code, body)
	}
	if body["status"] != "approved" {
		t.Fatalf("admin public persona should be approved, got %v", body["status"])
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("missing persona id")
	}

	// 2. List all personas → the new persona is present.
	rr, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%v", rr.Code, body)
	}
	list, _ := body["personas"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected 1 persona, got %d", len(list))
	}

	// 3. Filter by approved status.
	rr, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas?status=approved", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list approved status=%d body=%v", rr.Code, body)
	}
	if list, _ = body["personas"].([]any); len(list) != 1 {
		t.Fatalf("expected 1 approved persona, got %d", len(list))
	}

	// 4. Invalid status filter is rejected.
	rr, _ = adminReq(t, srv, http.MethodGet, "/v1/admin/personas?status=bogus", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter should 400, got %d", rr.Code)
	}

	// 5. Admin edits the approved persona → status preserved (still approved).
	rr, body = adminReq(t, srv, http.MethodPut, "/v1/admin/personas/"+id, map[string]any{
		"name":          "Admin Persona v2",
		"description":   "edited",
		"system_prompt": "you are very helpful",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%v", rr.Code, body)
	}
	if body["status"] != "approved" {
		t.Fatalf("admin edit should preserve approved, got %v", body["status"])
	}
	if body["name"] != "Admin Persona v2" {
		t.Fatalf("name not updated: %v", body["name"])
	}

	// 6. Moderate takedown (reject an approved persona).
	rr, body = adminReq(t, srv, http.MethodPost, "/v1/admin/personas/"+id+"/reject", map[string]any{"note": "taken down"})
	if rr.Code != http.StatusOK {
		t.Fatalf("takedown status=%d body=%v", rr.Code, body)
	}
	rr, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas?status=rejected", nil)
	if list, _ = body["personas"].([]any); len(list) != 1 {
		t.Fatalf("expected 1 rejected persona after takedown, got %d", len(list))
	}

	// 7. Re-list (approve a rejected persona).
	rr, body = adminReq(t, srv, http.MethodPost, "/v1/admin/personas/"+id+"/approve", map[string]any{"note": "restored"})
	if rr.Code != http.StatusOK {
		t.Fatalf("relist status=%d body=%v", rr.Code, body)
	}
	rr, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas?status=approved", nil)
	if list, _ = body["personas"].([]any); len(list) != 1 {
		t.Fatalf("expected 1 approved persona after relist, got %d", len(list))
	}

	// 8. Admin sets it as their own default, then clears it.
	rr, body = adminReq(t, srv, http.MethodPost, "/v1/admin/me/default-persona", map[string]any{"persona_id": id})
	if rr.Code != http.StatusOK {
		t.Fatalf("set default status=%d body=%v", rr.Code, body)
	}
	rr, body = adminReq(t, srv, http.MethodPost, "/v1/admin/me/default-persona", map[string]any{"persona_id": ""})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear default status=%d body=%v", rr.Code, body)
	}

	// 9. Admin deletes the persona.
	rr, body = adminReq(t, srv, http.MethodDelete, "/v1/admin/personas/"+id, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%v", rr.Code, body)
	}
	rr, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas", nil)
	if list, _ = body["personas"].([]any); len(list) != 0 {
		t.Fatalf("expected 0 personas after delete, got %d", len(list))
	}
}

// TestAdminCreatePrivatePersonaStaysPrivate verifies a non-public admin
// persona is not auto-published.
func TestAdminCreatePrivatePersonaStaysPrivate(t *testing.T) {
	srv, _ := personaAdminFixture(t)
	rr, body := adminReq(t, srv, http.MethodPost, "/v1/admin/personas", map[string]any{
		"name":          "Private Admin Persona",
		"system_prompt": "secret",
		"is_public":     false,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", rr.Code, body)
	}
	if body["status"] != "private" {
		t.Fatalf("private admin persona should stay private, got %v", body["status"])
	}
}

// TestAdminListMineFilter verifies ?mine=true returns only personas authored
// by the admin (dev user id), not personas created by other users.
func TestAdminListMineFilter(t *testing.T) {
	srv, _ := personaAdminFixture(t)

	// Another user (id 42) authors a persona directly via the store-backed
	// service so it exists in the pool but is NOT authored by the admin.
	if _, err := srv.personas.CreatePersona(context.Background(), 42, "Other User Persona", "", "prompt", false); err != nil {
		t.Fatalf("seed other-user persona: %v", err)
	}

	// Admin authors one of their own.
	rr, body := adminReq(t, srv, http.MethodPost, "/v1/admin/personas", map[string]any{
		"name":          "Admin Own",
		"system_prompt": "mine",
		"is_public":     false,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", rr.Code, body)
	}

	// Full list should show both personas.
	_, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas", nil)
	if list, _ := body["personas"].([]any); len(list) != 2 {
		t.Fatalf("expected 2 personas in full list, got %d", len(list))
	}

	// mine=true should show only the admin's own persona.
	_, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas?mine=true", nil)
	list, _ := body["personas"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected 1 persona in mine list, got %d", len(list))
	}
	p, _ := list[0].(map[string]any)
	if p["name"] != "Admin Own" {
		t.Fatalf("mine list returned wrong persona: %v", p["name"])
	}
}

// TestAdminPersonaValidation rejects empty name/prompt.
func TestAdminPersonaValidation(t *testing.T) {
	srv, _ := personaAdminFixture(t)
	rr, _ := adminReq(t, srv, http.MethodPost, "/v1/admin/personas", map[string]any{
		"name":          "",
		"system_prompt": "x",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name should 400, got %d", rr.Code)
	}
}

// defaultPersonaID reads a user's default_persona_id, returning "" when NULL.
func defaultPersonaID(t *testing.T, store *postgres.Store, userID int64) string {
	t.Helper()
	var id *string
	err := store.Pool().QueryRow(context.Background(),
		`SELECT default_persona_id FROM users WHERE id = $1`, userID).Scan(&id)
	if err != nil {
		t.Fatalf("read default_persona_id: %v", err)
	}
	if id == nil {
		return ""
	}
	return *id
}

// TestModerateRejectClearsDanglingDefault verifies that taking down (rejecting)
// an approved persona also clears it from every user who had it as their
// default, so no bot keeps applying a de-listed prompt.
func TestModerateRejectClearsDanglingDefault(t *testing.T) {
	srv, store := personaAdminFixture(t)

	// Admin publishes a public persona (approved) and pins it as their default.
	rr, body := adminReq(t, srv, http.MethodPost, "/v1/admin/personas", map[string]any{
		"name":          "Pinned",
		"system_prompt": "prompt",
		"is_public":     true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", rr.Code, body)
	}
	id, _ := body["id"].(string)
	rr, body = adminReq(t, srv, http.MethodPost, "/v1/admin/me/default-persona", map[string]any{"persona_id": id})
	if rr.Code != http.StatusOK {
		t.Fatalf("set default status=%d body=%v", rr.Code, body)
	}
	if got := defaultPersonaID(t, store, 9); got != id {
		t.Fatalf("default should be %s, got %q", id, got)
	}

	// Take it down (reject). The dangling default must be cleared.
	rr, body = adminReq(t, srv, http.MethodPost, "/v1/admin/personas/"+id+"/reject", map[string]any{"note": "down"})
	if rr.Code != http.StatusOK {
		t.Fatalf("reject status=%d body=%v", rr.Code, body)
	}
	if got := defaultPersonaID(t, store, 9); got != "" {
		t.Fatalf("default should be cleared after takedown, got %q", got)
	}
}

// TestSetDefaultRejectsInvisiblePersona verifies a user cannot pin another
// user's private persona as their default by guessing its id.
func TestSetDefaultRejectsInvisiblePersona(t *testing.T) {
	srv, _ := personaAdminFixture(t)

	// User 42 authors a private persona (not visible to the admin, user 9).
	other, err := srv.personas.CreatePersona(context.Background(), 42, "Secret", "", "secret prompt", false)
	if err != nil {
		t.Fatalf("seed private persona: %v", err)
	}

	// Admin (user 9) tries to pin it → must be rejected.
	rr, body := adminReq(t, srv, http.MethodPost, "/v1/admin/me/default-persona", map[string]any{"persona_id": other.ID})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("pinning invisible persona should 400, got %d body=%v", rr.Code, body)
	}
}

// TestAdminCreatePublicPersonaIsAtomic verifies an admin-authored public
// persona lands as approved+public with admin_id set in a single step (no
// pending orphan).
func TestAdminCreatePublicPersonaIsAtomic(t *testing.T) {
	srv, _ := personaAdminFixture(t)
	rr, body := adminReq(t, srv, http.MethodPost, "/v1/admin/personas", map[string]any{
		"name":          "Atomic",
		"system_prompt": "prompt",
		"is_public":     true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", rr.Code, body)
	}
	if body["status"] != "approved" {
		t.Fatalf("public admin persona should be approved, got %v", body["status"])
	}
	if body["is_public"] != true {
		t.Fatalf("public admin persona should have is_public=true, got %v", body["is_public"])
	}
	// No pending rows should exist anywhere.
	_, body = adminReq(t, srv, http.MethodGet, "/v1/admin/personas?status=pending", nil)
	if list, _ := body["personas"].([]any); len(list) != 0 {
		t.Fatalf("expected 0 pending personas, got %d", len(list))
	}
}
