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
type resolveInputTopicBody struct {
	Topic    experiments.InputTopic `json:"topic"`
	DeviceID string                 `json:"device_id"`
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
// @Description	assuming a name or a path survives a device change. Reads the
// @Description	topic's own device under Execute (§5.1: this route decides which
// @Description	history a run will read), and with device_id present, the target
// @Description	device the same way.
// @Tags			experiments
// @Accept			json
// @Produce		json
// @Security		Bearer
// @Param			request	body		resolveInputTopicBody	true	"the topic, and optionally the device to move it to"
// @Success		200		{object}	experiments.Resolved
// @Failure		400		{object}	map[string]string	"a malformed topic, or one that cannot be derived"
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

		token := auth.Bearer(c)
		// Execute, not Read: this route decides which history a run will read, so
		// it claims the permission that reading data needs (§5.1), the same as
		// handleCreateProfiles does for a device about to be profiled.
		from, err := deviceService.Get(token, body.Topic.FilterValue, drmodel.EXECUTE)
		if err != nil {
			respondUpstream(c, err)
			return
		}

		deviceID := strings.TrimSpace(body.DeviceID)
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
			resolved, err = experiments.Retarget(body.Topic, from, to)
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
