/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	servicejwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/tools"
)

// The chat surface, minus the streaming.
//
// Sessions, the exposure tier and the audit trail are ordinary request/response and
// stay REST: they carry a status code that means something and they are worth being
// able to curl. The streaming half — sending a message, resolving a confirmation,
// watching a turn — lives on the WebSocket in ws_chat.go, alongside the profiler
// operations, so there is one streaming mechanism rather than two.
//
// That is a deliberate departure from §5.7's "Streamed to the SPA over SSE". The
// reason is the one ws.go was built for: an exchange can run for minutes inside a
// single tool call, and the connection has to survive it. SSE could be kept alive
// with a heartbeat, but then ODE would maintain two streaming paths with two sets of
// liveness and cancellation semantics, and the WebSocket already had the harder half
// working.

// @Summary		Open a chat session
// @Description	An absent exposure_tier means L0, which §3.2 makes the default rather
// @Description	than a choice the caller has to remember. A tier above the admin
// @Description	ceiling for this user is refused, not silently clamped.
// @Tags			chat
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		object{title=string,provider=string,model=string,exposure_tier=string}	false	"all fields optional; an empty body opens a default session"
// @Success		201		{object}	chat.Session
// @Failure		400		{object}	map[string]string	"an unknown tier, or a malformed body"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"the provider, model or tier is not permitted for this user"
// @Failure		429		{object}	map[string]interface{}	"a limit from §3.3 was reached; the payload says which and when it resets"
// @Router			/chat/sessions [post]
func handleCreateChatSession(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Title    string `json:"title"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Tier     string `json:"exposure_tier"`
			// Which working context this conversation acts in. Absent means the
			// developer's only workbench; a second conversation naming the same one is
			// two chats about a single operator, and naming a different one is a
			// developer working on two at once.
			Workbench string `json:"workbench_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil && err.Error() != "EOF" {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Absent means L0, which §3.2 makes the default rather than a choice a
		// caller has to remember to make.
		tier := tools.DefaultTier
		if body.Tier != "" {
			parsed, err := tools.ParseTier(body.Tier)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			tier = parsed
		}

		token := auth.MustFromContext(c)
		session, err := engine.CreateSession(c.Request.Context(), token.Sub, chat.CreateRequest{
			Title: body.Title, Provider: body.Provider, Model: body.Model, Tier: tier,
			WorkbenchID: body.Workbench,
		})
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusCreated, session)
	}
}

// @Summary		The caller's chat sessions
// @Tags			chat
// @Produce		json
// @Security		Bearer
// @Param			limit	query		int	false	"maximum sessions; absent returns the store's default"
// @Success		200		{object}	map[string][]chat.Session
// @Failure		401		{object}	map[string]string
// @Router			/chat/sessions [get]
func handleListChatSessions(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 0
		if raw := c.Query("limit"); raw != "" {
			limit, _ = strconv.Atoi(raw)
		}
		token := auth.MustFromContext(c)
		sessions, err := engine.Sessions(c.Request.Context(), token.Sub, limit)
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"sessions": sessions})
	}
}

// @Summary		One session, its messages and anything awaiting confirmation
// @Tags			chat
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"session id"
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string	"no such session, or it belongs to another user"
// @Router			/chat/sessions/{id} [get]
func handleGetChatSession(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		session, err := engine.Session(c.Request.Context(), token.Sub, c.Param("id"))
		if err != nil {
			respondChatError(c, err)
			return
		}
		messages, err := engine.Messages(c.Request.Context(), token.Sub, session.ID)
		if err != nil {
			respondChatError(c, err)
			return
		}
		pending, err := engine.PendingConfirmations(c.Request.Context(), token.Sub, session.ID)
		if err != nil {
			respondChatError(c, err)
			return
		}
		described := make([]map[string]any, 0, len(pending))
		for _, confirmation := range pending {
			described = append(described, confirmation.Describe())
		}

		c.JSON(http.StatusOK, gin.H{
			"session":               session,
			"messages":              messages,
			"pending_confirmations": described,
		})
	}
}

