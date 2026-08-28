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

package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/kernel"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation"
	"github.com/SENERGY-Platform/operator-development-environment/pkg/simulation/templates"
)

// The simulation tools: MOSES as ODE's source of test scenarios.
//
// What they are for is one problem. An operator needs data before it can be
// developed, and the data an operator needs is rarely there on the day the work
// starts — a forecast wants weeks of history, a cycle detector wants a machine
// that actually cycles, a submetering operator wants a meter tree with something
// under it. MOSES simulates the site instead, and because a simulated asset is an
// ordinary platform device, everything else in this surface then finds it: the
// ontology tools, the profiler, the exploration pane.
//
// Three rules run through the file and are worth reading once rather than at each
// call site.
//
//   - **Every write is a read, a change and a write back, carrying the version.**
//     MOSES stores the whole document and refuses a write against a version that
//     has moved on. A conflict is reported to the model as a refusal that says to
//     read again — never retried here, because the change was described against a
//     document that no longer exists.
//   - **The model does not author the document.** It picks a template and fills in
//     what the template asks for, or it changes one channel. Three hundred lines of
//     nested JSON validated by a service the model cannot see is a guessing game;
//     a template makes the failure mode "the template lacks a knob".
//   - **A delete reaches only what this session created**, through the same
//     Creations record and the same rule as delete_export and delete_import_instance.

// ---- the backfill precondition, checked before a device exists ----

// timePathVerdicts reads the whole device types behind a MOSES catalogue and
// reports, per service, whether a channel on it could ever be backfilled.
//
// It is here rather than in pkg/simulation's client because the answer needs two
// services at once: MOSES says which device types can be simulated at all, and
// only the device repository carries the `senergy/time_path` attribute that
// decides the backfill. MOSES's own `/device-types` projection drops it.
//
// **Why before the device is created.** The attribute is optional and unset on
// most device types, because it only matters for a publisher that wants to write
// history — nobody sets it for a device reporting the present. A simulation is
// usually built precisely because an operator needs weeks of history on the day
// the work starts, so the one property that decides whether it can have any is
// also the one nobody thought about. Reporting it while the model is *choosing*
// the service is the only point at which the answer is free; after that it costs
// a device in somebody's repository and two confirmations.
//
// A failure to read is not a verdict. It answers BackfillUnknown per service,
// with the reason, and never "no": a device repository that did not answer says
// nothing about a device type.
func (s *surface) timePathVerdicts(
	ctx context.Context, req Request, catalogue []simulation.DeviceType,
) map[string]map[string]simulation.TimePathVerdict {
	out := map[string]map[string]simulation.TimePathVerdict{}
	unknown := func(reason string) map[string]map[string]simulation.TimePathVerdict {
		for _, deviceType := range catalogue {
			out[deviceType.ID] = map[string]simulation.TimePathVerdict{}
			for _, service := range deviceType.Services {
				out[deviceType.ID][service.ID] = simulation.TimePathVerdict{
					Backfillable: simulation.BackfillUnknown, Reason: reason,
				}
			}
		}
		return out
	}
	if s.deps.Ontology == nil {
		return unknown("this deployment has no device repository configured, so whether a " +
			"service can carry a historical timestamp cannot be established here")
	}

	ids := make([]string, 0, len(catalogue))
	for _, deviceType := range catalogue {
		ids = append(ids, deviceType.ID)
	}
	full, err := s.deps.Ontology.DeviceTypesByID(ctx, req.Token, ids)
	if err != nil {
		slog.WarnContext(ctx, "could not read device types for the backfill precondition",
			"error", err)
		return unknown("the device repository did not answer, so whether a service can carry " +
			"a historical timestamp is unknown rather than no")
	}

	for _, deviceType := range catalogue {
		out[deviceType.ID] = map[string]simulation.TimePathVerdict{}
		resolved, found := full[deviceType.ID]
		for _, service := range deviceType.Services {
			if !found {
				out[deviceType.ID][service.ID] = simulation.TimePathVerdict{
					Backfillable: simulation.BackfillUnknown,
					Reason: "the device repository did not return this device type, although the " +
						"simulator offers it",
				}
				continue
			}
			full, hasService := serviceOfModel(resolved, service.ID)
			if !hasService {
				out[deviceType.ID][service.ID] = simulation.TimePathVerdict{
					Backfillable: simulation.BackfillUnknown,
					Reason: "the device repository's copy of this device type has no such service, " +
						"which is a disagreement between two services rather than a fact about the " +
						"service",
				}
				continue
			}
			out[deviceType.ID][service.ID] = simulation.CheckTimePath(full)
		}
	}
	return out
}

func serviceOfModel(deviceType models.DeviceType, id string) (models.Service, bool) {
	for _, service := range deviceType.Services {
		if service.Id == id {
			return service, true
		}
	}
	return models.Service{}, false
}

// backfillReport is what could be established about a document's channels.
//
// Three counts rather than a list of problems, because the empty list is
// ambiguous and the ambiguity is dangerous: no blocked channel because every one
// is fine, and no blocked channel because nothing was checked, would otherwise
// produce the same silence — and the second one read as the first is a false
// reassurance a developer acts on.
type backfillReport struct {
	Possible   int
	Blocked    []string
	Unknown    int
	NotChecked int
}

// Checked is how many channels the report actually says something about.
func (r backfillReport) Checked() int { return r.Possible + len(r.Blocked) + r.Unknown }

// Warnings is what to hand the developer, or nothing when every channel is as
// good as ODE can tell.
//
// Returned as warnings rather than as a refusal, and that is the whole judgement
// here. A simulation nobody will backfill is a perfectly good scenario — a live
// one, for an operator being watched as it runs — so refusing to build it would
// be ODE deciding what the developer's case is. What ODE can do is make sure
// nobody finds out afterwards.
func (r backfillReport) Warnings() []string {
	warnings := []string{}
	if len(r.Blocked) > 0 {
		warnings = append(warnings, "**These channels cannot be backfilled**, so this simulation "+
			"will only ever have the history it accumulates from now on. Making one backfillable "+
			"means adding the "+simulation.TimePathAttribute+" attribute to the *device type* in "+
			"the device repository, which is shared inventory and a modelling decision for "+
			"whoever owns that type — neither ODE nor the simulator changes it. Say this to the "+
			"developer rather than proposing a backfill that would skip everything: "+
			strings.Join(r.Blocked, "; "))
	}
	if r.Unknown > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d channel(s) could not be checked against the device repository, so whether they "+
				"can be backfilled is unknown rather than settled either way.", r.Unknown))
	}
	if r.NotChecked > 0 && r.Checked() == 0 {
		warnings = append(warnings, "Whether these channels can carry a historical timestamp was "+
			"not established at all: none of their device types is in the simulator's catalogue "+
			"here. Do not read the absence of a warning as a working backfill.")
	}
	return warnings
}

// backfillChannels reports, per channel of a document, what the verdicts say —
// looked up by the device type and service each publishes through.
func backfillChannels(
	environment simulation.Environment,
	verdicts map[string]map[string]simulation.TimePathVerdict,
) backfillReport {
	report := backfillReport{Blocked: []string{}}
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		for _, channel := range asset.Channels {
			verdict, known := verdicts[asset.ExternalTypeId][channel.ExternalRef]
			if !known {
				report.NotChecked++
				continue
			}
			switch verdict.Backfillable {
			case simulation.BackfillImpossible:
				report.Blocked = append(report.Blocked, fmt.Sprintf("%s / %s: %s",
					asset.Name, channel.Name, verdict.Reason))
			case simulation.BackfillUnknown:
				report.Unknown++
			default:
				report.Possible++
			}
		}
	})
	return report
}

// ---- list_simulations (L0) ----

type listSimulationsInput struct {
	Search string `json:"search"`
}

func (s *surface) listSimulations(ctx context.Context, req Request) (any, error) {
	var in listSimulationsInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	environments, err := s.deps.Simulation.List(ctx, req.Token)
	if err != nil {
		return nil, simulationError(err)
	}

	needle := strings.ToLower(strings.TrimSpace(in.Search))
	listed := []map[string]any{}
	for _, environment := range environments {
		if needle != "" && !strings.Contains(strings.ToLower(environment.Name), needle) {
			continue
		}
		zones, assets, channels := countOf(environment)
		listed = append(listed, map[string]any{
			"simulation_id": environment.ID,
			"name":          environment.Name,
			"type":          environment.Type,
			"version":       environment.Version,
			"zones":         zones,
			"assets":        assets,
			"channels":      channels,
		})
	}

	return map[string]any{
		"simulations": listed,
		"note": "Structure only, no measurements. Every asset of a simulation is an ordinary " +
			"platform device, so what it publishes is found through resolve_semantic_selection " +
			"and profiled like anything else — a simulated series is not a second kind of data.",
	}, nil
}

// ---- get_simulation (L0) ----

type getSimulationInput struct {
	SimulationID string `json:"simulation_id"`
}

func (s *surface) getSimulation(ctx context.Context, req Request) (any, error) {
	var in getSimulationInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	environment, err := s.simulationOf(ctx, req, in.SimulationID)
	if err != nil {
		return nil, err
	}
	return simulationView(environment), nil
}

