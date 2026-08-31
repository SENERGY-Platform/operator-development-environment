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
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/admin"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// The repository surface (§5.11, M7).
//
// Every route resolves the developer from their own token, exactly as the kernel
// routes do and for the same reason: ODE holds a GitHub credential per developer
// and there is no parameter anywhere here that names whose.
//
// Two shapes are worth noticing. The OAuth flow has no unauthenticated callback —
// the redirect lands on the SPA, which posts the code back with its own platform
// token, so this whole group sits behind the realm-role gate. And nothing here
// commits on the developer's behalf: /commit and /push are routes a developer
// calls, never side effects of another one (§5.11 item 5).

// repoRequest builds the service request from the validated token.
//
// The author of a commit comes from the platform token rather than from the
// GitHub account, so history says who did the work in the platform's terms.
//
// The workbench comes from a query parameter rather than the path, for the reason
// the file routes give: it keeps every route static, and it lets a request that
// names none mean "my only workbench" — which is what every client sent before
// workbenches existed, and is still the right answer for a developer who has one.
func repoRequest(c *gin.Context) repo.Request {
	token := auth.MustFromContext(c)
	return repo.Request{
		Bearer:  auth.Bearer(c),
		UserSub: token.Sub,
		Author: repo.Author{
			Name:  token.Username,
			Email: token.Email,
			Sub:   token.Sub,
		},
		WorkbenchID: strings.TrimSpace(c.Query("workbench")),
	}
}

// @Summary		The developer's GitHub connection
// @Description	Whether this developer has connected a GitHub account, which account,
// @Description	and whether the grant actually carries the scopes §5.11 item 1 needs.
// @Description	Never the credential itself.
// @Description
// @Description	`?verify=true` additionally asks GitHub whether the stored credential
// @Description	still works, and reports what it said: the status, GitHub's own message,
// @Description	the scopes it reports for the token, and the token's *kind* — its public
// @Description	prefix, which is what tells an OAuth app's non-expiring token from a
// @Description	GitHub App's user token that expires in hours. Never any part of the
// @Description	credential's value. Off by default because the pane polls this route and
// @Description	a GitHub round trip per poll is not free.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Param			verify	query		bool	false	"ask GitHub whether the credential still works"
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Router			/repo/connection [get]
func handleRepoConnection(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		identity, connected, err := svc.Connection(c.Request.Context(), token.Sub)
		if err != nil {
			respondRepo(c, err)
			return
		}
		body := gin.H{"connected": connected, "scopes_requested": svc.Scopes()}
		if connected {
			body["identity"] = identity
		}
		if connected && c.Query("verify") == "true" {
			verification, err := svc.Verify(c.Request.Context(), token.Sub)
			if err != nil {
				respondRepo(c, err)
				return
			}
			body["verification"] = verification
		}
		c.JSON(http.StatusOK, body)
	}
}

// handleRepoAuthorize starts the OAuth web flow.
//
// @Summary		Begin the GitHub OAuth flow
// @Description	Returns the authorize URL and the single-use state bound to this
// @Description	developer. The SPA opens the URL; GitHub returns the browser to the
// @Description	SPA, which posts the code back to POST /repo/connection.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	repo.AuthorizeRequest
// @Failure		400	{object}	map[string]string
// @Failure		401	{object}	map[string]string
// @Router			/repo/connection/authorize [post]
func handleRepoAuthorize(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		authorize, err := svc.Authorize(token.Sub)
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, authorize)
	}
}

type connectBody struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

// @Summary		Complete the GitHub OAuth flow
// @Description	Exchanges the code for a token, reads which account it belongs to and
// @Description	stores it encrypted (§5.11 item 1). The state has to be the one this
// @Description	developer was issued, and it is single-use.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		connectBody	true	"the code and state GitHub returned"
// @Success		200		{object}	repo.Identity
// @Failure		400		{object}	map[string]string
// @Failure		401		{object}	map[string]string
// @Failure		502		{object}	map[string]string	"GitHub refused the exchange"
// @Router			/repo/connection [post]
func handleRepoConnect(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body connectBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		token := auth.MustFromContext(c)
		identity, err := svc.Connect(c.Request.Context(), token.Sub, body.Code, body.State)
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, identity)
	}
}