// @Summary		Delete a session
// @Tags			chat
// @Produce		json
// @Security		Bearer
// @Param			id	path	string	true	"session id"
// @Success		204	"deleted"
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Router			/chat/sessions/{id} [delete]
func handleDeleteChatSession(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		if err := engine.DeleteSession(c.Request.Context(), token.Sub, c.Param("id")); err != nil {
			respondChatError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// @Summary		Rename a session
// @Description	Sets the developer's own name for a conversation, in place of the
// @Description	first few words of its opening message. An empty title clears the
// @Description	name, and the next message titles the session again.
// @Description
// @Description	Its own sub-resource rather than a PUT of the whole session, so that
// @Description	nothing changes a tier through a route that does not write §3.2's
// @Description	audit trail.
// @Tags			chat
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string				true	"session id"
// @Param			request	body		object{title=string}	true	"the new title; empty clears it"
// @Success		200		{object}	chat.Session
// @Failure		400		{object}	map[string]string	"a malformed body, or a title longer than a session keeps"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string
// @Router			/chat/sessions/{id}/title [put]
func handleRenameChatSession(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Title string `json:"title"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		token := auth.MustFromContext(c)
		session, err := engine.RenameSession(
			c.Request.Context(), token.Sub, c.Param("id"), body.Title)
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	}
}

// handleMoveChatSession re-points a conversation at another working context.
//
// Its own sub-resource for the reason the title and the tier are: a PUT of the whole
// session would be a second way to move a tier, and that one would not audit it.
//
// @Summary		Move a session to another workbench
// @Description	Changes the working context an existing conversation acts in: which
// @Description	checkout its file tools write into, and which kernel its cells run in.
// @Description	An empty workbench_id clears the assignment, which everything below
// @Description	reads as "my only workbench".
// @Description
// @Description	The move leaves a note in the conversation. Every file read, file write
// @Description	and cell run above it happened in the previous checkout, and a model
// @Description	handed that history with no marker goes on believing the files it wrote
// @Description	are still there.
// @Description
// @Description	Refused with 400 while a turn is running on the session: that turn is
// @Description	acting in the workbench the move would take it away from.
// @Tags			chat
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string							true	"session id"
// @Param			request	body		object{workbench_id=string}		true	"the workbench to act in; empty clears it"
// @Success		200		{object}	chat.Session
// @Failure		400		{object}	map[string]string	"a malformed body, or a turn is running on this session"
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string	"no such session, or no such workbench of this developer's"
// @Router			/chat/sessions/{id}/workbench [put]
func handleMoveChatSession(engine *chat.Engine, benches *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Workbench string `json:"workbench_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		token := auth.MustFromContext(c)

		// Checked, unlike the workbench a session is *created* with, which is taken as
		// sent — kernel.go says why a guessed one is harmless there. A move is a
		// repair: the developer doing it has noticed the conversation is in the wrong
		// place, and answering "moved" while pointing it at something that does not
		// exist would hand them a conversation whose tools act in no checkout at all.
		//
		// Checked here rather than in the engine because workbenches live in pkg/repo,
		// and a chat engine that had to know about them would couple the conversation
		// surface to the repository surface for one lookup. The label travels with it so
		// the note the move leaves names the workbench the way the tab bar does.
		var label string
		if strings.TrimSpace(body.Workbench) != "" {
			bench, err := benches.Workbench(c.Request.Context(), token.Sub, body.Workbench)
			if err != nil {
				respondRepo(c, err)
				return
			}
			label = bench.Label()
		}

		session, err := engine.MoveSession(
			c.Request.Context(), token.Sub, c.Param("id"), body.Workbench, label)
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	}
}

// handleSetTier is the developer's control from §3.2. There is deliberately no
// LLM tool for this (tools.Denied), so this route is the only way it changes.
//
// @Summary		Set a session's exposure tier
// @Description	The developer's control from §3.2. No LLM tool exists for this
// @Description	(tools.Denied), so this route is the only way a tier changes. Every
// @Description	change is written to the session's audit trail.
// @Tags			chat
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string					true	"session id"
// @Param			request	body		object{exposure_tier=string}	true	"the tier to move to, e.g. L1"
// @Success		200		{object}	chat.Session
// @Failure		400		{object}	map[string]string	"an unknown tier"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"above the admin ceiling for this user"
// @Failure		404		{object}	map[string]string
// @Router			/chat/sessions/{id}/tier [put]
func handleSetTier(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Tier string `json:"exposure_tier"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		tier, err := tools.ParseTier(body.Tier)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		token := auth.MustFromContext(c)
		session, err := engine.SetTier(c.Request.Context(), token.Sub, c.Param("id"), tier)
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	}
}

// handleSetAutoRun is the developer's standing answer to a `run_code`
// confirmation. Like the tier, there is deliberately no LLM tool for it
// (tools.Denied), so this route is the only way it changes.
//
// @Summary		Turn auto mode on or off for a session
// @Description	Auto mode runs a `run_code` call without asking when its code is
// @Description	recognised as an inspection of data already in the kernel — see
// @Description	pkg/plaincode for what that means and, importantly, what it does not.
// @Description	It is relief from repeated prompting, **not** a security boundary:
// @Description	the boundary is unchanged and is the developer's own pod and token.
// @Description	Anything unrecognised is still confirmed, and no other confirmed
// @Description	tool is affected. No LLM tool exists for this.
// @Tags			chat
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string				true	"session id"
// @Param			request	body		object{auto_run=bool}	true	"whether to run recognised code without asking"
// @Success		200		{object}	chat.Session
// @Failure		400		{object}	map[string]string
// @Failure		401		{object}	map[string]string
// @Failure		404		{object}	map[string]string
// @Router			/chat/sessions/{id}/auto-run [put]
func handleSetAutoRun(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			AutoRun *bool `json:"auto_run"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// A pointer, so an absent field is refused rather than read as "off". The
		// two are different requests and only one of them was made.
		if body.AutoRun == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "auto_run is required"})
			return
		}

		token := auth.MustFromContext(c)
		session, err := engine.SetAutoRun(c.Request.Context(), token.Sub, c.Param("id"), *body.AutoRun)
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	}
}

