package httpserver

import (
	"errors"
	"net/http"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
)

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, auth.RoleOwner); !ok {
		return
	}
	users, err := s.auth.ListUsers(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireUser(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	username, usernameOK := body["username"].(string)
	password, passwordOK := body["password"].(string)
	role, _ := body["role"].(string)
	if !usernameOK || !passwordOK || (role != "owner" && role != "operator") {
		writeConfigError(w, &httpError{status: http.StatusBadRequest, message: "username, password, and role are required"})
		return
	}
	user, err := s.auth.CreateUser(r.Context(), actor, username, password, auth.Role(role))
	if err != nil {
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireUser(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	userID := r.PathValue("userId")
	body, ok := decodeJSONObject(w, r)
	if !ok {
		return
	}
	patch := auth.UserPatch{}
	if role, ok := body["role"].(string); ok && (role == "owner" || role == "operator") {
		parsed := auth.Role(role)
		patch.Role = &parsed
	}
	if active, ok := body["active"].(bool); ok {
		patch.Active = &active
	}
	if password, ok := body["password"].(string); ok {
		hash, err := auth.HashPassword(password)
		if err != nil {
			writeConfigError(w, &httpError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		patch.PasswordHash = &hash
	}
	user, err := s.auth.UpdateUser(r.Context(), actor, userID, patch)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeConfigError(w, &httpError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		writeConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireUser(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	userID := r.PathValue("userId")
	if err := s.auth.DeleteUser(r.Context(), actor, userID); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeConfigError(w, &httpError{status: http.StatusBadRequest, message: err.Error()})
			return
		}
		writeConfigError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