// simulationView is the document as a model reads it.
//
// Projected rather than handed over whole, for the reason every projection in
// this surface exists: the stored document carries a value per state key and a
// factor per hour of every profile, and none of that is what a model deciding
// what to change needs. What it needs is the tree, what each channel publishes
// and what drives it.
func simulationView(environment simulation.Environment) map[string]any {
	view := map[string]any{
		"simulation_id": environment.ID,
		"name":          environment.Name,
		"type":          environment.Type,
		// Carried because it is what the next write has to send back, and a model that
		// changes something has to hand it to the tool that writes.
		"version":      environment.Version,
		"seed":         environment.Seed,
		"context_keys": sortedKeys(environment.Context),
		"zones":        zoneViews(environment.Zones),
	}
	driven := sortedKeys(mapOfSources(environment.ContextSources))
	if len(driven) > 0 {
		view["driven_context_keys"] = driven
	}
	if environment.UnknownField != "" {
		view["read_only"] = true
		view["read_only_reason"] = fmt.Sprintf(
			"this simulation carries the field %q, which this ODE does not know. Every write to "+
				"MOSES is the whole document, so editing it from here would delete that field. "+
				"Read it, profile what it publishes, and make changes to it in the MOSES UI",
			environment.UnknownField)
	}
	return view
}

func zoneViews(zones []simulation.Zone) []map[string]any {
	out := []map[string]any{}
	for _, zone := range zones {
		view := map[string]any{
			"zone_id": zone.ID,
			"name":    zone.Name,
			"type":    zone.Type,
		}
		if len(zone.Tags) > 0 {
			view["tags"] = zone.Tags
		}
		if len(zone.Zones) > 0 {
			view["zones"] = zoneViews(zone.Zones)
		}
		if len(zone.Assets) > 0 {
			assets := []map[string]any{}
			for _, asset := range zone.Assets {
				assets = append(assets, assetView(asset))
			}
			view["assets"] = assets
		}
		out = append(out, view)
	}
	return out
}

func assetView(asset simulation.Asset) map[string]any {
	channels := []map[string]any{}
	for _, channel := range asset.Channels {
		view := map[string]any{
			"channel_id": channel.ID,
			"name":       channel.Name,
			"direction":  channel.Direction,
			// The platform service this publishes to. Together with the asset's device it
			// is the {device_id, service_id} half of an addressable series, which is what
			// makes a simulated channel findable by every other tool here.
			"service_id":        channel.ExternalRef,
			"characteristic_id": channel.CharacteristicId,
			"source":            string(channel.Source.Kind),
		}
		if channel.Unit != "" {
			view["unit"] = channel.Unit
		}
		if channel.IntervalSeconds > 0 {
			view["interval_seconds"] = channel.IntervalSeconds
		}
		if detail := sourceDetail(channel.Source); detail != nil {
			view["source_detail"] = detail
		}
		channels = append(channels, view)
	}
	view := map[string]any{
		"asset_id": asset.ID,
		"name":     asset.Name,
		"kind":     asset.Kind,
		// The platform device. This is the id every other tool in this surface takes.
		"device_id": asset.ExternalRef,
		"channels":  channels,
	}
	if asset.ExternalTypeId != "" {
		view["device_type_id"] = asset.ExternalTypeId
	}
	if asset.SubmeteredBy != "" {
		view["submetered_by"] = asset.SubmeteredBy
	}
	if !asset.ExternalManaged && asset.ExternalRef != "" {
		// Worth saying, because it decides what a delete destroys: a device the
		// developer picked and attached outlives the simulation, with its timeseries.
		view["device_is_the_developers_own"] = true
	}
	return view
}

// sourceDetail is the part of a source worth a model's context: enough to decide
// whether to change it, not the whole configuration.
func sourceDetail(source simulation.Source) map[string]any {
	switch source.Kind {
	case simulation.SourceProfile:
		if source.Profile == nil {
			return nil
		}
		detail := map[string]any{
			"base":           source.Profile.Base,
			"spread_percent": source.Profile.SpreadPercent,
			"cumulative":     source.Profile.Cumulative,
		}
		// A profile with no hour factors runs at its base around the clock, which is a
		// legal document and the opposite of what an empty active_hours would read as.
		// Reporting "shaped by neither the hour nor the weekday" is the honest form.
		if len(source.Profile.HourFactors) == 0 && len(source.Profile.WeekdayFactors) == 0 {
			detail["shape"] = "none: the same base at every hour of every day"
			return detail
		}
		if len(source.Profile.HourFactors) > 0 {
			detail["active_hours"] = activeHours(source.Profile.HourFactors)
		}
		if len(source.Profile.WeekdayFactors) > 0 {
			detail["weekday_factors"] = source.Profile.WeekdayFactors
		}
		return detail
	case simulation.SourceDataset:
		if source.Dataset == nil {
			return nil
		}
		detail := map[string]any{
			"origin": source.Dataset.Origin,
			"ref":    source.Dataset.Ref,
			"anchor": source.Dataset.Anchor,
		}
		if source.Dataset.Column != "" {
			detail["column"] = source.Dataset.Column
		}
		if source.Dataset.Window != "" {
			detail["window"] = source.Dataset.Window
		}
		return detail
	case simulation.SourceFormula:
		if source.Formula == nil {
			return nil
		}
		return map[string]any{
			"expression": source.Formula.Expression,
			"inputs":     source.Formula.Inputs,
		}
	case simulation.SourceAggregate:
		return map[string]any{
			"note": "the sum over every asset sub-metered by this one, on the channel carrying " +
				"the same characteristic. It has no configuration: the meter tree is the " +
				"configuration, so an asset added below is summed without changing anything here",
		}
	}
	return nil
}

// activeHours reports which hours a profile actually produces something in,
// because a list of 24 factors costs more context than it is worth and the
// question a reader has is when the thing runs.
//
// Only called where there are factors. An empty list means "no shaping" rather
// than "never runs", and the two are opposites — see sourceDetail.
func activeHours(factors []float64) []int {
	out := []int{}
	for hour, factor := range factors {
		if factor > 0 {
			out = append(out, hour)
		}
	}
	return out
}

// ---- list_simulation_templates (L0) ----

// A read tool that exists because of what create_simulation deliberately does not
// do. The model picks a template and fills in what the template asks for, which is
// only workable if the model can read what the template asks for; the alternative
// is every role, parameter and note of every template sitting in the system prompt
// of every session that will never create a simulation.
func (s *surface) listSimulationTemplates(_ context.Context, _ Request) (any, error) {
	listed := []map[string]any{}
	for _, template := range templates.All() {
		assets := []map[string]any{}
		for _, role := range template.Assets() {
			channels := []map[string]any{}
			for _, channel := range role.Channels {
				channels = append(channels, map[string]any{
					"channel_role": channel.Name,
					"purpose":      channel.Purpose,
					"required":     channel.Required,
					"direction":    channel.Direction,
				})
			}
			assets = append(assets, map[string]any{
				"asset_role": role.Name,
				"kind":       role.Kind,
				"purpose":    role.Purpose,
				"required":   role.Required,
				"repeated":   role.Repeated,
				"channels":   channels,
			})
		}
		parameters := []map[string]any{}
		for _, spec := range template.Params() {
			parameter := map[string]any{
				"name":        spec.Name,
				"description": spec.Description,
				"default":     spec.Default,
				"min":         spec.Min,
				"max":         spec.Max,
			}
			if spec.Integral {
				parameter["integral"] = true
			}
			parameters = append(parameters, parameter)
		}
		listed = append(listed, map[string]any{
			"template":    template.Name(),
			"summary":     template.Summary(),
			"notes":       template.Notes(),
			"asset_roles": assets,
			"parameters":  parameters,
		})
	}
	return map[string]any{
		"templates": listed,
		"note": "Bind every required asset role to a device type from " +
			"list_simulation_device_types, and every required channel role to one of that " +
			"type's services. The characteristic and the unit are copied from the service you " +
			"bind, never invented — which is why the binding is what you choose and the " +
			"characteristic is not.",
	}, nil
}

// ---- list_simulation_device_types (L0) ----

