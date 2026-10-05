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
	"sort"
	"strings"

	drmodel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/gin-gonic/gin"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/auth"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/devices"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/experiments"
)

// resolveInputTopicBody is a launch_experiment input topic, and optionally the
// device to move it to. Without device_id the route describes the topic as
// proposed; with one it retargets.
//
// ServiceID fixes the service a retarget lands on, instead of letting mapping 0
// derive it (RetargetToService). It is meaningless without a device to retarget
// to, so a body carrying it without device_id is rejected as a client error
// rather than silently ignored.
type resolveInputTopicBody struct {
	Topic     experiments.InputTopic `json:"topic"`
	DeviceID  string                 `json:"device_id"`
	ServiceID string                 `json:"service_id"`
}

// handleResolveInputTopic describes an input topic in the names a person reads,
// or retargets it to another device — the derivation the launch_experiment
// confirmation card needs so a developer can move a topic to another device
// without retyping its topic name or mapping sources by hand. The rules it
// applies, and why a mapping source cannot simply be carried across, are in
// docs/experiments.md.
//
// @Summary		Describe or retarget one launch_experiment input topic
// @Description	Without device_id, describes the topic against the device it
// @Description	already names. With device_id, rewrites the topic — its name,
// @Description	filterValue and every mapping source — for that device, deriving
// @Description	the counterpart variable from the target device type rather than
// @Description	assuming a name or a path survives a device change. With
// @Description	service_id also given, the target service is fixed rather than
// @Description	derived from mapping 0, and a mapping with no counterpart there is
// @Description	guessed instead of refused (RetargetToService) — every guessed
// @Description	mapping comes back with guessed=true and a warning naming it.
// @Description	service_id without device_id is a client error. Reads the topic's
// @Description	own device under Execute (§5.1: this route decides which history a
// @Description	run will read), and with device_id present, the target device the
// @Description	same way.
// @Tags			experiments
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		resolveInputTopicBody	true	"the topic, and optionally the device (and service) to move it to"
// @Success		200		{object}	experiments.Resolved
// @Failure		400		{object}	map[string]string	"a malformed topic, one that cannot be derived, or service_id without device_id"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"the platform refused this user a device named by the topic"
// @Failure		404		{object}	map[string]string
// @Router			/input-topics/resolve [post]
func handleResolveInputTopic(deviceService *devices.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body resolveInputTopicBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		// Checked before either device is read: a topic missing what identifies
		// its device should not cost a round trip to find that out.
		if err := experiments.ValidateResolvableTopic(body.Topic); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		deviceID := strings.TrimSpace(body.DeviceID)
		serviceID := strings.TrimSpace(body.ServiceID)
		// service_id only means anything against a device being retargeted to; on
		// its own it names a service on a device type the request never gives.
		if deviceID == "" && serviceID != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "service_id requires device_id"})
			return
		}

		token := auth.Bearer(c)
		// Execute, not Read: this route decides which history a run will read, so
		// it claims the permission that reading data needs (§5.1), the same as
		// handleCreateProfiles does for a device about to be profiled.
		from, err := deviceService.Get(token, body.Topic.FilterValue, drmodel.EXECUTE)
		if err != nil {
			respondUpstream(c, err)
			return
		}

		var resolved experiments.Resolved
		if deviceID == "" {
			resolved, err = experiments.Describe(body.Topic, from)
		} else {
			var to models.ExtendedDevice
			to, err = deviceService.Get(token, deviceID, drmodel.EXECUTE)
			if err != nil {
				respondUpstream(c, err)
				return
			}
			resolved, err = experiments.RetargetToService(body.Topic, from, to, serviceID)
		}
		if err != nil {
			if errors.Is(err, experiments.ErrInvalidRequest) {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			respondUpstream(c, err)
			return
		}
		c.JSON(http.StatusOK, resolved)
	}
}

// inputTopicCandidatesBody is a search over every device a topic could move to,
// each one carrying the fit the backend already computed against it — so the
// picker can put the closest matches first instead of downloading a page of
// devices and refiltering it in the browser.
type inputTopicCandidatesBody struct {
	Topic  experiments.InputTopic `json:"topic"`
	Search string                 `json:"search"`
	// Limit is how many devices to list; zero means defaultCandidateDeviceLimit,
	// devices.MaxLimit is the ceiling.
	Limit  int64 `json:"limit"`
	Offset int64 `json:"offset"`
}

// inputTopicCandidate is one device the picker can move a topic to, with the
// fit already computed against it.
type inputTopicCandidate struct {
	Device experiments.ResolvedDevice `json:"device"`
	Fit    experiments.Fit            `json:"fit"`
}

