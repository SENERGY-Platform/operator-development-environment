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

	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
)

// The kernel surface (SPEC §5.6, M4).
//
// Every route resolves the developer from their own token and nothing else.
// There is no user parameter anywhere here, which is what makes "your pod" mean
// yours: ODE's Hub credential could address any user's server, and the only
// thing that stops a request doing so is that no route offers a way to say which.
//
// Execution is not here. It streams, and a cell outlives an HTTP request as
// readily as a profile does, so it lives on the WebSocket beside the profiler
// operations (ws_kernel.go).

// @Summary		The caller's kernel status
// @Description	Resolved from the caller's own token: no route here takes a user
// @Description	parameter, which is what makes "your pod" mean yours (§5.6).
// @Tags			kernel
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	kernel.Status
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the Hub or the singleuser server could not be reached"
// @Router			/kernel [get]
func handleKernelStatus(svc *kernel.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		status, err := svc.Status(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondKernel(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// handleKernelEnsure is the pre-warm of §5.6: spawn the pod, start a kernel,
// install the platform token. Called when the developer opens the pane, so that
// a cold start of up to a minute happens while they are still reading rather
// than after they press run.
//
// @Summary		Spawn the pod and start a kernel
// @Description	The pre-warm of §5.6: spawn the pod, start a kernel, install the
// @Description	platform token. Called when the developer opens the pane so that a cold
// @Description	start of up to a minute happens while they are still reading.
// @Tags			kernel
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	kernel.Status
// @Success		202	{object}	map[string]string	"the pod is still coming up; keep polling rather than treating this as a failure"
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the Hub or the singleuser server could not be reached"
// @Router			/kernel [post]
func handleKernelEnsure(svc *kernel.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		status, err := svc.Ensure(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondKernel(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// @Summary		Restart the kernel
// @Description	Discards the kernel's state. The pod and the workspace survive.
// @Tags			kernel
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	kernel.Status
// @Success		202	{object}	map[string]string	"the pod is still coming up"
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the Hub or the singleuser server could not be reached"
// @Router			/kernel/restart [post]
func handleKernelRestart(svc *kernel.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		status, err := svc.Restart(c.Request.Context(), auth.Bearer(c))
		if err != nil {
			respondKernel(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// @Summary		Interrupt the running cell
// @Description	Interrupts rather than abandons, so the kernel stays usable and its
// @Description	state survives.
// @Tags			kernel
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]bool
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the Hub or the singleuser server could not be reached"
// @Router			/kernel/interrupt [post]
func handleKernelInterrupt(svc *kernel.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.InterruptUser(c.Request.Context(), auth.Bearer(c)); err != nil {
			respondKernel(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"interrupted": true})
	}
}

// handleKernelShutdown ends the kernel. It does not stop the pod: the pod is the
// developer's, their files are on it, and the cluster's idle culling is what
// reclaims it.
//
// @Summary		Shut the kernel down
// @Description	Ends the kernel but not the pod: the pod is the developer's, their
// @Description	files are on it, and the cluster's idle culling is what reclaims it.
// @Tags			kernel
// @Produce		json
// @Security		Bearer
// @Success		200	{object}	map[string]bool
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the Hub or the singleuser server could not be reached"
// @Router			/kernel [delete]
func handleKernelShutdown(svc *kernel.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := svc.ShutdownUser(c.Request.Context(), auth.Bearer(c)); err != nil {
			respondKernel(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"shutdown": true})
	}
}

// handleKernelFiles lists the persistent workspace.
//
// This is the visible half of M4's acceptance criterion, not the Code pane of
// §5.11: read-only, one directory at a time, no editing. The full tree with
// write access on every file is M7.
//
// @Summary		List the persistent workspace
// @Description	Read-only, one directory at a time. This is the visible half of M4's
// @Description	acceptance criterion — that a file written in one session is present in
// @Description	the next — not the Code pane of §5.11, which is M7.
// @Tags			kernel
// @Produce		json
// @Security		Bearer
// @Param			path	query		string	false	"directory relative to the workspace root; absent lists the root"
// @Success		200		{object}	map[string]interface{}
// @Failure		401	{object}	map[string]string
// @Failure		502	{object}	map[string]string	"the Hub or the singleuser server could not be reached"
// @Router			/kernel/files [get]
func handleKernelFiles(svc *kernel.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		entries, err := svc.Files(c.Request.Context(), auth.Bearer(c), c.Query("path"))
		if err != nil {
			respondKernel(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"workspace": svc.Workspace(),
			"path":      c.Query("path"),
			"entries":   entries,
		})
	}
}

// respondKernel maps the kernel domain errors onto status codes.
//
// A spawn timeout is deliberately 202 rather than an error: the pod is still
// coming up, the request was not wrong, and the SPA's answer is to keep polling
// rather than to show a failure.
func respondKernel(c *gin.Context, err error) {
	switch status := statusForKernelError(err); status {
	case http.StatusAccepted:
		c.JSON(status, gin.H{
			"error":   err.Error(),
			"pending": "spawn",
			"hint":    "the pod is still starting; a cold start takes up to a minute",
		})
	case 0:
		respondUpstream(c, err)
	default:
		c.JSON(status, gin.H{"error": err.Error()})
	}
}

func statusForKernelError(err error) int {
	var upstream *kernel.UpstreamError
	switch {
	case errors.Is(err, kernel.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, kernel.ErrNoKernel):
		return http.StatusNotFound
	case errors.Is(err, kernel.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, kernel.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, kernel.ErrSpawnTimeout):
		return http.StatusAccepted
	case errors.As(err, &upstream):
		switch upstream.Code {
		case http.StatusForbidden, http.StatusNotFound:
			return upstream.Code
		default:
			return http.StatusBadGateway
		}
	default:
		return 0
	}
}
