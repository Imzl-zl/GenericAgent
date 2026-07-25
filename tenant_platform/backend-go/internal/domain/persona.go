// Package domain defines persona store types.
package domain

import "time"

// PersonaStatus is the moderation lifecycle of a public persona.
type PersonaStatus string

const (
	PersonaPrivate  PersonaStatus = "private"
	PersonaPending  PersonaStatus = "pending"
	PersonaApproved PersonaStatus = "approved"
	PersonaRejected PersonaStatus = "rejected"
)

// IsValidPersonaStatus reports whether s is an allowed persona status.
func IsValidPersonaStatus(s PersonaStatus) bool {
	switch s {
	case PersonaPrivate, PersonaPending, PersonaApproved, PersonaRejected:
		return true
	}
	return false
}

// Persona is a reusable role configuration that can be applied to a user's bot.
type Persona struct {
	ID           string
	AuthorUserID int64
	Name         string
	Description  string
	SystemPrompt string
	IsPublic     bool
	Status       PersonaStatus
	AdminID      *int64
	AdminNote    *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IsVisibleTo reports whether the persona can be seen by the given user.
// Authors always see their own personas; approved public personas are visible to everyone.
func (p Persona) IsVisibleTo(userID int64) bool {
	if p.AuthorUserID == userID {
		return true
	}
	return p.IsPublic && p.Status == PersonaApproved
}

// IsEditableBy reports whether the given user can edit or delete the persona.
func (p Persona) IsEditableBy(userID int64) bool {
	return p.AuthorUserID == userID
}