// inputTopicCandidatesResponse mirrors devices.ListResult's paging fields so the
// picker can tell "no device matched this search" from "there are more beyond
// this page" the same way the /devices list already does.
type inputTopicCandidatesResponse struct {
	Candidates []inputTopicCandidate `json:"candidates"`
	Total      int64                 `json:"total"`
	Limit      int64                 `json:"limit"`
	Offset     int64                 `json:"offset"`
}

// candidateFitRank orders FitOf's three outcomes for the response: a path
// match first, a semantic match next, no match last.
var candidateFitRank = map[experiments.FitMatch]int{
	experiments.FitPath:      0,
	experiments.FitSemantics: 1,
	experiments.FitNone:      2,
}

// handleInputTopicCandidates lists every device the caller may execute against,
// each with the same fit Retarget's own mapping-0 selection already applies
// (experiments.FitOf) — the replacement for the DevicePicker's former
// quick_profiles call, which fetched ten devices and refiltered them in the
// browser on an exact function-and-aspect match: a device beyond the ten never
// appeared, and a device matching by path but not by function and aspect (or
// the reverse) was dropped even though Retarget itself would have accepted it.
//
// It reads no series and no availability: FitOf is a walk over two device
// types, so this route pays for exactly one device-repository call regardless
// of how many devices it returns.
//
// @Summary		Devices a topic could move to, with the fit already computed
// @Description	Every device this caller may execute (§5.1), sorted path match
// @Description	first, then function-and-aspect match, then no match, and by
// @Description	display name within a group. Reads no series and no
// @Description	availability: the fit is the same device-type walk Retarget
// @Description	applies to mapping 0 (experiments.FitOf), so a developer can see
// @Description	every device instead of the first ten and choose one with no
// @Description	match to pick a service and variable by hand.
// @Tags			experiments
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		inputTopicCandidatesBody	true	"the topic to move, and a device search"
// @Success		200		{object}	inputTopicCandidatesResponse
// @Failure		400		{object}	map[string]string	"a malformed topic, or a negative limit or offset"
// @Failure		401		{object}	map[string]string
// @Failure		403		{object}	map[string]string	"the platform refused this user the topic's own device"
// @Failure		404		{object}	map[string]string
// @Router			/input-topics/candidates [post]
func handleInputTopicCandidates(deviceService *devices.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body inputTopicCandidatesBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		if err := experiments.ValidateResolvableTopic(body.Topic); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if body.Limit < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a non-negative integer"})
			return
		}
		if body.Offset < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be a non-negative integer"})
			return
		}

		token := auth.Bearer(c)
		// The topic's own device, read the same way /resolve does, so FitOf's
		// origin side is read under the same claim regardless of which route asked.
		from, err := deviceService.Get(token, body.Topic.FilterValue, drmodel.EXECUTE)
		if err != nil {
			respondUpstream(c, err)
			return
		}

		limit := body.Limit
		if limit == 0 {
			limit = defaultCandidateDeviceLimit
		}
		if limit > devices.MaxLimit {
			limit = devices.MaxLimit
		}

		listed, err := deviceService.List(token, drmodel.ExtendedDeviceListOptions{
			Search: strings.TrimSpace(body.Search),
			Limit:  limit,
			Offset: body.Offset,
			// models.Execute, not models.Read: a candidate ODE cannot read data from
			// is not a candidate a developer can retarget onto (§5.1).
			Permission: models.Execute,
			// FitOf reads the device type, not just the device: candidates need it
			// the same way the fixed-service listing does (runQuickProfiles).
			FullDt: true,
		})
		if err != nil {
			respondUpstream(c, err)
			return
		}

		candidates := make([]inputTopicCandidate, 0, len(listed.Devices))
		for _, device := range listed.Devices {
			candidates = append(candidates, inputTopicCandidate{
				Device: experiments.ResolvedDevice{
					ID:             device.Id,
					Name:           devices.DisplayName(device),
					DeviceTypeID:   device.DeviceTypeId,
					DeviceTypeName: devices.TypeName(device),
				},
				Fit: experiments.FitOf(body.Topic, from, device),
			})
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			ri, rj := candidateFitRank[candidates[i].Fit.Match], candidateFitRank[candidates[j].Fit.Match]
			if ri != rj {
				return ri < rj
			}
			return candidates[i].Device.Name < candidates[j].Device.Name
		})

		c.JSON(http.StatusOK, inputTopicCandidatesResponse{
			Candidates: candidates,
			Total:      listed.Total,
			Limit:      limit,
			Offset:     body.Offset,
		})
	}
}
