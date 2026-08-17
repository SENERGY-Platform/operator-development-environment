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

package profiler

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/timeseries"
)

// Detector correctness is checked against fixtures with known answers rather
// than against the platform (§5.4.14): a synthesised 15-minute series with an
// injected gap, a monotonic counter with two resets, a bimodal
// washing-machine load. That is what makes the profiler testable without an LLM
// and without the cluster.

var fixtureStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

const quarterHour = 15 * time.Minute

// regularSeries is n points at a fixed interval, valued by shape(i).
func regularSeries(start time.Time, interval time.Duration, n int, shape func(i int) float64) ([]time.Time, []float64) {
	times := make([]time.Time, 0, n)
	values := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		times = append(times, start.Add(time.Duration(i)*interval))
		values = append(values, shape(i))
	}
	return times, values
}

// dropRange removes the points whose index falls in [from, to), which is how a
// gap is injected into an otherwise regular series.
func dropRange(times []time.Time, values []float64, from, to int) ([]time.Time, []float64) {
	outTimes := make([]time.Time, 0, len(times))
	outValues := make([]float64, 0, len(values))
	for i := range times {
		if i >= from && i < to {
			continue
		}
		outTimes = append(outTimes, times[i])
		outValues = append(outValues, values[i])
	}
	return outTimes, outValues
}

// counterWithResets is a rising counter that drops back to zero at each reset
// index, as a replaced or rolled-over meter does.
func counterWithResets(start time.Time, interval time.Duration, n int, step float64, resets []int) ([]time.Time, []float64) {
	times := make([]time.Time, 0, n)
	values := make([]float64, 0, n)
	current := 1000.0
	resetAt := map[int]bool{}
	for _, index := range resets {
		resetAt[index] = true
	}
	for i := 0; i < n; i++ {
		if resetAt[i] {
			current = 0
		}
		times = append(times, start.Add(time.Duration(i)*interval))
		values = append(values, current)
		current += step
	}
	return times, values
}

// washingMachineLoad is the motivating session fixture: long idle stretches at a
// standby draw, interrupted by cycles at a working load.
func washingMachineLoad(start time.Time, interval time.Duration, cycles int, idlePoints, activePoints int) ([]time.Time, []float64) {
	times := []time.Time{}
	values := []float64{}
	at := start
	push := func(value float64) {
		times = append(times, at)
		values = append(values, value)
		at = at.Add(interval)
	}
	for cycle := 0; cycle < cycles; cycle++ {
		for i := 0; i < idlePoints; i++ {
			push(2 + float64(i%3)) // a standby draw that jitters a little
		}
		for i := 0; i < activePoints; i++ {
			push(1800 + 40*math.Sin(float64(i)))
		}
	}
	for i := 0; i < idlePoints; i++ {
		push(2)
	}
	return times, values
}

// column builds the decoded shape the detectors consume.
func column(name string, times []time.Time, values []float64) timeseries.Column {
	out := timeseries.Column{Name: name, Times: times}
	for _, v := range values {
		out.Values = append(out.Values, json.Number(strconv.FormatFloat(v, 'f', -1, 64)))
	}
	return out
}

func textColumn(name string, times []time.Time, values []string) timeseries.Column {
	out := timeseries.Column{Name: name, Times: times}
	for _, v := range values {
		out.Values = append(out.Values, v)
	}
	return out
}

// --- platform stand-ins ---

// fakeTimeseries answers from fixtures. Query is the value-reading method, and
// onQuery is how a test asserts that a code path never reaches it.
type fakeTimeseries struct {
	availability map[string][]timeseries.Availability
	usage        map[string]timeseries.Usage
	results      [][]timeseries.QueryResult
	queries      [][]timeseries.QueryElement
	usageErr     error
	availErr     error
	queryErr     error
	onQuery      func([]timeseries.QueryElement)
}

func (f *fakeTimeseries) DataAvailability(_ context.Context, _ string, deviceID string) ([]timeseries.Availability, error) {
	if f.availErr != nil {
		return nil, f.availErr
	}
	return f.availability[deviceID], nil
}

func (f *fakeTimeseries) DeviceUsage(_ context.Context, _ string, deviceIDs []string) ([]timeseries.Usage, error) {
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	out := []timeseries.Usage{}
	for _, id := range deviceIDs {
		if usage, found := f.usage[id]; found {
			out = append(out, usage)
		}
	}
	return out, nil
}

func (f *fakeTimeseries) Query(_ context.Context, _ string, elements []timeseries.QueryElement, _ timeseries.QueryOptions) ([]timeseries.QueryResult, error) {
	if f.onQuery != nil {
		f.onQuery(elements)
	}
	f.queries = append(f.queries, elements)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if len(f.results) == 0 {
		return nil, nil
	}
	next := f.results[0]
	if len(f.results) > 1 {
		f.results = f.results[1:]
	}
	return next, nil
}

type fakeOntology struct {
	index *OntologyIndex
	err   error
}

func (f fakeOntology) Ontology(context.Context, string) (*OntologyIndex, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.index == nil {
		return NewOntologyIndex(nil, nil, nil), nil
	}
	return f.index, nil
}