func (s *surface) listSimulationDeviceTypes(ctx context.Context, req Request) (any, error) {
	deviceTypes, err := s.deps.Simulation.DeviceTypes(ctx, req.Token)
	if err != nil {
		return nil, simulationError(err)
	}

	// The backfill precondition, resolved while the model is still choosing. See
	// timePathVerdicts: this is the only moment the answer costs nothing.
	req.Progress("checking", "reading the device types to see which services can carry a historical timestamp")
	verdicts := s.timePathVerdicts(ctx, req, deviceTypes)

	backfillable, blocked := 0, 0
	listed := []map[string]any{}
	for _, deviceType := range deviceTypes {
		services := []map[string]any{}
		for _, service := range deviceType.Services {
			view := map[string]any{
				"service_id":        service.ID,
				"name":              service.Name,
				"direction":         service.Direction,
				"characteristic_id": service.CharacteristicId,
				"value_path":        service.ValuePath,
			}
			if verdict, known := verdicts[deviceType.ID][service.ID]; known {
				view["backfillable"] = verdict.Backfillable
				if verdict.TimePath != "" {
					view["time_path"] = verdict.TimePath
				}
				if verdict.Reason != "" {
					view["backfill_reason"] = verdict.Reason
				}
				switch verdict.Backfillable {
				case simulation.BackfillPossible:
					backfillable++
				case simulation.BackfillImpossible:
					blocked++
				}
			}
			services = append(services, view)
		}
		listed = append(listed, map[string]any{
			"device_type_id": deviceType.ID,
			"name":           deviceType.Name,
			"services":       services,
		})
	}
	answer := map[string]any{
		"device_types": listed,
		"note": "Only the types publishing through the simulator's own protocol are here, which " +
			"is what makes a type simulatable at all. A characteristic_id decides the unit of " +
			"everything a channel on that service publishes: pick the service that describes " +
			"the quantity you mean, and never carry a characteristic from one service to another.",
		"backfill_note": "`backfillable` says whether a channel on that service could ever hold " +
			"a historical timestamp. **If the developer needs history — which is usually why a " +
			"simulation is being built at all — prefer a service that says `possible`.** It is " +
			"decided by the " + simulation.TimePathAttribute + " attribute on the device type, " +
			"which is optional on this platform and unset on most types. " + simulation.BackfillCaveat,
	}
	if blocked > 0 {
		answer["backfill_summary"] = fmt.Sprintf(
			"%d service(s) can carry a historical timestamp, %d cannot.", backfillable, blocked)
	}
	if backfillable == 0 && blocked > 0 {
		answer["backfill_summary"] = "**No service on this platform's simulatable device types " +
			"can carry a historical timestamp.** A simulation built here publishes from now on " +
			"and cannot be backfilled at all. If the developer needs history, the way to it is " +
			"for whoever owns a device type to add the " + simulation.TimePathAttribute +
			" attribute to it in the device repository — say so rather than building a scenario " +
			"whose backfill will skip every channel."
	}
	if len(listed) == 0 {
		answer["note"] = "This platform has no device type the simulator can publish through, so " +
			"no simulation can be built here. That is a modelling gap in the device repository " +
			"— somebody has to declare a device type on the simulator's protocol — and not " +
			"something to work around. Say so rather than trying another route."
	}
	return answer, nil
}

// ---- get_simulation_state (L1) ----

// L1 for the reason every read in this surface sits where it does: these are
// actual values. Everything else about a simulation is structure.

type getSimulationStateInput struct {
	SimulationID string `json:"simulation_id"`
}

func (s *surface) getSimulationState(ctx context.Context, req Request) (any, error) {
	var in getSimulationStateInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.SimulationID) == "" {
		return nil, fmt.Errorf("%w: simulation_id is required", ErrInvalidInput)
	}
	state, err := s.deps.Simulation.State(ctx, req.Token, in.SimulationID)
	if err != nil {
		return nil, simulationError(err)
	}
	if !state.Running {
		return map[string]any{
			"simulation_id": in.SimulationID,
			"running":       false,
			"note": "This simulation is stored but not running on the MOSES instance that was " +
				"asked, so it holds no live state. That is the normal condition of a document " +
				"that was just written and of one another instance runs — it is not a failure " +
				"and not an empty simulation. What it publishes over time is read through the " +
				"ordinary series tools, not here.",
		}, nil
	}
	return map[string]any{
		"simulation_id": in.SimulationID,
		"running":       true,
		"as_of":         state.AsOf.UTC().Format(time.RFC3339),
		"context":       state.Context,
		"zones":         state.Zones,
		"assets":        state.Assets,
		"note": "Live values, resolved to as_of. A zone value the definition gives a time " +
			"constant is on its way to its set point rather than at it, so the number means " +
			"nothing without that instant. These are not the definition and reading them stores " +
			"nothing.",
	}, nil
}

// ---- create_simulation (L0, confirmed) ----

type createSimulationInput struct {
	Template  string                       `json:"template"`
	Name      string                       `json:"name"`
	Seed      int64                        `json:"seed"`
	Rationale string                       `json:"rationale"`
	Params    map[string]float64           `json:"params"`
	Bindings  map[string]templateBindingIn `json:"bindings"`
}

type templateBindingIn struct {
	DeviceTypeID string            `json:"device_type_id"`
	Channels     map[string]string `json:"channels"`
}

func (s *surface) createSimulation(ctx context.Context, req Request) (any, error) {
	var in createSimulationInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	template, found := templates.Lookup(strings.TrimSpace(in.Template))
	if !found {
		return nil, fmt.Errorf("%w: %q is not a template; this ODE has %v, and "+
			"list_simulation_templates says what each takes",
			ErrInvalidInput, in.Template, templates.Names())
	}

	req.Progress("catalogue", "reading the device types this simulator can publish through")
	catalogue, err := s.deps.Simulation.DeviceTypes(ctx, req.Token)
	if err != nil {
		return nil, simulationError(err)
	}

	bindings := map[string]templates.Binding{}
	for role, binding := range in.Bindings {
		bindings[role] = templates.Binding{
			DeviceTypeID: binding.DeviceTypeID,
			Channels:     binding.Channels,
		}
	}
	environment, err := template.Render(templates.Input{
		Name:      in.Name,
		Seed:      in.Seed,
		Bindings:  bindings,
		Params:    in.Params,
		Catalogue: catalogue,
	})
	if err != nil {
		return nil, invalidIfSimulationRequest(err)
	}

	// Checked against the rendered document, before anything is stored: these are the
	// bindings the developer is about to confirm, and this is the last moment the
	// answer is free of a device in somebody's repository.
	req.Progress("checking", "checking whether the bound services can carry a historical timestamp")
	blockedBackfill := backfillChannels(environment, s.timePathVerdicts(ctx, req, catalogue)).Warnings()

	req.Progress("creating", "storing the environment; MOSES registers a platform device per asset")
	created, err := s.deps.Simulation.Create(ctx, req.Token, environment)
	if err != nil {
		return nil, simulationError(err)
	}

	answer := simulationView(created)
	answer["template"] = template.Name()
	answer["rationale"] = in.Rationale
	answer["template_notes"] = template.Notes()
	answer["warnings"] = append([]string{
		"Every asset above is now a device in the device repository, visible to every " +
			"application that reads it. It is inventory until somebody removes it.",
		"A simulation publishes from now on. It has no past: use backfill_simulation to " +
			"reconstruct a window that has already gone by, or the series will be as young as " +
			"this call.",
	}, blockedBackfill...)
	answer["next"] = "The devices are addressable now. resolve_semantic_selection and " +
		"quick_profile find them like any others — profile one before trusting what it " +
		"publishes, rather than assuming the scenario came out the way it was described."
	if note := s.recordCreation(ctx, req, Creation{
		Kind: CreatedSimulation,
		ID:   created.ID,
		Name: created.Name,
		Tool: "create_simulation",
	}); note != "" {
		answer["warnings"] = append(answer["warnings"].([]string), note)
	}
	return answer, nil
}

// ---- add_simulated_asset (L0, confirmed) ----

