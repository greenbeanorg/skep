package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// SessionCookie is the name of the session cookie. The __Host- prefix makes
// browsers accept it only with Secure, Path=/, and no Domain, so it cannot
// be set or read by another host or over plain HTTP.
const SessionCookie = "__Host-skep_session"

// maxBodyBytes bounds the JSON body of a request to the API.
const maxBodyBytes = 64 << 10

// statusForAuthCode maps the error codes of internal/auth to statuses.
var statusForAuthCode = map[string]int{
	auth.CodeUnauthenticated:    http.StatusUnauthorized,
	auth.CodeInvalidCredentials: http.StatusUnauthorized,
	auth.CodeForbidden:          http.StatusForbidden,
	auth.CodeRateLimited:        http.StatusTooManyRequests,
	auth.CodeSetupRequired:      http.StatusServiceUnavailable,
	auth.CodeUsernameTaken:      http.StatusConflict,
	auth.CodeLastAdmin:          http.StatusConflict,
	auth.CodeUnknownRole:        http.StatusBadRequest,
}

func isUnauthenticated(err error) bool { return errors.Is(err, auth.ErrUnauthenticated) }

// fail maps an error to a response: authentication errors here, everything
// else through httpkit.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var limited *auth.RateLimitedError
	if errors.As(err, &limited) {
		seconds := int(limited.RetryAfter/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		httpkit.WriteError(w, r, http.StatusTooManyRequests, auth.CodeRateLimited,
			"too many login attempts; try again in "+strconv.Itoa(seconds)+" seconds")
		return
	}
	var me *models.Error
	if errors.As(err, &me) {
		if status, ok := statusForAuthCode[me.Code]; ok {
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="skep"`)
			}
			httpkit.WriteErrorDetails(w, r, status, me.Code, me.Message, me.Details)
			return
		}
	}
	httpkit.Fail(h.log, w, r, err)
}

// BackgroundHeader marks a request the client made by itself (a timer, a
// refetch on focus) rather than for something the user did. Such a request
// is authenticated like any other, the idle check included, but it does
// not count as use of the session, so an unattended page idles out (ADR
// 0008, decision 3). Any non-empty value marks the request.
const BackgroundHeader = "X-Skep-Background"

// identify returns the identity behind the credentials of a request. A
// token is accepted only through the channel it was issued for: an
// Authorization header is a bearer token, the session cookie is a cookie
// session, and a request that sends an Authorization header is judged by
// that alone.
func (h *handler) identify(r *http.Request) (auth.Identity, error) {
	authenticate := h.auth.Authenticate
	if r.Header.Get(BackgroundHeader) != "" {
		authenticate = h.auth.AuthenticateBackground
	}
	if authz := r.Header.Get("Authorization"); authz != "" {
		scheme, token, ok := strings.Cut(authz, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return auth.Identity{}, auth.ErrUnauthenticated
		}
		return authenticate(r.Context(), strings.TrimSpace(token), auth.BearerSession)
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		return authenticate(r.Context(), c.Value, auth.CookieSession)
	}
	return auth.Identity{}, auth.ErrUnauthenticated
}

// sameOrigin reports whether the request says it comes from this site: its
// Origin header must be present and name this host over HTTPS. Browsers
// set Origin on every request that changes state, and scripts cannot
// forge it.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && strings.EqualFold(u.Host, r.Host)
}

// ActionCSRF is the audit action of a request refused by the origin check.
const ActionCSRF = "auth.origin_check"

// denyOrigin refuses a request whose Origin header does not match this
// host, or a cookie-authenticated write without one. A refusal of a
// request with a valid session is audited under its user. One without a
// session is only logged: like other unauthenticated rejections, it has
// no actor, and auditing it would let any web page fill the audit log.
func (h *handler) denyOrigin(w http.ResponseWriter, r *http.Request, id *auth.Identity) {
	origin := r.Header.Get("Origin")
	if id != nil {
		h.writeAudit(r, audit.AccessEntry{
			ActorID: id.User.ID, ActorName: id.User.Username, Action: ActionCSRF, Outcome: audit.Denied,
			Details: map[string]any{"method": r.Method, "route": r.URL.Path, "origin_present": origin != ""},
		})
	} else {
		h.log.InfoContext(r.Context(), "refused a request from another origin",
			"method", r.Method, "route", r.URL.Path, "source_ip", sourceIPFrom(r.Context()))
	}
	message := "the Origin header does not match this host"
	if origin == "" {
		message = "a request that is authenticated by cookie and changes something must carry an Origin header that matches this host"
	}
	httpkit.WriteError(w, r, http.StatusForbidden, auth.CodeForbidden, message)
}

// writeAudit appends to the access audit log with the request's address and
// ID filled in.
func (h *handler) writeAudit(r *http.Request, e audit.AccessEntry) {
	e.SourceIP, e.RequestID = sourceIPFrom(r.Context()), httpkit.RequestID(r.Context())
	if err := h.audit.Write(r.Context(), e); err != nil {
		h.log.ErrorContext(r.Context(), "could not write to the access audit log", "error", err,
			"action", e.Action, "actor", e.ActorName, "outcome", string(e.Outcome))
	}
}

// decodeBody reads a JSON request body into v. It insists on the JSON
// content type: a cross-site form cannot send it without a preflight.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httpkit.WriteError(w, r, http.StatusUnsupportedMediaType, httpkit.CodeInvalidRequest, "the Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "the request body is not the expected JSON object")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "unexpected data after the JSON object")
		return false
	}
	return true
}

func publicUser(u auth.User) models.User {
	return models.User{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role.Name,
		Disabled: u.Disabled, LastLoginAt: u.LastLoginAt, PasswordChangedAt: u.PasswordChangedAt,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func (h *handler) sessionResponse(u auth.User, s auth.Session) models.SessionResponse {
	perms := u.Role.Permissions
	if perms == nil {
		perms = []string{}
	}
	return models.SessionResponse{
		User: publicUser(u),
		Session: models.SessionInfo{
			Kind: string(s.Kind), CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt,
			IdleTimeoutSeconds: int64(h.auth.SessionLifetime().IdleTimeout / time.Second),
		},
		Permissions: perms,
	}
}

// setSessionCookie hands a cookie session's token to the browser. HttpOnly
// keeps it from JavaScript, Secure from plain HTTP, SameSite=Strict from
// requests that other sites make.
func setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/",
		Expires: expires, MaxAge: int(time.Until(expires) / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// login starts a session. The client chooses one kind: a cookie session
// (the default), whose token is set in the cookie and is not in the body,
// or a bearer session, whose token is in the body and sets no cookie.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var req models.LoginRequest
	if !decodeBody(w, r, &req) {
		return
	}
	kind := auth.CookieSession
	switch req.Session {
	case "", models.SessionCookie:
	case models.SessionBearer:
		kind = auth.BearerSession
	default:
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, `"session" must be "cookie" or "bearer"`)
		return
	}
	res, err := h.auth.Login(r.Context(), auth.LoginRequest{
		Username: req.Username, Password: req.Password, Kind: kind,
		SourceIP: sourceIPFrom(r.Context()), UserAgent: r.UserAgent(), RequestID: httpkit.RequestID(r.Context()),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body := h.sessionResponse(res.User, res.Session)
	if kind == auth.BearerSession {
		body.Token = res.Token
	} else {
		setSessionCookie(w, res.Token, res.Session.ExpiresAt)
	}
	httpkit.WriteJSON(w, r, http.StatusOK, body)
}

// logout ends the session the request came in on.
func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFrom(r.Context())
	if err := h.auth.Logout(r.Context(), id, sourceIPFrom(r.Context()), httpkit.RequestID(r.Context())); err != nil {
		h.fail(w, r, err)
		return
	}
	if id.Session.Kind == auth.CookieSession {
		clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// session describes the caller and the session in use.
func (h *handler) session(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFrom(r.Context())
	httpkit.WriteJSON(w, r, http.StatusOK, h.sessionResponse(id.User, id.Session))
}

// changePassword replaces the caller's own password. It needs the current
// one, and it ends every session of the user, this one included.
func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFrom(r.Context())
	var req models.PasswordChange
	if !decodeBody(w, r, &req) {
		return
	}
	ip, requestID := sourceIPFrom(r.Context()), httpkit.RequestID(r.Context())
	// Check the new password first: a refusal for its length should not
	// cost a hash or count as a failed attempt.
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.auth.VerifyPassword(r.Context(), id, req.CurrentPassword, ip, requestID); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.users.SetPassword(r.Context(), id.Actor(ip, requestID), id.User.ID, req.NewPassword); err != nil {
		h.fail(w, r, err)
		return
	}
	if id.Session.Kind == auth.CookieSession {
		clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// forwardedFor splits X-Forwarded-For header values into addresses.
func forwardedFor(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