// availabilityWindow builds one raw availability entry, optionally with
// materialised aggregate variants beside it.
func availabilityWindow(serviceID string, from, to time.Time, aggregates ...string) []timeseries.Availability {
	fromCopy, toCopy := from, to
	out := []timeseries.Availability{{ServiceId: serviceID, From: &fromCopy, To: &toCopy}}
	for _, groupTime := range aggregates {
		bucket, groupType := groupTime, "mean"
		out = append(out, timeseries.Availability{
			ServiceId: serviceID, From: &fromCopy, To: &toCopy,
			GroupTime: &bucket, GroupType: &groupType,
		})
	}
	return out
}

// queryResult renders a decoded series back into the wire shape the platform
// actually returns.
//
// That shape is one [time, value] sub-series per requested column, in request
// order, with `columnNames` naming only the first — because /queries/v2 splits
// every column into its own query. This fixture used to emit one wide table with
// every column named, which is what the decoder was written against, so the
// tests confirmed a misunderstanding instead of catching it: a four-column read
// against the real platform found one column and reported the other three as
// dead channels. A fake that agrees with the code it tests proves nothing about
// the server.
func queryResult(requestIndex int, deviceID, serviceID string, columns []string, times []time.Time, values map[string][]float64) timeseries.QueryResult {
	device, service := deviceID, serviceID
	out := timeseries.QueryResult{
		RequestIndex: requestIndex,
		DeviceId:     &device,
		ServiceId:    &service,
	}
	if len(columns) > 0 {
		out.ColumnNames = columns[:1]
	}

	for _, name := range columns {
		series := values[name]
		rows := make([][]any, 0, len(times))
		for i, at := range times {
			// A column absent from the map is null throughout, which is how a
			// silent channel arrives.
			var value any
			if i < len(series) {
				value = json.Number(strconv.FormatFloat(series[i], 'f', -1, 64))
			}
			rows = append(rows, []any{
				at.UTC().Format("2006-01-02T15:04:05.000Z07:00"), value,
			})
		}
		out.Data = append(out.Data, rows)
	}
	return out
}

// meterDevice is a device type carrying instantaneous power and a cumulative
// energy counter on one service — the pairing §5.4.1 is built around.
func meterDevice(deviceID, serviceID string) models.ExtendedDevice {
	return models.ExtendedDevice{
		Device: models.Device{
			Id: deviceID, Name: "PV Meter", DeviceTypeId: "dt-meter",
		},
		ConnectionState: models.ConnectionStateOnline,
		Permissions:     models.Permissions{Read: true, Execute: true},
		DeviceType: &models.DeviceType{
			Id: "dt-meter", Name: "Meter", DeviceClassId: "dc-meter",
			Services: []models.Service{{
				Id: serviceID, Name: "readings", Interaction: models.EVENT,
				Outputs: []models.Content{{
					ContentVariable: models.ContentVariable{
						Id: "cv-root", Name: "value", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{
							{
								Id: "cv-power", Name: "power", Type: models.Float,
								CharacteristicId: "ch-watt", FunctionId: "fn-power", AspectId: "aspect-pv",
							},
							{
								Id: "cv-total", Name: "total", Type: models.Float,
								CharacteristicId: "ch-watthour", FunctionId: "fn-energy", AspectId: "aspect-pv",
							},
						},
					},
				}},
			}},
		},
	}
}

// powerOntology declares watts and watt-hours with a conversion between them,
// which is what unit resolution and the conversion walk are checked against.
func powerOntology() *OntologyIndex {
	watt := models.Characteristic{Id: "ch-watt", Name: "Watt", DisplayUnit: "W", MinValue: 0.0, MaxValue: 10000.0}
	kilowatt := models.Characteristic{Id: "ch-kilowatt", Name: "Kilowatt", DisplayUnit: "kW"}
	megawatt := models.Characteristic{Id: "ch-megawatt", Name: "Megawatt", DisplayUnit: "MW"}
	wattHour := models.Characteristic{Id: "ch-watthour", Name: "Watt hour", DisplayUnit: "Wh", MinValue: 0.0}

	return NewOntologyIndex(
		[]models.Characteristic{watt, kilowatt, megawatt, wattHour},
		[]models.ConceptWithCharacteristics{
			{
				Id: "concept-power", Name: "Power", BaseCharacteristicId: "ch-watt",
				Characteristics: []models.Characteristic{watt, kilowatt, megawatt},
				Conversions: []models.ConverterExtension{
					{From: "ch-watt", To: "ch-kilowatt", Distance: 1, Formula: "x / 1000"},
					{From: "ch-kilowatt", To: "ch-megawatt", Distance: 1, Formula: "x / 1000"},
				},
			},
			{
				Id: "concept-energy", Name: "Energy", BaseCharacteristicId: "ch-watthour",
				Characteristics: []models.Characteristic{wattHour},
			},
		},
		[]models.Function{
			{Id: "fn-power", Name: "power consumption", ConceptId: "concept-power", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
			{Id: "fn-energy", Name: "energy consumption", ConceptId: "concept-energy", RdfType: models.SES_ONTOLOGY_MEASURING_FUNCTION},
		},
	)
}

func newTestProfiler(t *testing.T, ts TimeseriesClient, index *OntologyIndex, now time.Time) *Profiler {
	t.Helper()
	prof, err := New(ts, fakeOntology{index: index}, NewMemoryStore(), Options{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return prof
}

func mustGet[T any](t *testing.T, value Value[T], what string) T {
	t.Helper()
	out, ok := value.Get()
	if !ok {
		status := value.Status()
		t.Fatalf("%s is not computed: %s (%s)", what, status.Reason, status.Detail)
	}
	return out
}
