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
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/repo"
)

// The experiment surface (§5.12, M8).
//
// Every route resolves the developer from their own token, exactly as the kernel
// and repo routes do: ODE holds a Ray and an MLflow service account, and the only
// thing that stops one developer reading another's run is that no route here takes
// a parameter naming whose. The ownership check is in the store's own WHERE clause
// rather than applied after a read, so a route added later cannot forget it.
//
// On HTTP throughout rather than on the WebSocket. A launch is slow — a git
// archive in the pod, an upload, three API calls — but it is bounded in seconds
// and it produces one answer; the thing that takes hours is the job, and that is
// polled rather than streamed. Nothing here needs cancellation.
//
// Logs have a route and no tool, which is §5.13 made structural rather than
// conventional: the developer reads them, the model never does.

// experimentRequest builds the service request from the validated token.
func experimentRequest(c *gin.Context) experiments.Request {
	token := auth.MustFromContext(c)
	return experiments.Request{
		Bearer:  auth.Bearer(c),
		UserSub: token.Sub,
		// The Hub username names the developer's MLflow experiment (D17), because
		// that string is what a human reads in MLflow's own UI.
		Username: token.Username,
		// The chat session a launch came from, when the SPA says. One of §5.12's four
		// metadata keys, and optional: a launch from the Experiments pane has none.
		SessionID: c.Query("session_id"),
		// Which working copy the commit comes from. Absent means the developer's only
		// workbench, as on every other route that takes one.
		WorkbenchID: strings.TrimSpace(c.Query("workbench")),
		Author: repo.Author{
			Name: token.Username, Email: token.Email, Sub: token.Sub,
		},
	}
}

type launchBody struct {
	// Entrypoint is the command Ray runs. Empty takes the deployment's default.
	Entrypoint string `json:"entrypoint"`
	// EnvVars are extra environment variables for the job, validated at the service
	// boundary: bounded in count and size, and unable to override what ODE sets.
	EnvVars map[string]string `json:"env_vars"`
	RunName string            `json:"run_name"`
	// SessionID ties the run to a chat session, so §5.13's summary can be injected
	// back into the conversation it came from.
	SessionID string `json:"session_id"`
}

// @Summary		Launch an experiment
// @Description	Packages the developer's **committed** repository state with
// @Description	`git archive`, uploads it to Ray, creates the MLflow run and tags it with
// @Description	the commit SHA, then submits the job (§5.12, §5.11 item 7).
// @Description
// @Description	A working copy with uncommitted changes is refused with 409 and the
// @Description	paths that made it dirty: the run records a commit SHA and is only
// @Description	reproducible from it if the code that ran is that commit.
// @Tags			experiments
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			body	body		launchBody	false	"the entrypoint, environment and run name"
// @Success		201		{object}	experiments.LaunchResult
// @Failure		400		{object}	map[string]string
// @Failure		401		{object}	map[string]string
// @Failure		409		{object}	map[string]interface{}	"the working copy is uncommitted, no repository is linked, or the package is too large"
// @Failure		502		{object}	map[string]string		"Ray or MLflow could not be reached"
// @Router			/experiments [post]
func handleLaunchExperiment(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body launchBody
		// A launch with no body at all is legitimate: it means "run the default
		// entrypoint against what is committed", which is the common case. So an
		// empty body is read as an absent one — and *only* an empty body.
		//
		// The guard used to be `ContentLength > 0`, which is not the same test and is
		// wrong for every client that streams. A chunked request has a ContentLength
		// of -1, so its body was skipped without a word: the entrypoint, the
		// environment, the run name and the session the run belongs to were all
		// dropped, the deployment default ran instead, and the developer saw a 201
		// with an experiment id. Silent, and in the direction that costs cluster time.
		if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		request := experimentRequest(c)
		if body.SessionID != "" {
			request.SessionID = body.SessionID
		}

		result, err := svc.Launch(c.Request.Context(), experiments.LaunchRequest{
			Request:    request,
			Entrypoint: body.Entrypoint,
			EnvVars:    body.EnvVars,
			RunName:    body.RunName,
		})
		if err != nil {
			respondExperiments(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	}
}

// @Summary		The caller's experiments
// @Description	Newest first. Statuses are refreshed from Ray for the runs that have
// @Description	not finished, and only those — a listing of finished runs costs no
// @Description	cluster calls. Resolved from the caller's own token: no route here takes
// @Description	a user parameter.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			limit	query		int	false	"how many to return; default 100"
// @Success		200		{object}	map[string]interface{}
// @Failure		401		{object}	map[string]string
// @Router			/experiments [get]
func handleListExperiments(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 0
		if raw := c.Query("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
				return
			}
			limit = parsed
		}

		records, err := svc.List(c.Request.Context(), experimentRequest(c), limit)
		if err != nil {
			respondExperiments(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"experiments": records,
			"count":       len(records),
			// Where a browser should open the two services, so the pane can offer
			// "open in new tab" without knowing ODE's configuration.
			"ray_url":    svc.DashboardURL(),
			"mlflow_url": svc.TrackingUIURL(),
		})
	}
}

// @Summary		One experiment
// @Description	The record plus its status, refreshed from Ray unless the job has
// @Description	already finished. Another developer's experiment answers 404 rather than
// @Description	403: nothing here reveals that one exists.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"the experiment id"
// @Success		200	{object}	experiments.Experiment
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"Ray could not be reached"
// @Router			/experiments/{id} [get]
func handleGetExperiment(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		record, err := svc.Get(c.Request.Context(), experimentRequest(c), c.Param("id"))
		if err != nil {
			respondExperiments(c, err)
			return
		}
		c.JSON(http.StatusOK, record)
	}
}

