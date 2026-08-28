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
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// The workbenches a developer has open.
//
// A workbench is one working context: a repository checkout and the kernel that
// runs in it. This group creates, lists, renames and closes them; everything that
// *acts* in one — the repository routes, the kernel routes — names it with a
// `workbench` query parameter instead, so those routes stay static and a request
// that names none still means "my only one".
//
// Like every other route here, whose workbenches these are comes from the caller's
// own token. There is no user parameter, and an id belonging to somebody else
// answers 404 rather than 403 — knowing that another developer's workbench exists
// is not something an id in a URL should buy.

type workbenchBody struct {
	// Title is the developer's own name for it. Optional: without one the label
	// falls back to the repository, and before there is one, to the id.
	Title string `json:"title"`
}

// @Summary		The caller's workbenches
// @Description	One working context each: a repository checkout and the kernel that
// @Description	runs in it. Oldest first, so a picker does not reorder itself while
// @Description	the developer is aiming at an entry.
// @Tags			workbenches
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Router			/workbenches [get]
func handleListWorkbenches(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		benches, err := svc.Workbenches(c.Request.Context(), token.Sub)
		if err != nil {
			respondRepo(c, err)
			return
		}
		if benches == nil {
			benches = []repo.Workbench{}
		}
		c.JSON(http.StatusOK, gin.H{
			"workbenches": benches,
			// Reported so the SPA can disable the button rather than let a click fail,
			// and so a developer can see why: the cap is on kernels in one pod.
			"max": svc.MaxWorkbenches(),
		})
	}
}

// @Summary		Open a workbench
// @Description	Creates an empty one. Nothing is cloned and no pod is touched: the
// @Description	repository is chosen afterwards with POST /repo/link, naming this
// @Description	workbench.
// @Tags			workbenches
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		workbenchBody	false	"an optional title"
// @Success		201		{object}	repo.Workbench
// @Failure		409		{object}	map[string]string	"as many are open as this deployment allows"
// @Router			/workbenches [post]
func handleCreateWorkbench(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		var body workbenchBody
		// A body is optional: opening one with no name is the ordinary case.
		_ = c.ShouldBindJSON(&body)

		bench, err := svc.CreateWorkbench(c.Request.Context(), token.Sub, body.Title)
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusCreated, bench)
	}
}

// @Summary		Rename a workbench
// @Description	Sets the developer's own name for it. An empty title clears it, which
// @Description	puts the label back to the repository.
// @Tags			workbenches
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			id		path		string			true	"workbench id"
// @Param			body	body		workbenchBody	true	"the new title"
// @Success		200		{object}	repo.Workbench
// @Failure		404		{object}	map[string]string
// @Router			/workbenches/{id} [put]
func handleRenameWorkbench(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		var body workbenchBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		bench, err := svc.RenameWorkbench(
			c.Request.Context(), token.Sub, strings.TrimSpace(c.Param("id")), body.Title)
		if err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, bench)
	}
}

// @Summary		Close a workbench
// @Description	Forgets it. The checkout stays on the PVC — it is the developer's work
// @Description	and may hold uncommitted changes (§5.11 item 6) — and the kernel is
// @Description	left running rather than killed, because something may still be in it.
// @Description	Opening a workbench on the same repository again reuses the directory.
// @Tags			workbenches
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"workbench id"
// @Success		200	{object}	map[string]bool
// @Failure		404	{object}	map[string]string
// @Router			/workbenches/{id} [delete]
func handleDeleteWorkbench(svc *repo.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.MustFromContext(c)
		if err := svc.DeleteWorkbench(
			c.Request.Context(), token.Sub, strings.TrimSpace(c.Param("id"))); err != nil {
			respondRepo(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"closed": true})
	}
}