// @Summary		Disconnect GitHub
// @Description	Forgets the credential. The working copy on the developer's PVC is left
// @Description	exactly where it is — it is their work (§5.11 item 6).
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]bool
// @Failure		401	{object}	map[string]string
// @Router			/repo/connection [delete]
func handleRepoDisconnect(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		if err := svc.Disconnect(c.Request.Context(), token.Sub); err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"disconnected": true})
	}
}

// @Summary		The repositories this developer could work on
// @Description	Read under the developer's own GitHub token. A repository they cannot
// @Description	push to is listed and says so, rather than being filtered out and
// @Description	failing later.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string][]repo.Repository
// @Failure		401	{object}	map[string]string
// @Failure		409	{object}	map[string]string	"no GitHub account is connected"
// @Failure		502	{object}	map[string]string
// @Router			/repo/repositories [get]
func handleRepoRepositories(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		repositories, err := svc.Repositories(c.Request.Context(), repoRequest(c))
		if err != nil {
			respondRepo(c, err)
			return
		}
		if repositories == nil {
			repositories = []repo.Repository{}
		}
		c.JSON(http.StatusOK, gin.H{"repositories": repositories})
	}
}

type selectBody struct {
	FullName string `json:"full_name"`
	Scaffold bool   `json:"scaffold"`
}

