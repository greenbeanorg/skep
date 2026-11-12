package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// actor returns the authenticated caller as the actor of an audited
// change.
func actor(r *http.Request) auth.Actor {
	id, _ := identityFrom(r.Context())
	return id.Actor(sourceIPFrom(r.Context()), httpkit.RequestID(r.Context()))
}

// roleID resolves a role given by name or ID. Roles are few; listing them
// is cheap.
func (h *handler) roleID(r *http.Request, name string) (string, error) {
	roles, err := h.users.Roles(r.Context())
	if err != nil {
		return "", err
	}
	for _, role := range roles {
		if role.Name == name || role.ID == name {
			return role.ID, nil
		}
	}
	return "", auth.ErrUnknownRole
}

func userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid user ID")
		return "", false
	}
	return id, true
}

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list := models.UserList{Items: make([]models.User, 0, len(users))}
	for _, u := range users {
		list.Items = append(list.Items, publicUser(u))
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := userID(w, r)
	if !ok {
		return
	}
	u, err := h.users.ByID(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, publicUser(u))
}

func (h *handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req models.NewUser
	if !decodeBody(w, r, &req) {
		return
	}
	in := auth.NewUser{Username: req.Username, DisplayName: req.DisplayName, Password: req.Password}
	if req.Role != "" {
		id, err := h.roleID(r, req.Role)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		in.RoleID = id
	}
	u, err := h.users.Create(r.Context(), actor(r), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/users/"+u.ID)
	httpkit.WriteJSON(w, r, http.StatusCreated, publicUser(u))
}

func (h *handler) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := userID(w, r)
	if !ok {
		return
	}
	var req models.UserUpdate
	if !decodeBody(w, r, &req) {
		return
	}
	// Validate everything before changing anything.
	if req.Password != nil {
		if err := auth.ValidatePassword(*req.Password); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	upd := auth.UserUpdate{DisplayName: req.DisplayName, Disabled: req.Disabled}
	if req.Role != nil {
		rid, err := h.roleID(r, *req.Role)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		upd.RoleID = &rid
	}
	u, err := h.users.Update(r.Context(), actor(r), id, upd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Password != nil {
		if err := h.users.SetPassword(r.Context(), actor(r), id, *req.Password); err != nil {
			h.fail(w, r, err)
			return
		}
		if u, err = h.users.ByID(r.Context(), id); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	httpkit.WriteJSON(w, r, http.StatusOK, publicUser(u))
}

func (h *handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := userID(w, r)
	if !ok {
		return
	}
	if err := h.users.Delete(r.Context(), actor(r), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.users.Roles(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list := models.RoleList{Items: make([]models.Role, 0, len(roles))}
	for _, role := range roles {
		perms := role.Permissions
		if perms == nil {
			perms = []string{}
		}
		list.Items = append(list.Items, models.Role{
			ID: role.ID, Name: role.Name, Description: role.Description, Permissions: perms, BuiltIn: role.BuiltIn,
		})
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

// listAudit returns a page of the access audit log, newest first.
func (h *handler) listAudit(w http.ResponseWriter, r *http.Request) {
	if h.history == nil {
		h.fail(w, r, errors.New("api: no audit reader is configured"))
		return
	}
	q := r.URL.Query()
	var limit int
	var before int64
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if raw := q.Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid cursor")
			return
		}
		before = n
	}
	entries, next, err := h.history.List(r.Context(), limit, before)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list := models.AuditList{Items: entries}
	if next > 0 {
		list.NextCursor = strconv.FormatInt(next, 10)
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}
