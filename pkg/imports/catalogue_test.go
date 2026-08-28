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

package imports

import (
	"testing"

	dsmodel "github.com/SENERGY-Platform/device-selection/pkg/model"
	"github.com/SENERGY-Platform/models/go/models"
)

// semanticType carries what weatherType deliberately does not: a function and an
// aspect per variable, one variable per criterion, so a criteria match can be
// told apart from a match on everything.
func semanticType() dsmodel.ImportType {
	return dsmodel.ImportType{
		Id:   testTypeID,
		Name: "Open-Meteo history",
		Configs: []dsmodel.ImportTypeConfig{
			{Name: "lat", Type: models.Float, DefaultValue: 51.34},
			{Name: "station", Type: models.String},
			{Name: "api_key", Type: models.String},
		},
		Output: dsmodel.ImportContentVariable{
			Name: "root", Type: models.Structure,
			SubContentVariables: []dsmodel.ImportContentVariable{
				{Name: "import_id", Type: models.String},
				{Name: "time", Type: models.String},
				{Name: "value", Type: models.Structure, SubContentVariables: []dsmodel.ImportContentVariable{
					{Name: "temperature", Type: models.Float, CharacteristicId: "ch-celsius",
						FunctionId: "fn-temperature", AspectId: "inverter"},
					{Name: "power", Type: models.Float, FunctionId: "fn-power", AspectId: "pv"},
					{Name: "station_name", Type: models.String},
				}},
			},
		},
	}
}

func TestTypeVariablesDropsTheEnvelopeAndAddressesThePayload(t *testing.T) {
	found := TypeVariables(semanticType())

	paths := map[string]bool{}
	for _, variable := range found {
		paths[variable.Path] = true
	}
	// import_id and time are content variables of the message and not series. A
	// catalogue row offering them would send a model to wire an operator to a
	// timestamp.
	if paths["import_id"] || paths["time"] || paths["root.import_id"] {
		t.Errorf("envelope leaves are not variables: %v", paths)
	}
	// Message-relative, as everywhere else here: the form an operator mapping and
	// an export value take.
	if !paths["value.temperature"] {
		t.Errorf("got %v, want value.temperature", paths)
	}
	if len(found) != 3 {
		t.Fatalf("got %d variables, want the three payload leaves: %+v", len(found), found)
	}
	if found[0].Path > found[1].Path {
		t.Errorf("variables are sorted by path, got %+v", found)
	}
}

func TestTypeVariablesKeepAnUndeclaredCharacteristicNull(t *testing.T) {
	for _, variable := range TypeVariables(semanticType()) {
		if variable.Path == "value.power" && variable.CharacteristicID != nil {
			t.Errorf("characteristic = %q, want null: an invented one authorises a wrong conversion",
				*variable.CharacteristicID)
		}
		if variable.Path == "value.temperature" {
			if variable.CharacteristicID == nil || *variable.CharacteristicID != "ch-celsius" {
				t.Errorf("declared characteristic dropped: %+v", variable)
			}
		}
	}
}

func TestMatchingVariablesUnionsTheCriteriaRatherThanIntersectingThem(t *testing.T) {
	// Upstream ANDs criteria to decide whether the *type* matches, possibly across
	// two different variables. Which variables were the reason is a union — an
	// intersection would report nothing for exactly the type that matched because
	// its variables carried one criterion each.
	found := MatchingVariables(semanticType(), []TypeCriterion{
		{FunctionID: "fn-temperature", AspectIDs: []string{"inverter"}},
		{FunctionID: "fn-power", AspectIDs: []string{"pv"}},
	})
	if len(found) != 2 {
		t.Fatalf("got %d variables, want both: %+v", len(found), found)
	}
}

func TestMatchingVariablesNeedsTheAspectSubtree(t *testing.T) {
	// The variable is described against `inverter`, a child of `pv`. A criterion
	// carrying only the parent matches nothing here, which is the reason
	// ontology.AspectSubtreeIDs exists — upstream expands nothing either.
	bare := MatchingVariables(semanticType(), []TypeCriterion{
		{FunctionID: "fn-temperature", AspectIDs: []string{"pv"}},
	})
	if len(bare) != 0 {
		t.Fatalf("got %+v, want nothing: the aspect was not expanded", bare)
	}

	expanded := MatchingVariables(semanticType(), []TypeCriterion{
		{FunctionID: "fn-temperature", AspectIDs: []string{"pv", "inverter"}},
	})
	if len(expanded) != 1 || expanded[0].Path != "value.temperature" {
		t.Fatalf("got %+v, want value.temperature", expanded)
	}
}

func TestMatchingVariablesWithoutCriteriaIsEveryVariable(t *testing.T) {
	if got := MatchingVariables(semanticType(), nil); len(got) != 3 {
		t.Fatalf("got %d, want every payload leaf", len(got))
	}
}

func TestMatchingVariablesCanBeEmptyForAMatchingType(t *testing.T) {
	// The asymmetry worth keeping visible: upstream's criteria index is flattened
	// per type, so this type matches {fn-temperature} and {aspect pv} together
	// while no single variable carries both.
	found := MatchingVariables(semanticType(), []TypeCriterion{
		{FunctionID: "fn-temperature", AspectIDs: []string{"pv"}},
	})
	if len(found) != 0 {
		t.Fatalf("got %+v, want nothing", found)
	}
}

func TestBlockingCredentialsNamesWhatCannotBeDeployedFromAChat(t *testing.T) {
	blocking := BlockingCredentials(semanticType())
	if len(blocking) != 1 || blocking[0] != "api_key" {
		t.Fatalf("got %v, want [api_key]", blocking)
	}

	// With a default there is nothing for a chat to supply, so the type deploys
	// and CreateInstance leaves the value alone. The two have to agree, which is
	// why both read secretShaped.
	withDefault := semanticType()
	withDefault.Configs[2].DefaultValue = "set-by-the-platform"
	if got := BlockingCredentials(withDefault); len(got) != 0 {
		t.Errorf("got %v, want nothing: a credential with a default is not a blocker", got)
	}
}

func TestRequiredConfigsExcludesCredentialsAndDefaults(t *testing.T) {
	required := RequiredConfigs(semanticType())
	if len(required) != 1 || required[0] != "station" {
		// lat has a default; api_key is a refusal rather than a decision.
		t.Fatalf("got %v, want [station]", required)
	}
}

// A creation refuses exactly what BlockingCredentials predicts. The two are
// separate code paths — one describes the type, the other validates a request —
// and a disagreement would be a model told it may deploy something that is then
// rejected under the developer's eyes.
func TestBlockingCredentialsAgreesWithWhatCreateInstanceRefuses(t *testing.T) {
	importType := semanticType()
	if len(BlockingCredentials(importType)) == 0 {
		t.Fatal("fixture has no blocking credential")
	}
	_, _, _, err := resolveConfigs(importType, []ConfigValue{{Name: "station", Value: "leipzig"}})
	if err == nil {
		t.Fatal("resolveConfigs accepted a type BlockingCredentials calls undeployable")
	}
}