// @Summary		Work on an existing repository
// @Description	Links the repository and makes sure its working copy is on the PVC:
// @Description	clone if it is not there, reuse it if it is — including its uncommitted
// @Description	changes, which the answer reports (§5.11 items 5 and 6).
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		selectBody	true	"the repository to work on"
// @Success		200		{object}	repo.Status
// @Failure		400		{object}	map[string]string
// @Failure		409		{object}	map[string]string	"no GitHub account is connected"
// @Failure		502		{object}	map[string]string	"GitHub, the Hub or git refused"
// @Router			/repo/link [post]
func handleRepoSelect(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body selectBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		status, err := svc.Select(c.Request.Context(), repo.SelectRequest{
			Request:  repoRequest(c),
			FullName: body.FullName,
			Scaffold: body.Scaffold,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

type createBody struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Private      bool   `json:"private"`
	Organisation string `json:"organisation"`
	// Scaffold defaults to true, which is why it is a pointer: an absent field and
	// an explicit false have to be told apart, and a created repository with no
	// scaffold is not what anyone asked for.
	Scaffold *bool `json:"scaffold"`
}

// @Summary		Create a repository and scaffold its working copy
// @Description	Creates an *empty* repository on GitHub, clones it, and writes the
// @Description	operator template of §5.11 item 3 into the working copy — pinning
// @Description	Operator Lib at the newest ref at scaffold time (D15). Nothing is
// @Description	committed: the developer's own commit is the repository's first.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		createBody	true	"the repository to create"
// @Success		201		{object}	repo.Status
// @Failure		400		{object}	map[string]string
// @Failure		409		{object}	map[string]string	"no GitHub account is connected"
// @Failure		502		{object}	map[string]string	"GitHub refused, or the name is taken"
// @Router			/repo/repositories [post]
func handleRepoCreate(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body createBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		scaffold := true
		if body.Scaffold != nil {
			scaffold = *body.Scaffold
		}
		status, err := svc.Create(c.Request.Context(), repo.CreateRequest{
			Request:      repoRequest(c),
			Name:         body.Name,
			Description:  body.Description,
			Private:      body.Private,
			Organisation: body.Organisation,
			Scaffold:     scaffold,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusCreated, status)
	}
}

// @Summary		The working copy's state
// @Description	Which repository, which branch, how far ahead and behind the remote,
// @Description	what is uncommitted, and which of the compliance files are present.
// @Description	`fetch=true` contacts the remote first, which is what makes the
// @Description	divergence current rather than remembered (§5.11 item 5).
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Param			fetch	query		bool	false	"contact the remote before answering"
// @Success		200		{object}	repo.Status
// @Failure		409		{object}	map[string]string	"no repository is selected"
// @Failure		502		{object}	map[string]string
// @Router			/repo [get]
func handleRepoStatus(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		status, err := svc.Status(c.Request.Context(), repo.StatusRequest{
			Request: repoRequest(c),
			Fetch:   c.Query("fetch") == "true",
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// @Summary		Stop working on this repository
// @Description	Forgets the link. The checkout stays on the PVC, so selecting the
// @Description	repository again is a reuse rather than a clone.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]bool
// @Router			/repo/link [delete]
func handleRepoUnlink(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.Unlink(c.Request.Context(), repoRequest(c)); err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"unlinked": true})
	}
}

// @Summary		Fetch and report divergence
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	repo.Status
// @Failure		409	{object}	map[string]string	"no repository is selected"
// @Failure		502	{object}	map[string]string
// @Router			/repo/fetch [post]
func handleRepoFetch(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		status, err := svc.Fetch(c.Request.Context(), repo.FetchRequest{Request: repoRequest(c)})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// @Summary		Write the missing template files into the working copy
// @Description	Never overwrites: a file that is there belongs to the developer, and the
// @Description	answer says what was written and what was left alone. Nothing is
// @Description	committed.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	repo.ScaffoldResult
// @Failure		409	{object}	map[string]string	"no repository is selected, or it is not cloned"
// @Failure		502	{object}	map[string]string
// @Router			/repo/scaffold [post]
func handleRepoScaffold(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := svc.Scaffold(c.Request.Context(),
			repo.ScaffoldRequest{Request: repoRequest(c)})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

type commitBody struct {
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
}

// @Summary		Commit the working copy
// @Description	An explicit developer action. No other route in ODE commits, and no LLM
// @Description	tool exists that does (§5.11 item 5, §5.8).
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		commitBody	true	"the message, and optionally which paths"
// @Success		200		{object}	repo.CommitResult
// @Failure		400		{object}	map[string]string
// @Failure		409		{object}	map[string]string	"nothing to commit, or no repository"
// @Failure		502		{object}	map[string]string
// @Router			/repo/commit [post]
func handleRepoCommit(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body commitBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := svc.Commit(c.Request.Context(), repo.CommitRequest{
			Request: repoRequest(c),
			Message: body.Message,
			Paths:   body.Paths,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

type draftBody struct {
	Paths []string `json:"paths"`
}

// @Summary		Draft a commit message
// @Description	Asks the configured LLM provider for a commit message for the uncommitted
// @Description	work: the recent subjects of this repository as style, the diff against the
// @Description	last commit, and the content of files git does not track yet.
// @Description
// @Description	It changes nothing. The working copy, the index and the remote are untouched,
// @Description	the answer is text the developer edits or discards, and committing is still
// @Description	the separate explicit action of §5.11 item 5.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		draftBody	false	"which paths, if not everything uncommitted"
// @Success		200		{object}	repo.Draft
// @Failure		409		{object}	map[string]string	"nothing to commit, or no repository"
// @Failure		429		{object}	map[string]string	"the developer is at a spend cap (§3.3)"
// @Failure		502		{object}	map[string]string
// @Failure		503		{object}	map[string]string	"no LLM provider is configured"
// @Router			/repo/commit/message [post]
func handleRepoCommitMessage(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body draftBody
		// Optional, like push's: a draft of everything uncommitted is the common case
		// and should not require a body.
		_ = c.ShouldBindJSON(&body)
		draft, err := svc.DraftCommitMessage(c.Request.Context(), repo.DraftRequest{
			Request: repoRequest(c),
			Paths:   body.Paths,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, draft)
	}
}

type pushBody struct {
	Branch string `json:"branch"`
}

// @Summary		Push to GitHub
// @Description	Pushes the current branch under the developer's own GitHub credential.
// @Description	git's own reporting comes back, because the useful part of a push is
// @Description	what the remote said.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		pushBody	false	"the branch, if not the current one"
// @Success		200		{object}	repo.PushResult
// @Failure		409		{object}	map[string]string	"no repository is selected"
// @Failure		502		{object}	map[string]string	"the remote refused the push"
// @Router			/repo/push [post]
func handleRepoPush(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body pushBody
		// A body is optional here: pushing the current branch is the common case and
		// should not require one.
		_ = c.ShouldBindJSON(&body)
		result, err := svc.Push(c.Request.Context(), repo.PushRequest{
			Request: repoRequest(c),
			Branch:  body.Branch,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

type stashBody struct {
	Message string `json:"message"`
}

// @Summary		Stash uncommitted changes
// @Description	The reversible of the three answers §5.11 item 6 requires for
// @Description	uncommitted work found on reopen. Includes untracked files.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		stashBody	false	"an optional stash message"
// @Success		200		{object}	repo.Status
// @Failure		409		{object}	map[string]string
// @Failure		502		{object}	map[string]string
// @Router			/repo/stash [post]
func handleRepoStash(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body stashBody
		_ = c.ShouldBindJSON(&body)
		status, err := svc.Stash(c.Request.Context(), repo.StashRequest{
			Request: repoRequest(c),
			Message: body.Message,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

type discardBody struct {
	Confirm bool `json:"confirm"`
}

// @Summary		Discard uncommitted changes
// @Description	The destructive answer of the three, so it requires `confirm: true` and
// @Description	the service refuses without it. Resets tracked files to HEAD and removes
// @Description	untracked ones.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		discardBody	true	"confirm: true"
// @Success		200		{object}	repo.Status
// @Failure		400		{object}	map[string]string	"not confirmed"
// @Failure		409		{object}	map[string]string
// @Failure		502		{object}	map[string]string
// @Router			/repo/discard [post]
func handleRepoDiscard(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body discardBody
		_ = c.ShouldBindJSON(&body)
		status, err := svc.Discard(c.Request.Context(), repo.DiscardRequest{
			Request: repoRequest(c),
			Confirm: body.Confirm,
		})
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// @Summary		The working copy's file tree
// @Description	Every file of the repository, with nothing hidden and nothing reserved
// @Description	(D14) — the workflow file and the gitignore are editable like any other.
// @Description	Only `.git` is excluded, because it is git's storage rather than source.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	repo.FileTree
// @Failure		409	{object}	map[string]string	"no repository is selected"
// @Failure		502	{object}	map[string]string
// @Router			/repo/files [get]
func handleRepoFiles(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		tree, err := svc.Files(c.Request.Context(), repoRequest(c))
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, tree)
	}
}

// @Summary		Read one file of the working copy
// @Description	A file that is not UTF-8 comes back marked binary with no text rather
// @Description	than as mangled content, and one longer than the limit comes back
// @Description	truncated and says so.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Param			path	query		string	true	"path relative to the repository root"
// @Success		200		{object}	repo.File
// @Failure		400		{object}	map[string]string	"the path leaves the repository"
// @Failure		404		{object}	map[string]string
// @Failure		409		{object}	map[string]string	"no repository is selected"
// @Router			/repo/files/content [get]
func handleRepoReadFile(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		file, err := svc.ReadFile(c.Request.Context(), repoRequest(c), c.Query("path"))
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, file)
	}
}

type writeFileBody struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// @Summary		Write one file of the working copy
// @Description	Writes the file and nothing else: no staging, no commit. The same
// @Description	operation the `write_file` tool performs, through the same service —
// @Description	a second path would be a second, laxer one.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		writeFileBody	true	"the path and its new content"
// @Success		200		{object}	repo.WriteResult
// @Failure		400		{object}	map[string]string	"the path leaves the repository, or the content is too large"
// @Failure		409		{object}	map[string]string	"no repository is selected"
// @Router			/repo/files/content [put]
func handleRepoWriteFile(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body writeFileBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := svc.WriteFile(c.Request.Context(), repoRequest(c),
			body.Path, []byte(body.Content))
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// @Summary		Delete a file or directory from the working copy
// @Description	From the working copy, not from history: the deletion is an uncommitted
// @Description	change like any other and reaches GitHub when the developer commits it.
// @Tags			repo
// @Produce		json
// @Security		Bearer
// @Param			path		query		string	true	"path relative to the repository root"
// @Param			recursive	query		bool	false	"required to delete a directory"
// @Success		200			{object}	map[string]bool
// @Failure		400			{object}	map[string]string
// @Failure		404			{object}	map[string]string
// @Router			/repo/files/content [delete]
func handleRepoDeleteFile(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := svc.Delete(c.Request.Context(), repoRequest(c),
			c.Query("path"), c.Query("recursive") == "true")
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"deleted": true})
	}
}

type makeDirBody struct {
	Path string `json:"path"`
}

// @Summary		Create a directory in the working copy
// @Description	git does not track an empty directory, which is why this exists: a
// @Description	developer laying out a package needs the directory before the file.
// @Tags			repo
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		makeDirBody	true	"the directory to create"
// @Success		200		{object}	map[string]bool
// @Failure		400		{object}	map[string]string
// @Router			/repo/files/directory [post]
func handleRepoMakeDir(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body makeDirBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := svc.MakeDir(c.Request.Context(), repoRequest(c), body.Path); err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"created": true})
	}
}

// respondRepo maps this package's domain errors onto status codes.
//
// The three that matter are told apart on purpose. "No GitHub account" and "no
// repository selected" are 409 rather than 404: the request was well formed and
// the answer is a step the developer has not taken yet, which is what the SPA
// turns into a connect card or a repository picker. A git failure is 502, because
// the thing that refused is the remote or the pod rather than ODE.
func respondRepo(c *gin.Context, err error) {
	var gitErr *repo.GitError
	var upstream *repo.UpstreamError
	var limitErr *admin.LimitError

	// A spend cap is answered the way the chat surface answers it, with §3.3's own
	// payload: the commit message draft is a provider request, and a developer who
	// has hit a cap needs to read the same "how much, and when does it reset" here
	// as they do there.
	if errors.As(err, &limitErr) {
		c.JSON(http.StatusTooManyRequests, limitErr.Payload())
		return
	}

	switch {
	case errors.Is(err, repo.ErrDraftsUnavailable):
		// Not an error in this deployment's terms: ODE is served without a provider
		// on purpose, and the answer is that the developer writes the message.
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":     err.Error(),
			"available": false,
			"hint":      "write the commit message yourself; drafting needs a configured LLM provider",
		})
	case errors.Is(err, repo.ErrDraftFailed):
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	case errors.Is(err, repo.ErrInvalidRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, repo.ErrNotConnected):
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "github_connection",
			"hint":  "start the OAuth flow with POST /repo/connection/authorize",
		})
	case errors.Is(err, repo.ErrCredentialRejected):
		// The same `needs` as no connection at all, because it is the same repair —
		// and a 409 rather than the 502 this used to be: nothing upstream broke, the
		// grant this deployment was given has gone, and reconnecting is a step the
		// developer can take without help.
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "github_connection",
			"hint": "the stored GitHub credential is no longer accepted — it was revoked, " +
				"expired, or the authorisation was withdrawn; connect the account again",
		})
	case errors.Is(err, repo.ErrNoWorkbench):
		// The same answer for "does not exist" and "belongs to somebody else", which
		// is what the service already decided: an id in a URL must not be enough to
		// learn that another developer's workbench exists.
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, repo.ErrRepositoryInUse):
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "another_repository",
			"hint": "a repository is open in one workbench at a time; work in the one " +
				"that has it, or select a different repository here",
		})
	case errors.Is(err, repo.ErrTooManyWorkbenches):
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "free_workbench",
			"hint":  "close a workbench before opening another; each one is a kernel in the pod",
		})
	case errors.Is(err, repo.ErrNoRepository):
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "repository",
			"hint":  "select one with POST /repo/link, or create one with POST /repo/repositories",
		})
	case errors.Is(err, repo.ErrNotCloned):
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "clone",
			"hint":  "select the repository again to clone it into the workspace",
		})
	case errors.Is(err, repo.ErrNothingToCommit):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "nothing_to_commit": true})
	case errors.Is(err, repo.ErrRemoteMismatch):
		// A conflict rather than a gateway error: nothing upstream failed. The
		// checkout in the developer's pod points at a different repository than the
		// one they have selected, and committing or pushing into it would put the
		// work somewhere they did not ask for. Named here rather than left to fall
		// through to the GitError case, which would report it as "upstream platform
		// request failed" and send them looking at the wrong thing.
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "remote_match",
			"hint": "the checkout's origin is not the selected repository; select the " +
				"repository the checkout points at, or move the directory aside",
		})
	case errors.As(err, &gitErr):
		body := gin.H{
			"error":     gitErr.Error(),
			"git":       gitErr.Args[0],
			"exit_code": gitErr.ExitCode,
			"timed_out": gitErr.TimedOut,
		}
		// What ODE checked and git could not. Only ever set where it changes what to
		// do next — see repo.explainAuth.
		if gitErr.Hint != "" {
			body["hint"] = gitErr.Hint
		}
		c.JSON(http.StatusBadGateway, body)
	case errors.As(err, &upstream) && upstream.Code == http.StatusUnauthorized:
		// A 401 from GitHub's API is the credential, whatever route asked. Answered as
		// the same refusal a rejected push produces, rather than as GitHub's own status
		// passed through: the repair is one step the developer takes, and every surface
		// that reaches GitHub — the repository list, creating one, linking one,
		// resolving the Operator Lib pin — was until now rendering "401: Bad
		// credentials" as text beside a spinner that never stopped.
		//
		// It also stops the SPA reading it as a platform-session problem. 401 on an
		// ODE route means *this* request was not authenticated; the developer's session
		// is fine, and GitHub is the one refusing.
		c.JSON(http.StatusConflict, gin.H{
			"error": repo.ErrCredentialRejected.Error() + ": GitHub answered " +
				strconv.Quote(upstream.Message),
			"needs": "github_connection",
			"hint": "the stored GitHub credential is no longer accepted — it was revoked, " +
				"expired, or the authorisation was withdrawn; connect the account again",
		})
	case errors.As(err, &upstream):
		// GitHub's own code where it is meaningful to a client.
		switch upstream.Code {
		case http.StatusForbidden, http.StatusNotFound,
			http.StatusUnprocessableEntity:
			c.JSON(upstream.Code, gin.H{"error": upstream.Error()})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": upstream.Error()})
		}
	case errors.Is(err, kernel.ErrBusy):
		// The pod runs one cell at a time, and the repository operations run in it.
		// Reported only after the bounded wait of kernel.Options.WorkspaceWait, so
		// what is left here is a kernel held by something longer than any repository
		// operation — a training run, and saying so is the honest answer.
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"needs": "idle_kernel",
			"hint":  "the kernel is running a cell; the repository operations share it",
		})
	default:
		// Everything the kernel can answer with — a spawn still pending, a missing
		// path, an unreachable Hub — already has a mapping, so it is reused rather
		// than restated.
		respondKernel(c, err)
	}
}
