package application

import (
	"context"
	"fmt"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

// PersonaStore is the persistence port for the persona store.
type PersonaStore interface {
	CreatePersona(ctx context.Context, authorUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error)
	AdminCreatePersona(ctx context.Context, adminUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error)
	GetPersona(ctx context.Context, id string) (domain.Persona, error)
	ListPersonasVisibleTo(ctx context.Context, userID int64) ([]domain.Persona, error)
	ListPendingPublicPersonas(ctx context.Context) ([]domain.Persona, error)
	ListAllPersonas(ctx context.Context, status string) ([]domain.Persona, error)
	ListPersonasByAuthor(ctx context.Context, authorUserID int64) ([]domain.Persona, error)
	UpdatePersona(ctx context.Context, id string, authorUserID int64, name, description, systemPrompt string) (domain.Persona, error)
	AdminUpdatePersona(ctx context.Context, id string, adminUserID int64, name, description, systemPrompt string) (domain.Persona, error)
	DeletePersona(ctx context.Context, id string, authorUserID int64) error
	AdminDeletePersona(ctx context.Context, id string) error
	SubmitPersonaForReview(ctx context.Context, id string, authorUserID int64) error
	ModeratePersona(ctx context.Context, id string, adminUserID int64, status domain.PersonaStatus, note string) error
	SetUserDefaultPersona(ctx context.Context, userID int64, personaID *string) error
}

// PersonaService manages the public/private persona catalog.
type PersonaService interface {
	CreatePersona(ctx context.Context, authorUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error)
	AdminCreatePersona(ctx context.Context, adminUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error)
	GetPersona(ctx context.Context, id string, viewerUserID int64) (domain.Persona, error)
	ListForUser(ctx context.Context, userID int64) ([]domain.Persona, error)
	ListPendingReview(ctx context.Context) ([]domain.Persona, error)
	ListAll(ctx context.Context, status string) ([]domain.Persona, error)
	ListByAuthor(ctx context.Context, authorUserID int64) ([]domain.Persona, error)
	UpdatePersona(ctx context.Context, id string, authorUserID int64, name, description, systemPrompt string) (domain.Persona, error)
	AdminUpdatePersona(ctx context.Context, id string, adminUserID int64, name, description, systemPrompt string) (domain.Persona, error)
	DeletePersona(ctx context.Context, id string, authorUserID int64) error
	AdminDeletePersona(ctx context.Context, id string) error
	SubmitForReview(ctx context.Context, id string, authorUserID int64) error
	Moderate(ctx context.Context, id string, adminUserID int64, status domain.PersonaStatus, note string) error
	SetDefault(ctx context.Context, userID int64, personaID string) error
	ClearDefault(ctx context.Context, userID int64) error
}

type personaService struct {
	store PersonaStore
}

// NewPersonaService constructs the service.
func NewPersonaService(store PersonaStore) (PersonaService, error) {
	if store == nil {
		return nil, fmt.Errorf("persona store is required")
	}
	return &personaService{store: store}, nil
}

func (s *personaService) CreatePersona(ctx context.Context, authorUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error) {
	return s.store.CreatePersona(ctx, authorUserID, name, description, systemPrompt, isPublic)
}

func (s *personaService) AdminCreatePersona(ctx context.Context, adminUserID int64, name, description, systemPrompt string, isPublic bool) (domain.Persona, error) {
	return s.store.AdminCreatePersona(ctx, adminUserID, name, description, systemPrompt, isPublic)
}

func (s *personaService) GetPersona(ctx context.Context, id string, viewerUserID int64) (domain.Persona, error) {
	p, err := s.store.GetPersona(ctx, id)
	if err != nil {
		return domain.Persona{}, err
	}
	if !p.IsVisibleTo(viewerUserID) {
		return domain.Persona{}, fmt.Errorf("persona not found or not visible")
	}
	return p, nil
}

func (s *personaService) ListForUser(ctx context.Context, userID int64) ([]domain.Persona, error) {
	return s.store.ListPersonasVisibleTo(ctx, userID)
}

func (s *personaService) ListPendingReview(ctx context.Context) ([]domain.Persona, error) {
	return s.store.ListPendingPublicPersonas(ctx)
}

func (s *personaService) ListAll(ctx context.Context, status string) ([]domain.Persona, error) {
	return s.store.ListAllPersonas(ctx, status)
}

func (s *personaService) ListByAuthor(ctx context.Context, authorUserID int64) ([]domain.Persona, error) {
	return s.store.ListPersonasByAuthor(ctx, authorUserID)
}

func (s *personaService) UpdatePersona(ctx context.Context, id string, authorUserID int64, name, description, systemPrompt string) (domain.Persona, error) {
	p, err := s.store.GetPersona(ctx, id)
	if err != nil {
		return domain.Persona{}, err
	}
	if !p.IsEditableBy(authorUserID) {
		return domain.Persona{}, fmt.Errorf("persona not found or not editable")
	}
	return s.store.UpdatePersona(ctx, id, authorUserID, name, description, systemPrompt)
}

func (s *personaService) AdminUpdatePersona(ctx context.Context, id string, adminUserID int64, name, description, systemPrompt string) (domain.Persona, error) {
	return s.store.AdminUpdatePersona(ctx, id, adminUserID, name, description, systemPrompt)
}

func (s *personaService) DeletePersona(ctx context.Context, id string, authorUserID int64) error {
	return s.store.DeletePersona(ctx, id, authorUserID)
}

func (s *personaService) AdminDeletePersona(ctx context.Context, id string) error {
	return s.store.AdminDeletePersona(ctx, id)
}

func (s *personaService) SubmitForReview(ctx context.Context, id string, authorUserID int64) error {
	return s.store.SubmitPersonaForReview(ctx, id, authorUserID)
}

func (s *personaService) Moderate(ctx context.Context, id string, adminUserID int64, status domain.PersonaStatus, note string) error {
	return s.store.ModeratePersona(ctx, id, adminUserID, status, note)
}

func (s *personaService) SetDefault(ctx context.Context, userID int64, personaID string) error {
	if personaID == "" {
		return fmt.Errorf("persona id is required")
	}
	// A user may only default to a persona they can actually see: their own, or
	// an approved public one. This prevents pinning another user's private
	// persona (or a taken-down one) by guessing its id.
	p, err := s.store.GetPersona(ctx, personaID)
	if err != nil {
		return fmt.Errorf("persona not found")
	}
	if !p.IsVisibleTo(userID) {
		return fmt.Errorf("persona not found or not visible")
	}
	return s.store.SetUserDefaultPersona(ctx, userID, &personaID)
}

func (s *personaService) ClearDefault(ctx context.Context, userID int64) error {
	return s.store.SetUserDefaultPersona(ctx, userID, nil)
}