// @Summary		A session's exposure-tier history
// @Description	§3.2 requires every tier change to be logged. This is that record.
// @Tags			chat
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"session id"
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Router			/chat/sessions/{id}/tier-changes [get]
func handleTierAudit(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		changes, err := engine.TierChanges(c.Request.Context(), token.Sub, c.Param("id"))
		if err != nil {
			respondChatError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"changes": changes})
	}
}

// handleListTools publishes §5.8's table.
//
// Served to the SPA and to anyone auditing the surface: every declared tool with its
// minimum tier and confirmation requirement, which of them this deployment can
// actually run, and the list of capabilities that deliberately have no tool.
//
// @Summary		The tool surface of §5.8
// @Description	Every declared tool with its minimum tier and confirmation
// @Description	requirement, which of them this deployment can actually run, what each
// @Description	tier exposes, and the capabilities that deliberately have no tool.
// @Description	Readable by any developer: knowing what the assistant may do is not
// @Description	privileged information.
// @Tags			chat
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Router			/llm/tools [get]
func handleListTools(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		registry := engine.Registry()

		declared := registry.Definitions()
		published := make([]map[string]any, 0, len(declared))
		for _, definition := range declared {
			published = append(published, map[string]any{
				"name":        definition.Name,
				"description": definition.Description,
				"effect":      definition.Effect,
				"min_tier":    definition.MinTier,
				"confirm":     definition.Confirm,
				"implemented": definition.Implemented(),
				"unavailable": definition.Unavailable,
			})
		}

		tiers := make([]map[string]any, 0, len(tools.Tiers()))
		for _, tier := range tools.Tiers() {
			tiers = append(tiers, map[string]any{
				"tier":      tier,
				"exposes":   tier.Exposes(),
				"available": names(registry.Available(tier)),
			})
		}

		c.JSON(http.StatusOK, gin.H{
			"tools": published,
			"tiers": tiers,
			// The capabilities §5.8 denies, with the reason each is a developer action.
			// Published because "no tool exists" is a design claim, and a reader should
			// be able to check it against the list above.
			"denied": tools.Denied(),
		})
	}
}

// handleListProviders serves the configured providers and their capabilities,
// including any that came up degraded (§5.7).
//
// @Summary		The configured LLM providers
// @Description	Each provider with its models and capabilities, including any that came
// @Description	up degraded (§5.7), plus which one is the default.
// @Tags			chat
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Router			/llm/providers [get]
func handleListProviders(engine *chat.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"providers": engine.Providers().Describe(),
			"default":   engine.Providers().Default(),
		})
	}
}

func names(definitions []tools.Definition) []string {
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Name)
	}
	return out
}

// respondChatError maps the domain errors onto status codes.
//
// A spend-cap breach is the one that matters most to get right: it is 429 with
// §3.3's structured payload, not a 500, because the SPA has to tell the developer
// they have hit a limit and when it resets.
func respondChatError(c *gin.Context, err error) {
	var limitErr *admin.LimitError
	if errors.As(err, &limitErr) {
		c.JSON(http.StatusTooManyRequests, limitErr.Payload())
		return
	}

	switch {
	case errors.Is(err, chat.ErrNoSuchSession):
		c.JSON(http.StatusNotFound, gin.H{"error": "no such chat session"})
	case errors.Is(err, chat.ErrNoSuchConfirmation):
		c.JSON(http.StatusNotFound, gin.H{"error": "no such confirmation"})
	case errors.Is(err, chat.ErrAlreadyResolved):
		c.JSON(http.StatusConflict, gin.H{"error": "this confirmation was already resolved"})
	case errors.Is(err, chat.ErrInvalidRequest),
		errors.Is(err, tools.ErrInvalidTier):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		// Everything else here is a policy refusal or a provider misconfiguration —
		// an unpermitted provider, a model outside the allow-list, a tier above the
		// admin ceiling, a session cap. All are the caller's request being refused
		// rather than ODE failing, so 403 rather than 500.
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	}
}

// AuthenticateMCP is how the MCP transport authorises, reusing this package's
// token handling rather than repeating it.
//
// The MCP handler cannot sit behind auth.Middleware, because it has to read the
// session header and resolve an exposure tier before it can decide which tools to
// offer — work that happens before any gin route. So it is handed this function,
// which applies exactly the checks the middleware applies: parse the token, reject
// a token with no subject, and require the same realm role. Anything less here
// would be a second, weaker front door to the same tools.
func AuthenticateMCP(requiredRole string) func(*http.Request) (userSub, token string, err error) {
	return func(request *http.Request) (string, string, error) {
		parsed, err := servicejwt.GetParsedToken(request)
		if err != nil {
			return "", "", errors.New("missing or invalid auth token")
		}
		if err := parsed.Valid(); err != nil {
			return "", "", errors.New("invalid auth token")
		}
		if requiredRole != "" && !parsed.HasRole(requiredRole) {
			return "", "", fmt.Errorf("missing required realm role %q", requiredRole)
		}
		return parsed.Sub, parsed.Jwt(), nil
	}
}
