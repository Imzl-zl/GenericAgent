import { api, userApi } from './client';
import type { Persona } from './types';

interface PersonaListResponse {
  personas: Persona[];
}

interface CreatePersonaRequest {
  name: string;
  description: string;
  system_prompt: string;
  is_public: boolean;
}

interface SetDefaultPersonaRequest {
  persona_id: string;
}

interface ModeratePersonaRequest {
  note: string;
}

export async function listPersonas(): Promise<Persona[]> {
  const res = await userApi.get<PersonaListResponse>('/v1/personas');
  return res.personas;
}

export async function createPersona(
  name: string,
  description: string,
  systemPrompt: string,
  isPublic: boolean
): Promise<Persona> {
  return userApi.post<Persona>('/v1/personas', {
    name,
    description,
    system_prompt: systemPrompt,
    is_public: isPublic,
  } as CreatePersonaRequest);
}

export async function updatePersona(
  id: string,
  name: string,
  description: string,
  systemPrompt: string
): Promise<Persona> {
  return userApi.put<Persona>(`/v1/personas/${id}`, {
    name,
    description,
    system_prompt: systemPrompt,
  });
}

export async function deletePersona(id: string): Promise<{ deleted: boolean }> {
  return userApi.delete<{ deleted: boolean }>(`/v1/personas/${id}`);
}

export async function submitPersona(id: string): Promise<{ submitted: boolean }> {
  return userApi.post<{ submitted: boolean }>(`/v1/personas/${id}/submit`);
}

export async function setDefaultPersona(id: string): Promise<{ default_persona_id: string }> {
  return userApi.post<{ default_persona_id: string }>('/v1/users/me/default-persona', {
    persona_id: id,
  } as SetDefaultPersonaRequest);
}

export async function clearDefaultPersona(): Promise<{ default_persona_id: string }> {
  return userApi.post<{ default_persona_id: string }>('/v1/users/me/default-persona', {
    persona_id: '',
  } as SetDefaultPersonaRequest);
}

export async function listPendingPersonas(): Promise<Persona[]> {
  const res = await api.get<PersonaListResponse>('/v1/admin/personas/pending');
  return res.personas;
}

export async function adminListPersonas(status?: string): Promise<Persona[]> {
  const query = status ? `?status=${encodeURIComponent(status)}` : '';
  const res = await api.get<PersonaListResponse>(`/v1/admin/personas${query}`);
  return res.personas;
}

// adminListMyPersonas returns only personas authored by the admin (dev user),
// including private/pending/rejected ones — used by the "我的人设" view.
export async function adminListMyPersonas(): Promise<Persona[]> {
  const res = await api.get<PersonaListResponse>('/v1/admin/personas?mine=true');
  return res.personas;
}

export async function adminCreatePersona(
  name: string,
  description: string,
  systemPrompt: string,
  isPublic: boolean
): Promise<Persona> {
  return api.post<Persona>('/v1/admin/personas', {
    name,
    description,
    system_prompt: systemPrompt,
    is_public: isPublic,
  } as CreatePersonaRequest);
}

export async function adminUpdatePersona(
  id: string,
  name: string,
  description: string,
  systemPrompt: string
): Promise<Persona> {
  return api.put<Persona>(`/v1/admin/personas/${id}`, {
    name,
    description,
    system_prompt: systemPrompt,
  });
}

export async function adminDeletePersona(id: string): Promise<{ deleted: boolean }> {
  return api.delete<{ deleted: boolean }>(`/v1/admin/personas/${id}`);
}

export async function adminSetDefaultPersona(id: string): Promise<{ default_persona_id: string }> {
  return api.post<{ default_persona_id: string }>('/v1/admin/me/default-persona', {
    persona_id: id,
  } as SetDefaultPersonaRequest);
}

export async function adminClearDefaultPersona(): Promise<{ default_persona_id: string }> {
  return api.post<{ default_persona_id: string }>('/v1/admin/me/default-persona', {
    persona_id: '',
  } as SetDefaultPersonaRequest);
}

export async function approvePersona(id: string, note: string): Promise<{ id: string; status: string }> {
  return api.post<{ id: string; status: string }>(`/v1/admin/personas/${id}/approve`, { note } as ModeratePersonaRequest);
}

export async function rejectPersona(id: string, note: string): Promise<{ id: string; status: string }> {
  return api.post<{ id: string; status: string }>(`/v1/admin/personas/${id}/reject`, { note } as ModeratePersonaRequest);
}
