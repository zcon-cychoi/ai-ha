package rest

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// Context keys carrying the authenticated caller. They hold identity only:
// whether that agent may act on a particular job is decided where the job is
// known, because holding a valid token must not by itself reach another
// agent's work.
const (
	ctxAgentID  = "aiha.agentId"
	ctxSystemID = "aiha.systemId"
)

// bearerPrefix is the scheme agents authenticate with. The token travels in a
// header and never in the URL: the request logger records the URI.
const bearerPrefix = "Bearer "

// agentAuth authenticates an agent by its bearer token.
//
// It separates a caller that must re-enrol from one that should simply try
// again. Reporting a database outage as a rejected token would tell every
// healthy agent in the fleet to discard its credentials.
func agentAuth(svc *agent.Service) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			token, ok := bearerToken(c.Request().Header.Get(echo.HeaderAuthorization))
			if !ok {
				return c.JSON(http.StatusUnauthorized,
					model.SimpleMsg{Message: "Agent token required"})
			}

			authenticated, err := svc.Authenticate(c.Request().Context(), token)
			if err != nil {
				return authFailure(c, err)
			}

			c.Set(ctxAgentID, authenticated.AgentID)
			c.Set(ctxSystemID, authenticated.SystemID)
			return next(c)
		}
	}
}

// authFailure answers a failed authentication, distinguishing a credential the
// caller must replace from a condition that will pass on its own.
func authFailure(c echo.Context, err error) error {
	switch {
	case errors.Is(err, agent.ErrAgentNotFound):
		return c.JSON(http.StatusUnauthorized,
			model.SimpleMsg{Message: "Agent token rejected"})

	case errors.Is(err, agent.ErrAgentRevoked):
		return c.JSON(http.StatusUnauthorized,
			model.SimpleMsg{Message: "Agent token revoked: register again"})

	default:
		// The token may well be valid; the framework cannot tell right now.
		// Saying so keeps a database outage from emptying the fleet's
		// credentials. The reason goes to the log, not to the caller.
		log.Error().Err(err).Msg("Could not authenticate an agent")
		c.Response().Header().Set(echo.HeaderRetryAfter, retryAfterSeconds)
		return c.JSON(http.StatusServiceUnavailable,
			model.SimpleMsg{Message: "Service temporarily unavailable, retry shortly"})
	}
}

// retryAfterSeconds is what an agent is asked to wait when the framework
// cannot answer. It is short: the agent is otherwise idle.
const retryAfterSeconds = "5"

// bearerToken extracts the token from an Authorization header.
func bearerToken(header string) (string, bool) {
	// The scheme is case-insensitive per RFC 7235, the token is not.
	if len(header) < len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// agentIDOf returns the authenticated agent's id. It is only called from
// handlers behind agentAuth, which always sets it.
func agentIDOf(c echo.Context) string {
	id, _ := c.Get(ctxAgentID).(string)
	return id
}

// systemIDOf returns the authenticated agent's system id.
func systemIDOf(c echo.Context) string {
	id, _ := c.Get(ctxSystemID).(string)
	return id
}