type addSimulatedAssetInput struct {
	SimulationID string `json:"simulation_id"`
	ZoneID       string `json:"zone_id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	DeviceTypeID string `json:"device_type_id"`
	SubmeteredBy string `json:"submetered_by"`
	Rationale    string `json:"rationale"`
	Channels     []struct {
		Name            string   `json:"name"`
		ServiceID       string   `json:"service_id"`
		IntervalSeconds int64    `json:"interval_seconds"`
		Source          sourceIn `json:"source"`
	} `json:"channels"`
}

func (s *surface) addSimulatedAsset(ctx context.Context, req Request) (any, error) {
	var in addSimulatedAssetInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: the asset needs a name, which is what the device it "+
			"creates will be called", ErrInvalidInput)
	}
	if strings.TrimSpace(in.DeviceTypeID) == "" {
		return nil, fmt.Errorf("%w: device_type_id is required; list_simulation_device_types "+
			"reports what this platform has", ErrInvalidInput)
	}
	if len(in.Channels) == 0 {
		return nil, fmt.Errorf("%w: an asset with no channel publishes nothing, so it would be "+
			"a device that never sends", ErrInvalidInput)
	}
	kind, err := assetKindOf(in.Kind)
	if err != nil {
		return nil, err
	}

	req.Progress("reading", "reading the simulation, because a write carries the version it was read at")
	environment, err := s.simulationOf(ctx, req, in.SimulationID)
	if err != nil {
		return nil, err
	}
	zone, found := environment.FindZone(strings.TrimSpace(in.ZoneID))
	if !found {
		return nil, fmt.Errorf("%w: this simulation has no zone %q. Its zones are %v",
			ErrInvalidInput, in.ZoneID, zoneIDsOf(environment))
	}
	if in.SubmeteredBy != "" && !hasAsset(environment, in.SubmeteredBy) {
		return nil, fmt.Errorf("%w: submetered_by names %q, which is not an asset of this "+
			"simulation. It has to be the meter above this asset, and it has to be inside the "+
			"same top level zone", ErrInvalidInput, in.SubmeteredBy)
	}

	catalogue, err := s.deps.Simulation.DeviceTypes(ctx, req.Token)
	if err != nil {
		return nil, simulationError(err)
	}
	deviceType, known := deviceTypeOf(catalogue, in.DeviceTypeID)
	if !known {
		return nil, fmt.Errorf("%w: device type %q is not one this simulator can publish "+
			"through, which is what list_simulation_device_types lists",
			ErrInvalidInput, in.DeviceTypeID)
	}

	assetID := freshID(environment, "asset", in.Name)
	asset := simulation.Asset{
		ID:             assetID,
		Name:           strings.TrimSpace(in.Name),
		Kind:           kind,
		ExternalTypeId: in.DeviceTypeID,
		SubmeteredBy:   strings.TrimSpace(in.SubmeteredBy),
		InitialStates:  map[string]any{},
	}
	for index, channel := range in.Channels {
		service, serviceFound := serviceOf(deviceType, channel.ServiceID)
		if !serviceFound {
			return nil, fmt.Errorf("%w: service %q is not part of device type %q (%s)",
				ErrInvalidInput, channel.ServiceID, deviceType.Name, deviceType.ID)
		}
		source, err := buildSource(channel.Source)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(channel.Name)
		if name == "" {
			name = service.Name
		}
		interval := channel.IntervalSeconds
		if service.Direction == simulation.Actuator && interval != 0 {
			// Refused here rather than at MOSES, which answers it with a 400: an actuator
			// is driven from outside and an interval on one is a caller who thinks it is
			// a measurement.
			return nil, fmt.Errorf("%w: %q is an actuator and must not carry an interval; it "+
				"is driven from outside, through set_simulation_context", ErrInvalidInput, service.Name)
		}
		if service.Direction == simulation.Sensor && interval <= 0 {
			interval = defaultChannelInterval
		}
		asset.Channels = append(asset.Channels, simulation.Channel{
			ID:               fmt.Sprintf("%s-channel-%d", assetID, index+1),
			Name:             name,
			Direction:        service.Direction,
			ExternalRef:      service.ID,
			CharacteristicId: service.CharacteristicId,
			IntervalSeconds:  interval,
			Source:           source,
		})
	}

	zone.Assets = append(zone.Assets, asset)

	// The same check the create makes, over the one asset being added. Before the
	// write, for the same reason: after it there is a device.
	req.Progress("checking", "checking whether the bound services can carry a historical timestamp")
	blockedBackfill := backfillChannels(
		simulation.Environment{Zones: []simulation.Zone{{Assets: []simulation.Asset{asset}}}},
		s.timePathVerdicts(ctx, req, catalogue)).Warnings()

	req.Progress("writing", "storing the simulation; MOSES registers the platform device for the new asset")
	stored, err := s.deps.Simulation.Replace(ctx, req.Token, environment)
	if err != nil {
		return nil, simulationError(err)
	}

	answer := map[string]any{
		"simulation_id": stored.ID,
		"version":       stored.Version,
		"rationale":     in.Rationale,
		"added":         addedAssetView(stored, assetID),
		"zone_id":       zone.ID,
		"warnings": append([]string{
			"This asset is now a device in the device repository, visible to every application " +
				"that reads it.",
		}, blockedBackfill...),
	}
	if in.SubmeteredBy != "" {
		answer["note"] = "Sub-metered by " + in.SubmeteredBy + ". Any aggregate channel on that " +
			"asset now sums this one too, without being edited — the meter tree is the " +
			"configuration."
	}
	return answer, nil
}

// defaultChannelInterval is what a sensor channel publishes at when nothing says
// otherwise. A minute, matching the templates: it is a plausible sampling rate
// for a site and it is what decides how many rows a backfill writes.
const defaultChannelInterval = 60

func addedAssetView(environment simulation.Environment, assetID string) map[string]any {
	var view map[string]any
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.ID == assetID {
			view = assetView(*asset)
		}
	})
	return view
}

// ---- set_channel_source (L0, confirmed) ----

type setChannelSourceInput struct {
	SimulationID    string   `json:"simulation_id"`
	ChannelID       string   `json:"channel_id"`
	IntervalSeconds int64    `json:"interval_seconds"`
	Rationale       string   `json:"rationale"`
	Source          sourceIn `json:"source"`
}

func (s *surface) setChannelSource(ctx context.Context, req Request) (any, error) {
	var in setChannelSourceInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	if strings.TrimSpace(in.ChannelID) == "" {
		return nil, fmt.Errorf("%w: channel_id is required; get_simulation reports one per "+
			"channel", ErrInvalidInput)
	}
	source, err := buildSource(in.Source)
	if err != nil {
		return nil, err
	}

	req.Progress("reading", "reading the simulation, because a write carries the version it was read at")
	environment, err := s.simulationOf(ctx, req, in.SimulationID)
	if err != nil {
		return nil, err
	}
	asset, channel, found := environment.FindChannel(strings.TrimSpace(in.ChannelID))
	if !found {
		return nil, fmt.Errorf("%w: this simulation has no channel %q. get_simulation reports "+
			"the channels it has", ErrInvalidInput, in.ChannelID)
	}
	if channel.Direction == simulation.Actuator && in.IntervalSeconds != 0 {
		return nil, fmt.Errorf("%w: %q is an actuator and must not carry an interval",
			ErrInvalidInput, channel.Name)
	}
	if source.Kind == simulation.SourceAggregate && asset.SubmeteredBy != "" {
		// MOSES refuses this too, and refusing it here says why. An asset that is
		// sub-metered by something and also aggregates would be counting itself into
		// its own parent.
		return nil, fmt.Errorf("%w: %q is sub-metered by %q, so an aggregate on it would sum "+
			"a tree it is itself part of", ErrInvalidInput, asset.Name, asset.SubmeteredBy)
	}

	before := string(channel.Source.Kind)
	channel.Source = source
	if in.IntervalSeconds > 0 {
		channel.IntervalSeconds = in.IntervalSeconds
	}

	req.Progress("writing", "storing the simulation")
	stored, err := s.deps.Simulation.Replace(ctx, req.Token, environment)
	if err != nil {
		return nil, simulationError(err)
	}

	answer := map[string]any{
		"simulation_id": stored.ID,
		"version":       stored.Version,
		"channel_id":    in.ChannelID,
		"asset":         asset.Name,
		"device_id":     asset.ExternalRef,
		"service_id":    channel.ExternalRef,
		"was":           before,
		"now":           string(source.Kind),
		"rationale":     in.Rationale,
		"note": "The change reaches the running simulation on its next reload. What it published " +
			"before is already in timescale and does not change — a series whose source was " +
			"swapped has two regimes in it, and a profile over the whole window will say so.",
	}
	if source.Kind == simulation.SourceDataset && source.Dataset != nil {
		answer["replay"] = datasetReplayNote(*source.Dataset)
	}
	return answer, nil
}

func datasetReplayNote(dataset simulation.DatasetSource) string {
	switch dataset.Origin {
	case simulation.OriginPlatform:
		return "This channel now replays the history of a real platform device, fetched once " +
			"when the environment starts and replayed from then on. The replay does not follow " +
			"that device live: it is a recording of the window, not a mirror."
	case simulation.OriginFile:
		return "This channel now replays an uploaded dataset. A dataset is immutable in MOSES, " +
			"so what it plays cannot change under it — which is what keeps the replay " +
			"reproducible and a backfill over it repeatable."
	case simulation.OriginEndpoint:
		return "This channel now polls an endpoint. Whether that endpoint is reachable at all " +
			"is MOSES's allow-list to decide, not this tool's: a host it does not allow is " +
			"refused there, and the channel then plays silence rather than failing loudly."
	}
	return ""
}

// ---- set_simulation_context (L0, confirmed) ----

type setSimulationContextInput struct {
	SimulationID string                    `json:"simulation_id"`
	Rationale    string                    `json:"rationale"`
	Context      map[string]any            `json:"context"`
	Zones        map[string]map[string]any `json:"zones"`
	Assets       map[string]map[string]any `json:"assets"`
}

func (s *surface) setSimulationContext(ctx context.Context, req Request) (any, error) {
	var in setSimulationContextInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	change := simulation.StateChange{Context: in.Context, Zones: in.Zones, Assets: in.Assets}
	if change.Empty() {
		return nil, fmt.Errorf("%w: the patch sets nothing", ErrInvalidInput)
	}
	if strings.TrimSpace(in.SimulationID) == "" {
		return nil, fmt.Errorf("%w: simulation_id is required", ErrInvalidInput)
	}

	if err := s.deps.Simulation.Patch(ctx, req.Token, in.SimulationID, change); err != nil {
		return nil, simulationError(err)
	}
	return map[string]any{
		"simulation_id": in.SimulationID,
		"rationale":     in.Rationale,
		"applied":       change,
		"note": "This changed the live state, not the definition: it is gone when the " +
			"simulation restarts, and get_simulation still reports what the document says. " +
			"That is what makes it right for \"what happens when the hall is at 30 °C\" and " +
			"wrong for \"this hall runs warm\", which is a change to the definition.",
		"one_step_only": "There is no scheduling here and one call sets one state. Ramping a " +
			"value over an hour is your own run_code against the same endpoint, not a second " +
			"call to this.",
	}, nil
}

// ---- delete_simulation (L0, confirmed) ----

type deleteSimulationInput struct {
	SimulationID string `json:"simulation_id"`
	Rationale    string `json:"rationale"`
}

func (s *surface) deleteSimulation(ctx context.Context, req Request) (any, error) {
	var in deleteSimulationInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	created, err := s.creationOf(ctx, req, CreatedSimulation, in.SimulationID, in.Rationale)
	if err != nil {
		return nil, err
	}

	// Read before deleting, so the answer can say what went. A simulation whose
	// devices are gone is not something the developer can look up afterwards.
	devices := []string{}
	keptDevices := []string{}
	if environment, readErr := s.deps.Simulation.Get(ctx, req.Token, created.ID); readErr == nil {
		environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
			if asset.ExternalRef == "" {
				return
			}
			if asset.ExternalManaged {
				devices = append(devices, asset.ExternalRef)
				return
			}
			keptDevices = append(keptDevices, asset.ExternalRef)
		})
	}

	if err := s.deps.Simulation.Delete(ctx, req.Token, created.ID); err != nil {
		return nil, simulationError(err)
	}

	answer := map[string]any{
		"deleted":         created,
		"rationale":       in.Rationale,
		"devices_deleted": devices,
		"note": "The simulation, the platform devices MOSES created for it and the graph it was " +
			"mirrored as are gone. What those devices published is gone with them: a timeseries " +
			"belongs to its device, so anything trained on this scenario has no data behind it " +
			"any more.",
		"undoable": false,
	}
	if len(keptDevices) > 0 {
		answer["devices_kept"] = keptDevices
		answer["devices_kept_note"] = "These were the developer's own devices, attached to an " +
			"asset rather than created for it. They and their timeseries outlive the simulation."
	}
	return answer, nil
}

// ---- backfill_simulation (L0, confirmed) ----

type backfillSimulationInput struct {
	SimulationID string `json:"simulation_id"`
	From         string `json:"from"`
	To           string `json:"to"`
	Rationale    string `json:"rationale"`
}

func (s *surface) backfillSimulation(ctx context.Context, req Request) (any, error) {
	var in backfillSimulationInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	if strings.TrimSpace(in.SimulationID) == "" {
		return nil, fmt.Errorf("%w: simulation_id is required", ErrInvalidInput)
	}
	window, err := parseWindow(in.From, in.To)
	if err != nil {
		return nil, err
	}
	if window.From.IsZero() || window.To.IsZero() {
		return nil, fmt.Errorf("%w: a backfill needs both ends of the window, as RFC3339. "+
			"There is no default: how far back a scenario should reach is the developer's "+
			"decision and it is what the run costs", ErrInvalidInput)
	}

	// Checked before the job rather than read out of its status afterwards. The
	// warning is the same one create_simulation gives, and it is repeated here
	// because a simulation can be built by one session and backfilled by another —
	// and because a job that skips every channel still runs, reports itself done
	// and publishes nothing.
	//
	// It warns and does not refuse. The developer has confirmed a window, a job
	// that publishes nothing costs only time, and the status it leaves behind is
	// itself a record of what the device types do not allow. Refusing on ODE's own
	// reading of a precondition MOSES evaluates more fully would be ODE overruling
	// a decision it cannot fully see.
	req.Progress("checking", "checking which channels can carry a historical timestamp")
	blockedBackfill := s.backfillPrecondition(ctx, req, in.SimulationID)

	req.Progress("starting", "asking MOSES to reconstruct the window")
	status, err := s.deps.Simulation.Backfill(ctx, req.Token, in.SimulationID, window.From, window.To)
	if err != nil {
		return nil, simulationError(err)
	}

	return map[string]any{
		"simulation_id": status.EnvironmentID,
		"state":         status.State,
		"from":          window.From.Format(time.RFC3339),
		"to":            window.To.Format(time.RFC3339),
		"rationale":     in.Rationale,
		"note": "The job runs asynchronously; this is where it stood the moment it was accepted. " +
			"Follow it with get_backfill_status, and read the skipped channels there before " +
			"concluding anything about the data: a job can finish with every channel skipped.",
		"warnings": append([]string{
			"A backfill is not idempotent. Running the same window twice writes every row twice, " +
				"and timescale keeps both — there is no de-duplication anywhere downstream.",
			"Only profile and dataset channels are reconstructed. A script channel depends on " +
				"state its earlier runs left behind, and a formula or an aggregate follows from " +
				"other channels; all three are skipped and say so.",
		}, blockedBackfill...),
	}, nil
}

// backfillPrecondition names the channels of a stored simulation that cannot
// carry a historical timestamp, or says nothing when it could not find out.
//
// Two extra metadata reads on a tool that is about to write weeks of rows, which
// is a trade not worth thinking about twice. Neither failure fails the backfill:
// a simulation that could not be read here is one MOSES will read for itself a
// moment later, and a warning that could not be produced is not a reason to
// refuse a confirmed job.
func (s *surface) backfillPrecondition(ctx context.Context, req Request, id string) []string {
	environment, err := s.deps.Simulation.Get(ctx, req.Token, id)
	if err != nil {
		return nil
	}
	catalogue, err := s.deps.Simulation.DeviceTypes(ctx, req.Token)
	if err != nil {
		return nil
	}
	report := backfillChannels(environment, s.timePathVerdicts(ctx, req, catalogue))
	if warnings := report.Warnings(); len(warnings) > 0 {
		return warnings
	}
	if report.Checked() == 0 {
		// Reached when the document's channels matched nothing in the catalogue at
		// all. Saying nothing here would be the one outcome worth avoiding: silence
		// where a check was expected reads as a pass.
		return []string{"Whether these channels can carry a historical timestamp was not " +
			"established. Read the job's skipped channels in get_backfill_status rather than " +
			"taking the absence of a warning for a working backfill."}
	}
	return []string{"Nothing ODE can see stops these channels carrying a historical timestamp. " +
		simulation.BackfillCaveat}
}

// ---- get_backfill_status (L0) ----

type getBackfillStatusInput struct {
	SimulationID string `json:"simulation_id"`
}

func (s *surface) getBackfillStatus(ctx context.Context, req Request) (any, error) {
	var in getBackfillStatusInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.SimulationID) == "" {
		return nil, fmt.Errorf("%w: simulation_id is required", ErrInvalidInput)
	}
	status, err := s.deps.Simulation.BackfillStatusOf(ctx, req.Token, in.SimulationID)
	if err != nil {
		if errors.Is(err, simulation.ErrNotFound) {
			return map[string]any{
				"simulation_id": in.SimulationID,
				"state":         "unknown",
				"note": "MOSES knows nothing about a backfill of this simulation. That is the " +
					"answer for one that was never started and also for one whose MOSES has " +
					"restarted since — jobs live in memory and a restart forgets them, which is " +
					"reported rather than guessed at. The way to tell them apart is the data: " +
					"profile what the simulation published over the window and see what is there.",
			}, nil
		}
		return nil, simulationError(err)
	}

	skipped := []map[string]any{}
	for _, channel := range status.Skipped() {
		skipped = append(skipped, map[string]any{
			"channel_id": channel.ChannelID,
			"name":       channel.Name,
			"reason":     channel.SkipReason,
		})
	}
	published := []map[string]any{}
	for _, channel := range status.Channels {
		if !channel.Backfillable {
			continue
		}
		entry := map[string]any{
			"channel_id": channel.ChannelID,
			"name":       channel.Name,
			"published":  channel.Published,
		}
		if channel.Failed > 0 {
			entry["failed"] = channel.Failed
			entry["last_error"] = channel.LastError
		}
		if channel.Silent > 0 {
			entry["silent"] = channel.Silent
		}
		published = append(published, entry)
	}

	answer := map[string]any{
		"simulation_id":    status.EnvironmentID,
		"state":            status.State,
		"from":             status.From.UTC().Format(time.RFC3339),
		"to":               status.To.UTC().Format(time.RFC3339),
		"channels_total":   status.ChannelsTotal,
		"channels_done":    status.ChannelsDone,
		"published_rows":   status.Published,
		"channels":         published,
		"skipped_channels": skipped,
	}
	if status.Error != "" {
		answer["error"] = status.Error
	}
	if status.State == simulation.BackfillRunning && status.CurrentChannel != "" {
		answer["current_channel"] = status.CurrentChannel
	}
	switch {
	case len(published) == 0 && len(skipped) > 0:
		answer["note"] = "Every channel was skipped, so this window has no new data in it " +
			"whatever the job's state says. The reasons above are the answer — read them out " +
			"rather than reporting the job as done."
	case status.State == simulation.BackfillDone:
		answer["note"] = "The rows are in timescale and are indistinguishable from live ones. " +
			"Profile the series to see what actually landed: the count above is what was " +
			"published, not what a query over the window returns."
	}
	return answer, nil
}

// ---- list_simulation_datasets (L0) ----

func (s *surface) listSimulationDatasets(ctx context.Context, req Request) (any, error) {
	datasets, err := s.deps.Simulation.Datasets(ctx, req.Token)
	if err != nil {
		return nil, simulationError(err)
	}
	listed := []map[string]any{}
	for _, dataset := range datasets {
		columns := []map[string]any{}
		for _, column := range dataset.Columns {
			columns = append(columns, map[string]any{
				"column": column.Name,
				"points": column.Points,
				"from":   column.From().Format(time.RFC3339),
				"to":     column.To().Format(time.RFC3339),
			})
		}
		listed = append(listed, map[string]any{
			"dataset_id": dataset.ID,
			"name":       dataset.Name,
			"timezone":   dataset.Timezone,
			"columns":    columns,
			"size_bytes": dataset.SizeBytes,
		})
	}
	return map[string]any{
		"datasets": listed,
		"note": "Uploaded timeseries a simulated channel can replay. A dataset is immutable — a " +
			"corrected file is a new dataset, which is what keeps a replay and a backfill over " +
			"it reproducible. Check the span of a column before pointing a channel at it: a " +
			"replay outside the data's own range plays silence rather than failing.",
	}, nil
}

// ---- upload_simulation_dataset (L0, confirmed) ----

// This is the tool that lets the assistant go and find example data rather than
// only arrange what the platform happens to hold.
//
// The file comes out of the developer's own pod, which is where the search
// happened: run_code fetches an open dataset, or reads one the developer put
// there, or writes one out of a notebook, and this carries it into MOSES. Nothing
// here reaches the network on its own — the fetch is the developer's confirmed
// run_code under the developer's own identity, and this call is the second
// confirmation, for putting the result somewhere other applications can see.
type uploadSimulationDatasetInput struct {
	WorkspacePath string `json:"workspace_path"`
	Name          string `json:"name"`
	Timezone      string `json:"timezone"`
	Rationale     string `json:"rationale"`
}

func (s *surface) uploadSimulationDataset(ctx context.Context, req Request) (any, error) {
	var in uploadSimulationDatasetInput
	if err := decode(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Rationale) == "" {
		return nil, fmt.Errorf("%w: a rationale is required: the developer confirms this, "+
			"and cannot confirm what is not argued", ErrInvalidInput)
	}
	if strings.TrimSpace(in.WorkspacePath) == "" {
		return nil, fmt.Errorf("%w: workspace_path is required: the file is read out of the "+
			"developer's own workspace, so write it there with run_code first", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: the dataset needs a name, which is what it will be found "+
			"under", ErrInvalidInput)
	}

	limit := s.deps.Simulation.MaxDatasetBytes()
	req.Progress("reading", "reading "+in.WorkspacePath+" from the developer's workspace")
	content, err := s.deps.Kernel.ReadFile(ctx,
		kernel.Ref{Bearer: req.Token, Workbench: req.WorkbenchID}, in.WorkspacePath, limit)
	if err != nil {
		return nil, err
	}
	if content.Binary {
		return nil, fmt.Errorf("%w: %s did not decode as text. A dataset is a CSV: a header "+
			"line, the time column first, then one or more named value columns",
			ErrInvalidInput, in.WorkspacePath)
	}
	if content.Truncated {
		// Refused rather than uploaded short. A truncated CSV parses — the last line is
		// simply gone — so MOSES would accept it and the replay would end early with
		// nothing anywhere saying why.
		return nil, fmt.Errorf("%w: %s is larger than the %d bytes one dataset may be, and "+
			"what was read is a cut-off file rather than a smaller one. Narrow the window or "+
			"resample it before writing it out", ErrInvalidInput, in.WorkspacePath, limit)
	}
	if strings.TrimSpace(content.Text) == "" {
		return nil, fmt.Errorf("%w: %s is empty", ErrInvalidInput, in.WorkspacePath)
	}

	req.Progress("uploading", "storing the dataset in MOSES, which parses it before it stores it")
	dataset, err := s.deps.Simulation.UploadDataset(ctx, req.Token,
		strings.TrimSpace(in.Name), strings.TrimSpace(in.Timezone), []byte(content.Text))
	if err != nil {
		return nil, simulationError(err)
	}

	columns := []map[string]any{}
	for _, column := range dataset.Columns {
		columns = append(columns, map[string]any{
			"column": column.Name,
			"points": column.Points,
			"from":   column.From().Format(time.RFC3339),
			"to":     column.To().Format(time.RFC3339),
		})
	}
	answer := map[string]any{
		"dataset_id":  dataset.ID,
		"name":        dataset.Name,
		"timezone":    dataset.Timezone,
		"source_path": in.WorkspacePath,
		"size_bytes":  dataset.SizeBytes,
		"columns":     columns,
		"rationale":   in.Rationale,
		"next": "Point a channel at it with set_channel_source: a dataset source with origin " +
			"\"file\", the dataset_id above as ref, and the column you mean. Pick the resample " +
			"mode from what the column is — hold for a state, linear for a temperature, " +
			"distribute for an energy — because the wrong one produces plausible values that " +
			"are wrong between samples.",
		"warnings": []string{
			"MOSES read the timestamps as " + dataset.Timezone + ". A file of local timestamps " +
				"without an offset read in the wrong zone shifts every value by an hour or two, " +
				"which is invisible in the data and wrong in everything trained on it.",
			"Check the spans above against the window you want. A replay outside the data's own " +
				"range plays silence, and a backfill over such a window reports the ticks as " +
				"silent rather than failing.",
		},
	}
	if note := s.recordCreation(ctx, req, Creation{
		Kind: CreatedSimulationDataset,
		ID:   dataset.ID,
		Name: dataset.Name,
		Tool: "upload_simulation_dataset",
	}); note != "" {
		answer["warnings"] = append(answer["warnings"].([]string), note)
	}
	return answer, nil
}

// ---- the shared source shape ----

// sourceIn is what both add_simulated_asset and set_channel_source accept.
//
// `script` is deliberately absent. A script source is JavaScript executed inside
// MOSES, and admitting one here is a decision about a second execution surface
// rather than a feature: profile, dataset, formula and aggregate cover every
// scenario these tools are for, declaratively. A developer who wants a script
// writes it in the MOSES UI, where it is theirs.
type sourceIn struct {
	Kind    string `json:"kind"`
	Profile *struct {
		Base           float64   `json:"base"`
		HourFactors    []float64 `json:"hour_factors"`
		WeekdayFactors []float64 `json:"weekday_factors"`
		DayWindow      *struct {
			FromHour float64 `json:"from_hour"`
			ToHour   float64 `json:"to_hour"`
			Shape    string  `json:"shape"`
		} `json:"day_window"`
		WeekendFactor *float64 `json:"weekend_factor"`
		SpreadPercent float64  `json:"spread_percent"`
		Cumulative    bool     `json:"cumulative"`
	} `json:"profile"`
	Dataset *struct {
		Origin     string  `json:"origin"`
		Ref        string  `json:"ref"`
		ServiceRef string  `json:"service_ref"`
		Column     string  `json:"column"`
		Window     string  `json:"window"`
		Resample   string  `json:"resample"`
		Anchor     string  `json:"anchor"`
		Scale      float64 `json:"scale"`
		Cumulative bool    `json:"cumulative"`
	} `json:"dataset"`
	Formula *struct {
		Expression string            `json:"expression"`
		Inputs     map[string]string `json:"inputs"`
	} `json:"formula"`
}

func buildSource(in sourceIn) (simulation.Source, error) {
	switch simulation.SourceKind(strings.TrimSpace(in.Kind)) {
	case simulation.SourceProfile:
		return buildProfileSource(in)
	case simulation.SourceDataset:
		return buildDatasetSource(in)
	case simulation.SourceFormula:
		return buildFormulaSource(in)
	case simulation.SourceAggregate:
		// No variant, and that is the design rather than an omission: an aggregate sums
		// every asset sub-metered by this one, so the meter tree is the whole
		// configuration.
		return simulation.Source{Kind: simulation.SourceAggregate}, nil
	case simulation.SourceScript:
		return simulation.Source{}, fmt.Errorf("%w: a script source is JavaScript run inside "+
			"MOSES, and these tools do not write one. Say so, and say what the scenario needs: "+
			"a shape over the day is a profile, a real series is a dataset, a value derived "+
			"from other channels is a formula, and a total over sub-metered assets is an "+
			"aggregate. A script is written by the developer in the MOSES UI", ErrInvalidInput)
	case "":
		return simulation.Source{}, fmt.Errorf("%w: the source needs a kind: profile, dataset, "+
			"formula or aggregate", ErrInvalidInput)
	}
	return simulation.Source{}, fmt.Errorf("%w: %q is not a source kind; it is one of profile, "+
		"dataset, formula or aggregate", ErrInvalidInput, in.Kind)
}

func buildProfileSource(in sourceIn) (simulation.Source, error) {
	if in.Profile == nil {
		return simulation.Source{}, fmt.Errorf(
			"%w: kind is \"profile\" and there is no profile", ErrInvalidInput)
	}
	profile := simulation.ProfileSource{
		Base:          in.Profile.Base,
		SpreadPercent: in.Profile.SpreadPercent,
		Cumulative:    in.Profile.Cumulative,
	}
	if profile.SpreadPercent < 0 {
		return simulation.Source{}, fmt.Errorf("%w: spread_percent is %g and cannot be negative",
			ErrInvalidInput, profile.SpreadPercent)
	}

	switch {
	case len(in.Profile.HourFactors) > 0 && in.Profile.DayWindow != nil:
		return simulation.Source{}, fmt.Errorf("%w: give hour_factors or day_window, not both — "+
			"day_window is the shorthand that builds the 24 factors", ErrInvalidInput)
	case len(in.Profile.HourFactors) > 0:
		if len(in.Profile.HourFactors) != 24 {
			return simulation.Source{}, fmt.Errorf("%w: hour_factors has %d entries and needs "+
				"exactly 24, one per hour of the day starting at 00:00. Use day_window instead "+
				"if what you mean is \"on between these hours\"",
				ErrInvalidInput, len(in.Profile.HourFactors))
		}
		profile.HourFactors = in.Profile.HourFactors
	case in.Profile.DayWindow != nil:
		factors, err := dayWindowFactors(
			in.Profile.DayWindow.FromHour, in.Profile.DayWindow.ToHour, in.Profile.DayWindow.Shape)
		if err != nil {
			return simulation.Source{}, err
		}
		profile.HourFactors = factors
	}

	switch {
	case len(in.Profile.WeekdayFactors) > 0 && in.Profile.WeekendFactor != nil:
		return simulation.Source{}, fmt.Errorf("%w: give weekday_factors or weekend_factor, "+
			"not both", ErrInvalidInput)
	case len(in.Profile.WeekdayFactors) > 0:
		if len(in.Profile.WeekdayFactors) != 7 {
			return simulation.Source{}, fmt.Errorf("%w: weekday_factors has %d entries and needs "+
				"exactly 7, starting at monday", ErrInvalidInput, len(in.Profile.WeekdayFactors))
		}
		profile.WeekdayFactors = in.Profile.WeekdayFactors
	case in.Profile.WeekendFactor != nil:
		weekend := *in.Profile.WeekendFactor
		profile.WeekdayFactors = []float64{1, 1, 1, 1, 1, weekend, weekend}
	}

	return simulation.Source{Kind: simulation.SourceProfile, Profile: &profile}, nil
}

// dayWindowFactors builds the 24 hour factors MOSES insists on, from the two
// numbers a caller actually has in mind.
//
// It exists because "the machine runs from six to ten" is the sentence, and
// turning it into a list of exactly twenty-four numbers by hand is the single
// most reliable way to be refused with a 400 — and, when it is off by one entry
// rather than refused, to produce a day that is silently wrong.
func dayWindowFactors(fromHour, toHour float64, shape string) ([]float64, error) {
	if fromHour < 0 || fromHour > 24 || toHour < 0 || toHour > 24 {
		return nil, fmt.Errorf("%w: day_window hours are between 0 and 24", ErrInvalidInput)
	}
	if fromHour == toHour {
		return nil, fmt.Errorf("%w: day_window starts and ends at the same hour, so nothing "+
			"would ever run", ErrInvalidInput)
	}
	factors := make([]float64, 24)
	switch strings.TrimSpace(shape) {
	case "", "block":
		for hour := 0; hour < 24; hour++ {
			at := float64(hour)
			inside := at >= fromHour && at < toHour
			if fromHour > toHour {
				// Wraps past midnight, which is what a night shift is.
				inside = at >= fromHour || at < toHour
			}
			if inside {
				factors[hour] = 1
			}
		}
	case "curve":
		if fromHour >= toHour {
			return nil, fmt.Errorf("%w: a curve rises and falls within one day, so it cannot "+
				"wrap past midnight. Use \"block\" for a night shift", ErrInvalidInput)
		}
		span := toHour - fromHour
		for hour := 0; hour < 24; hour++ {
			at := float64(hour) + 0.5
			if at <= fromHour || at >= toHour {
				continue
			}
			factors[hour] = halfSine((at - fromHour) / span)
		}
	default:
		return nil, fmt.Errorf("%w: day_window shape is \"block\" (on or off) or \"curve\" "+
			"(rising and falling, like daylight), not %q", ErrInvalidInput, shape)
	}
	return factors, nil
}

func buildDatasetSource(in sourceIn) (simulation.Source, error) {
	if in.Dataset == nil {
		return simulation.Source{}, fmt.Errorf(
			"%w: kind is \"dataset\" and there is no dataset", ErrInvalidInput)
	}
	origin := simulation.DatasetOrigin(strings.TrimSpace(in.Dataset.Origin))
	switch origin {
	case simulation.OriginPlatform, simulation.OriginFile, simulation.OriginEndpoint:
	default:
		return simulation.Source{}, fmt.Errorf("%w: a dataset origin is \"platform\" (a real "+
			"device's history), \"file\" (an uploaded dataset) or \"endpoint\" (an allow-listed "+
			"url), not %q", ErrInvalidInput, in.Dataset.Origin)
	}
	if strings.TrimSpace(in.Dataset.Ref) == "" {
		return simulation.Source{}, fmt.Errorf("%w: a dataset source needs a ref: the device id "+
			"for origin \"platform\", the dataset id for \"file\", the url for \"endpoint\"",
			ErrInvalidInput)
	}
	if origin == simulation.OriginPlatform {
		if strings.TrimSpace(in.Dataset.ServiceRef) == "" {
			return simulation.Source{}, fmt.Errorf("%w: replaying a platform device needs the "+
				"service too — a device publishes several, and which one carries the value is "+
				"not derivable from the device id", ErrInvalidInput)
		}
		if strings.TrimSpace(in.Dataset.Column) == "" {
			return simulation.Source{}, fmt.Errorf("%w: replaying a platform device needs the "+
				"output variable path, e.g. \"value\" — the same path an operator mapping takes",
				ErrInvalidInput)
		}
	}

	resample := simulation.ResampleMode(strings.TrimSpace(in.Dataset.Resample))
	switch resample {
	case simulation.ResampleHold, simulation.ResampleLinear, simulation.ResampleDistribute:
	case "":
		return simulation.Source{}, fmt.Errorf("%w: a replay needs a resample mode, because it "+
			"decides what the values between two samples are. \"hold\" keeps the last value and "+
			"is right for a state, \"linear\" interpolates and is right for a temperature, "+
			"\"distribute\" spreads a sample across the interval and is right for an energy. "+
			"The wrong one produces plausible values that are wrong", ErrInvalidInput)
	default:
		return simulation.Source{}, fmt.Errorf("%w: %q is not a resample mode; it is \"hold\", "+
			"\"linear\" or \"distribute\"", ErrInvalidInput, in.Dataset.Resample)
	}

	anchor := simulation.AnchorMode(strings.TrimSpace(in.Dataset.Anchor))
	switch anchor {
	case simulation.AnchorLoop, simulation.AnchorOriginal:
	case "":
		anchor = simulation.AnchorLoop
	default:
		return simulation.Source{}, fmt.Errorf("%w: %q is not an anchor; it is \"loop\" (replay "+
			"from simulation start, repeating) or \"original\" (at the timestamps the data "+
			"carries)", ErrInvalidInput, in.Dataset.Anchor)
	}

	if in.Dataset.Window != "" {
		if _, err := simulation.ParseWindow(in.Dataset.Window); err != nil {
			return simulation.Source{}, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
		}
	}

	return simulation.Source{
		Kind: simulation.SourceDataset,
		Dataset: &simulation.DatasetSource{
			Origin:     origin,
			Ref:        strings.TrimSpace(in.Dataset.Ref),
			ServiceRef: strings.TrimSpace(in.Dataset.ServiceRef),
			Column:     strings.TrimSpace(in.Dataset.Column),
			Window:     strings.TrimSpace(in.Dataset.Window),
			Resample:   resample,
			Anchor:     anchor,
			Scale:      in.Dataset.Scale,
			Cumulative: in.Dataset.Cumulative,
		},
	}, nil
}

func buildFormulaSource(in sourceIn) (simulation.Source, error) {
	if in.Formula == nil {
		return simulation.Source{}, fmt.Errorf(
			"%w: kind is \"formula\" and there is no formula", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Formula.Expression) == "" {
		return simulation.Source{}, fmt.Errorf("%w: a formula needs an expression", ErrInvalidInput)
	}
	if len(in.Formula.Inputs) == 0 {
		return simulation.Source{}, fmt.Errorf("%w: a formula needs inputs, mapping each name "+
			"used in the expression to a channel id or a context key", ErrInvalidInput)
	}
	return simulation.Source{
		Kind: simulation.SourceFormula,
		Formula: &simulation.FormulaSource{
			Expression: strings.TrimSpace(in.Formula.Expression),
			Inputs:     in.Formula.Inputs,
		},
	}, nil
}

// ---- shared helpers ----

// simulationOf reads one environment, turning the two answers a model can act on
// into refusals it can read.
func (s *surface) simulationOf(ctx context.Context, req Request, id string) (simulation.Environment, error) {
	if strings.TrimSpace(id) == "" {
		return simulation.Environment{}, fmt.Errorf(
			"%w: simulation_id is required; list_simulations reports one per simulation",
			ErrInvalidInput)
	}
	environment, err := s.deps.Simulation.Get(ctx, req.Token, id)
	if err != nil {
		return simulation.Environment{}, simulationError(err)
	}
	return environment, nil
}

// simulationError maps the simulation package's own errors onto what a model
// should read.
//
// The version conflict is the one worth care. It is not a platform failure and it
// is not the model's bad argument: it is a race, and the only correct reaction is
// to read the document again and re-apply the change to what is there now. Saying
// that in the error is what stops a model retrying the same stale write — which
// would fail identically, forever.
func simulationError(err error) error {
	var conflict *simulation.VersionConflict
	if errors.As(err, &conflict) {
		return fmt.Errorf("%w: %s. Nothing was written and nothing was deleted: MOSES refuses "+
			"the whole document. Read the simulation again with get_simulation and apply the "+
			"change to what is there now — do not send this one again, it will be refused the "+
			"same way", ErrInvalidInput, conflict.Error())
	}
	if errors.Is(err, simulation.ErrUnknownField) {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	if errors.Is(err, simulation.ErrNotFound) {
		return fmt.Errorf("%w: no such simulation, or this developer cannot see it. "+
			"list_simulations reports the ones they own", ErrInvalidInput)
	}
	return invalidIfSimulationRequest(err)
}

func invalidIfSimulationRequest(err error) error {
	if errors.Is(err, simulation.ErrInvalidRequest) {
		return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	return err
}

func countOf(environment simulation.Environment) (zones, assets, channels int) {
	var walk func(list []simulation.Zone)
	walk = func(list []simulation.Zone) {
		for _, zone := range list {
			zones++
			for _, asset := range zone.Assets {
				assets++
				channels += len(asset.Channels)
			}
			walk(zone.Zones)
		}
	}
	walk(environment.Zones)
	return zones, assets, channels
}

func zoneIDsOf(environment simulation.Environment) []string {
	out := []string{}
	var walk func(list []simulation.Zone)
	walk = func(list []simulation.Zone) {
		for _, zone := range list {
			out = append(out, zone.ID+" ("+zone.Name+")")
			walk(zone.Zones)
		}
	}
	walk(environment.Zones)
	return out
}

func hasAsset(environment simulation.Environment, id string) bool {
	found := false
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		if asset.ID == id {
			found = true
		}
	})
	return found
}

func assetKindOf(raw string) (simulation.AssetKind, error) {
	kind := simulation.AssetKind(strings.TrimSpace(raw))
	switch kind {
	case simulation.AssetMeter, simulation.AssetInverter, simulation.AssetMachine,
		simulation.AssetSensor, simulation.AssetActuator:
		return kind, nil
	case "":
		return "", fmt.Errorf("%w: the asset needs a kind: meter, inverter, machine, sensor "+
			"or actuator", ErrInvalidInput)
	}
	return "", fmt.Errorf("%w: %q is not an asset kind; it is meter, inverter, machine, sensor "+
		"or actuator", ErrInvalidInput, raw)
}

func deviceTypeOf(catalogue []simulation.DeviceType, id string) (simulation.DeviceType, bool) {
	for _, deviceType := range catalogue {
		if deviceType.ID == id {
			return deviceType, true
		}
	}
	return simulation.DeviceType{}, false
}

func serviceOf(deviceType simulation.DeviceType, id string) (simulation.DeviceTypeService, bool) {
	for _, service := range deviceType.Services {
		if service.ID == id {
			return service, true
		}
	}
	return simulation.DeviceTypeService{}, false
}

// freshID mints an id that is unique within the document.
//
// MOSES assigns one where a document leaves it empty, so this exists for the
// other half: an id has to be known before the write when something else in the
// same document refers to it, and it has to read as something in a document a
// human will open.
func freshID(environment simulation.Environment, prefix, name string) string {
	taken := map[string]bool{}
	environment.ForEachAsset(func(_ *simulation.Zone, asset *simulation.Asset) {
		taken[asset.ID] = true
	})
	base := prefix + "-" + slug(name)
	candidate := base
	for suffix := 2; taken[candidate]; suffix++ {
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
	return candidate
}

func slug(name string) string {
	var builder strings.Builder
	previousDash := true
	for _, letter := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= '0' && letter <= '9':
			builder.WriteRune(letter)
			previousDash = false
		case !previousDash:
			builder.WriteByte('-')
			previousDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func sortedKeys[V any](values map[string]V) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func mapOfSources(sources map[string]simulation.Source) map[string]simulation.Source {
	if sources == nil {
		return map[string]simulation.Source{}
	}
	return sources
}

// halfSine is a curve over [0,1]: zero at both ends, one in the middle.
//
// The same shape the pv_site template builds its day from, and here for the same
// reason — a triangular day has a corner at noon that a period detector reads
// differently from a smooth one, so a caller asking for "curve" should get the
// curve rather than a ramp.
func halfSine(position float64) float64 {
	if position <= 0 || position >= 1 {
		return 0
	}
	return math.Sin(position * math.Pi)
}

// sourceSchema is what drives a channel, as both add_simulated_asset and
// set_channel_source accept it.
//
// One constant, inlined into both schemas rather than referenced with a $ref.
// A local $ref is legal JSON Schema and is not reliably resolved by every
// provider's tool-input validator, and a schema a provider silently fails to
// resolve is a tool the model cannot call for a reason nothing reports.
//
// `script` is absent, deliberately: it is JavaScript executed inside MOSES, and
// admitting one is a decision about a second execution surface rather than a
// feature. The four kinds here cover the scenarios declaratively.
const sourceSchema = `{
  "type": "object",
  "description": "What drives this channel. Exactly one of profile, dataset or formula goes with the matching kind; \"aggregate\" takes none.",
  "properties": {
    "kind": {
      "type": "string",
      "enum": ["profile", "dataset", "formula", "aggregate"],
      "description": "profile: a shape over the day and week. dataset: replay a real timeseries. formula: derive from other channels and the context. aggregate: the sum over every asset sub-metered by this one, on the channel carrying the same characteristic — it has no configuration at all, because the meter tree is the configuration."
    },
    "profile": {
      "type": "object",
      "properties": {
        "base": {"type": "number", "description": "The value at a factor of one, in the unit of the bound service."},
        "day_window": {
          "type": "object",
          "description": "The shorthand for the 24 hour factors, and the one to prefer: say when the thing runs and this builds them.",
          "properties": {
            "from_hour": {"type": "number", "description": "Local hour it starts."},
            "to_hour": {"type": "number", "description": "Local hour it ends. Below from_hour wraps past midnight, which is a night shift; only \"block\" may wrap."},
            "shape": {"type": "string", "enum": ["block", "curve"], "description": "block: on or off, for a shift. curve: rising and falling like daylight. Default block."}
          },
          "required": ["from_hour", "to_hour"]
        },
        "hour_factors": {"type": "array", "items": {"type": "number"}, "description": "Exactly 24 entries, one per hour starting at 00:00. Use day_window instead unless the shape is genuinely per-hour; a list of the wrong length is refused."},
        "weekend_factor": {"type": "number", "description": "What fraction of the weekday value saturday and sunday carry. 0 is a site that stands still."},
        "weekday_factors": {"type": "array", "items": {"type": "number"}, "description": "Exactly 7 entries starting at monday. Use weekend_factor unless the week is genuinely per-day."},
        "spread_percent": {"type": "number", "description": "Random variation around the value, drawn from the simulation's seed so a repeat matches."},
        "cumulative": {"type": "boolean", "description": "Makes it a meter reading that keeps counting up. The base is then the rate per tick, not the total."}
      },
      "required": ["base"]
    },
    "dataset": {
      "type": "object",
      "properties": {
        "origin": {"type": "string", "enum": ["platform", "file", "endpoint"], "description": "platform: a real device's own history, which is the truest example data there is. file: something uploaded with upload_simulation_dataset. endpoint: an http url, subject to the simulator's allow-list."},
        "ref": {"type": "string", "description": "The device id, the dataset id or the url, per origin."},
        "service_ref": {"type": "string", "description": "For origin \"platform\": which service of that device carries the value."},
        "column": {"type": "string", "description": "For \"platform\": the output variable path, e.g. \"value\". For \"file\": the column name; empty takes the first."},
        "window": {"type": "string", "description": "For \"platform\": how much history to fetch, backwards from simulation start — \"36h\", \"7d\", \"4w\"."},
        "resample": {"type": "string", "enum": ["hold", "linear", "distribute"], "description": "What the values between two samples are. hold for a state, linear for a temperature, distribute for an energy. Required, because the wrong one produces plausible values that are wrong."},
        "anchor": {"type": "string", "enum": ["loop", "original"], "description": "loop replays from simulation start and repeats, which is the only usable mode for a site that runs permanently. original replays at the timestamps the data carries. Default loop."},
        "scale": {"type": "number", "description": "Multiply every value, for adapting a foreign profile in size. Omit to play it as it is."},
        "cumulative": {"type": "boolean", "description": "Keep a meter reading counting across a loop boundary instead of jumping back."}
      },
      "required": ["origin", "ref", "resample"]
    },
    "formula": {
      "type": "object",
      "properties": {
        "expression": {"type": "string", "description": "The expression, in terms of the input names below."},
        "inputs": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Each name used in the expression, mapped to a channel id or a context key."}
      },
      "required": ["expression", "inputs"]
    }
  },
  "required": ["kind"]
}`