// @Summary		One experiment's structured summary
// @Description	§5.13's compact structured summary: status, params, the latest value
// @Description	of each metric, the run's tags, resource usage and the comparison against
// @Description	the previous run of the same experiment. **Never logs** — those are a
// @Description	route of their own, and no LLM tool reaches them.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"the experiment id"
// @Success		200	{object}	experiments.Summary
// @Failure		400	{object}	map[string]string	"the experiment has no MLflow run"
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"MLflow could not be reached"
// @Router			/experiments/{id}/results [get]
func handleExperimentResults(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		summary, err := svc.Results(c.Request.Context(), experimentRequest(c), c.Param("id"))
		if err != nil {
			respondExperiments(c, err)
			return
		}
		c.JSON(http.StatusOK, summary)
	}
}

// handleExperimentLogs is the developer's own view of a job's output.
//
// It exists as a route so that §5.13's "never raw logs" is a property of the
// design rather than of discipline: the developer has them here, and the tool
// surface has no method that could reach them.
//
// @Summary		One job's driver output
// @Description	The developer's own view of what the job printed, tail-capped. This is
// @Description	deliberately not available to the assistant: §5.13 builds a structured
// @Description	summary and says raw logs never enter the model's context.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"the experiment id"
// @Success		200	{object}	experiments.LogPage
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"Ray could not be reached"
// @Router			/experiments/{id}/logs [get]
func handleExperimentLogs(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, err := svc.Logs(c.Request.Context(), experimentRequest(c), c.Param("id"))
		if err != nil {
			respondExperiments(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

// @Summary		Stop a running job
// @Description	Asks Ray to stop it and reads the status back rather than assuming it:
// @Description	Ray stops asynchronously, and a record claiming STOPPED while the job was
// @Description	still winding down would disagree with the dashboard beside it. A job that
// @Description	has already finished is answered with its record, not an error.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			id	path		string	true	"the experiment id"
// @Success		200	{object}	experiments.Experiment
// @Failure		401	{object}	map[string]string
// @Failure		404	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"Ray could not be reached"
// @Router			/experiments/{id}/stop [post]
func handleStopExperiment(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		record, err := svc.Stop(c.Request.Context(), experimentRequest(c), c.Param("id"))
		if err != nil {
			respondExperiments(c, err)
			return
		}
		c.JSON(http.StatusOK, record)
	}
}

// handleExperimentEmbed is the backend half of D6.
//
// The frontend half is the SPA's: a hidden iframe with a load timeout, falling
// back to a link-only card. This answers the question the browser handles worst —
// a service answering X-Frame-Options: DENY produces a blank frame and no event
// the SPA can catch, so without this the pane would wait out its whole timeout on
// every open.
//
// @Summary		Whether the Ray and MLflow UIs can be framed
// @Description	The backend half of D6: each configured service is asked for its
// @Description	X-Frame-Options and Content-Security-Policy frame-ancestors, and the
// @Description	verdict is cached with a TTL. `embeddable` is "yes", "no" or "unknown",
// @Description	and **"unknown" is a real answer** — ODE is inside the cluster and the
// @Description	browser is not, so a service ODE cannot reach may still frame perfectly.
// @Description	The pane should still load a hidden iframe with a timeout and fall back
// @Description	to a link-only card, which is the half only a browser can decide.
// @Tags			experiments
// @Produce		json
// @Security		Bearer
// @Param			refresh	query		bool	false	"re-probe rather than answering from the cache"
// @Success		200		{object}	experiments.EmbedReport
// @Failure		401		{object}	map[string]string
// @Router			/experiments/embed [get]
func handleExperimentEmbed(svc *experiments.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		refresh := c.Query("refresh") == "true"
		c.JSON(http.StatusOK, svc.EmbedProbes(c.Request.Context(), refresh))
	}
}

// respondExperiments maps this package's domain errors onto status codes.
//
// The two 409s are the pane's own control flow rather than errors, the same shape
// the repo routes use: an uncommitted working copy and an oversized package are
// both well-formed requests whose answer is something the developer does next.
// Naming it in a `needs` field is what lets the pane offer a commit button instead
// of printing a sentence.
func respondExperiments(c *gin.Context, err error) {
	var dirty *experiments.DirtyError
	var tooLarge *experiments.PackageTooLargeError
	var upstream *experiments.UpstreamError

	switch {
	case errors.As(err, &dirty):
		body := gin.H{
			"error": dirty.Error(),
			"needs": "commit",
			"hint": "commit the working copy and launch again; an experiment is submitted " +
				"from a commit so that its MLflow run is reproducible from one",
			"repository": dirty.Repository,
			"unborn":     dirty.Unborn,
		}
		if len(dirty.Paths) > 0 {
			body["uncommitted"] = dirty.Paths
			body["uncommitted_elided"] = dirty.Elided
		}
		c.JSON(http.StatusConflict, body)
	case errors.As(err, &tooLarge):
		c.JSON(http.StatusConflict, gin.H{
			"error": tooLarge.Error(),
			"needs": "smaller_package",
			"bytes": tooLarge.Bytes,
			"limit": tooLarge.Limit,
			"hint": "exclude what the job does not need from the repository, or raise " +
				"experiment_max_package_bytes",
		})
	case errors.Is(err, experiments.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, experiments.ErrInvalidRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.As(err, &upstream):
		// Ray's and MLflow's own codes where they mean something to a client, and 502
		// otherwise: the thing that refused is the cluster rather than ODE.
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   upstream.Error(),
			"service": upstream.Service,
			"code":    upstream.Code,
		})
	default:
		// Everything the repo and kernel surfaces can answer with already has a
		// mapping — no repository linked, no checkout, a busy kernel, an unreachable
		// Hub — and a launch reaches all of them on its way to the cluster.
		respondRepo(c, err)
	}
}
