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
	"math"
	"time"

	"gonum.org/v1/gonum/stat"
)

const (
	// minOverlapPoints is the fewest shared timestamps a pair needs before a
	// relationship is worth asserting.
	minOverlapPoints = 30
	// strongShapeMatch is the correlation at which two series are moving
	// together rather than coincidentally.
	strongShapeMatch = 0.9
	// redundantMatch is the correlation at which two variables look like the
	// same measurement reported twice.
	redundantMatch = 0.99
	// tightResidual is the normalised residual below which a scaled fit
	// explains the pair.
	tightResidual = 0.1
	// noShapeMatch is where a pair that ought to agree demonstrably does not.
	noShapeMatch = 0.3
)

// variableSeries pairs a variable with the raw series read for it, which is what
// the cross-variable checks work on.
type variableSeries struct {
	Variable Variable
	Times    []time.Time
	Values   []float64
	Kind     ValueKind
}

// detectRelationships is detector 8 of §5.4.13, over the service-scoped batch.
//
// The check that motivates the whole batching decision (§5.4.1): energy meters
// routinely emit instantaneous power and a cumulative energy counter on the same
// service. Differencing the counter and comparing it against integrated power
// catches unit errors, dead channels and unflagged resets — and because both
// series arrive in one read, it costs nothing.
//
// Scale is fitted rather than assumed. A W series against a Wh counter differs
// by a factor of 3600, and requiring the raw magnitudes to agree would report
// every correctly-behaving meter as inconsistent. What is tested is the shape;
// the implied factor is reported as evidence.
func detectRelationships(subject variableSeries, siblings []variableSeries) []Relationship {
	out := []Relationship{}
	for _, sibling := range siblings {
		if sibling.Variable.Path == subject.Variable.Path {
			continue
		}
		if relationship, found := relate(subject, sibling); found {
			out = append(out, relationship)
		}
	}
	return out
}

func relate(subject, sibling variableSeries) (Relationship, bool) {
	switch {
	case subject.Kind == KindCumulativeCounter && sibling.Kind == KindInstantaneous:
		return counterAgainstRate(subject, sibling, RelationIntegralOf)
	case subject.Kind == KindInstantaneous && sibling.Kind == KindCumulativeCounter:
		return counterAgainstRate(sibling, subject, RelationDerivativeOf)
	case subject.Kind == sibling.Kind:
		return sameKind(subject, sibling)
	default:
		return Relationship{}, false
	}
}

// counterAgainstRate compares a counter's increments with the rate series
// integrated over the same intervals.
//
// direction says which way round the relationship is reported: the counter is
// the integral of the rate, and the rate is the derivative of the counter. Both
// describe the same finding from the two variables' points of view.
func counterAgainstRate(counter, rate variableSeries, direction RelationshipType) (Relationship, bool) {
	times, counterValues, rateValues := align(counter.Times, counter.Values, rate.Times, rate.Values)
	if len(times) < minOverlapPoints {
		return Relationship{}, false
	}

	increments := make([]float64, 0, len(times)-1)
	integrals := make([]float64, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		seconds := times[i].Sub(times[i-1]).Seconds()
		if seconds <= 0 {
			continue
		}
		delta := counterValues[i] - counterValues[i-1]
		if delta < 0 {
			// A reset, not an increment. Differencing across it would inject a
			// large negative energy the rate series cannot match.
			continue
		}
		increments = append(increments, delta)
		integrals = append(integrals, 0.5*(rateValues[i]+rateValues[i-1])*seconds)
	}
	if len(increments) < minOverlapPoints {
		return Relationship{}, false
	}

	// One side accumulated over the window and the other did not. This is the
	// dead-channel case the check exists for — a counter climbing beside a power
	// reading pinned at zero — and it is decided on totals rather than on shape,
	// because a flat channel has no shape to correlate against.
	if accumulated(sum(increments)) != accumulated(sum(integrals)) {
		return Relationship{
			Type:      RelationInconsistentWith,
			OtherPath: rate.Variable.Path,
			Evidence: RelationshipEvidence{
				OverlapPoints: len(increments),
				ResidualRatio: 1,
			},
			Confidence: Likely,
		}, true
	}

	correlation := stat.Correlation(increments, integrals, nil)
	if math.IsNaN(correlation) {
		// A correlation is undefined when one side does not vary. A counter rising
		// by a constant amount while the rate beside it moves is not tracking it,
		// which is a finding rather than an absence of one.
		if varies(increments) != varies(integrals) {
			return Relationship{
				Type:      RelationInconsistentWith,
				OtherPath: rate.Variable.Path,
				Evidence: RelationshipEvidence{
					OverlapPoints: len(increments),
					ResidualRatio: 1,
				},
				Confidence: Uncertain,
			}, true
		}
		return Relationship{}, false
	}
	scale, residual := fitThroughOrigin(integrals, increments)

	// The other side of the relationship is whichever of the pair is not the
	// subject: an integral_of names the rate it integrates, a derivative_of names
	// the counter it differentiates.
	otherPath := rate.Variable.Path
	if direction == RelationDerivativeOf {
		otherPath = counter.Variable.Path
	}
	relationship := Relationship{
		Type:      direction,
		OtherPath: otherPath,
		Evidence: RelationshipEvidence{
			Correlation:   round2(correlation),
			ResidualRatio: round2(residual),
			OverlapPoints: len(increments),
			ImpliedScale:  signif(scale, 6),
		},
	}

	switch {
	case correlation >= strongShapeMatch && residual <= tightResidual:
		relationship.Confidence = Likely
	case correlation >= strongShapeMatch:
		relationship.Confidence = Uncertain
	case correlation <= noShapeMatch:
		// A counter and a rate on the same service that do not move together is
		// the dead-channel and unit-error case this check exists for.
		relationship.Type = RelationInconsistentWith
		relationship.Confidence = Likely
	default:
		return Relationship{}, false
	}
	return relationship, true
}

func sameKind(subject, sibling variableSeries) (Relationship, bool) {
	times, a, b := align(subject.Times, subject.Values, sibling.Times, sibling.Values)
	if len(times) < minOverlapPoints {
		return Relationship{}, false
	}
	correlation := stat.Correlation(a, b, nil)
	if math.IsNaN(correlation) {
		return Relationship{}, false
	}
	scale, residual := fitThroughOrigin(b, a)

	evidence := RelationshipEvidence{
		Correlation:   round2(correlation),
		ResidualRatio: round2(residual),
		OverlapPoints: len(times),
		ImpliedScale:  signif(scale, 6),
	}
	switch {
	case correlation >= redundantMatch:
		return Relationship{
			Type: RelationRedundantWith, OtherPath: sibling.Variable.Path,
			Evidence: evidence, Confidence: Likely,
		}, true
	case sameQuantity(subject.Variable, sibling.Variable) && correlation <= noShapeMatch:
		// Two variables the ontology says measure the same thing, that do not
		// agree, is a finding rather than noise.
		return Relationship{
			Type: RelationInconsistentWith, OtherPath: sibling.Variable.Path,
			Evidence: evidence, Confidence: Likely,
		}, true
	default:
		return Relationship{}, false
	}
}

func sum(values []float64) float64 {
	var total float64
	for _, v := range values {
		total += v
	}
	return total
}

// accumulated reports whether a total is distinguishable from nothing. The
// threshold is absolute and tiny: it separates "this channel reported no energy
// at all" from a rounding residue, not one magnitude from another.
func accumulated(total float64) bool {
	return math.Abs(total) > 1e-9
}

// varies reports whether a series moves at all, which is what decides whether an
// undefined correlation is a dead channel or two flat lines.
func varies(values []float64) bool {
	for i := 1; i < len(values); i++ {
		if values[i] != values[0] {
			return true
		}
	}
	return false
}

// sameQuantity is the ontology's own claim that two variables measure the same
// thing: the same function on the same aspect, or the same characteristic.
func sameQuantity(a, b Variable) bool {
	if a.FunctionID != "" && a.FunctionID == b.FunctionID && a.AspectID == b.AspectID {
		return true
	}
	return a.CharacteristicID != "" && a.CharacteristicID == b.CharacteristicID
}

// fitThroughOrigin is least squares for y = kx with no intercept, and returns the
// residual sum of squares normalised by the total, so the number is comparable
// across series of any magnitude.
func fitThroughOrigin(x, y []float64) (scale float64, residualRatio float64) {
	var sumXY, sumXX, sumYY float64
	for i := range x {
		sumXY += x[i] * y[i]
		sumXX += x[i] * x[i]
		sumYY += y[i] * y[i]
	}
	if sumXX == 0 || sumYY == 0 {
		return 0, 1
	}
	scale = sumXY / sumXX
	var residual float64
	for i := range x {
		difference := y[i] - scale*x[i]
		residual += difference * difference
	}
	return scale, residual / sumYY
}

// align pairs two series on exact timestamps by a merge join.
//
// Exact matching is right here rather than tolerant: both series come from the
// same service-scoped read, so a shared timestamp means the same message, and
// tolerance would only pair values from different messages.
func align(aTimes []time.Time, aValues []float64, bTimes []time.Time, bValues []float64) ([]time.Time, []float64, []float64) {
	times := make([]time.Time, 0, min(len(aTimes), len(bTimes)))
	left := make([]float64, 0, cap(times))
	right := make([]float64, 0, cap(times))

	i, j := 0, 0
	for i < len(aTimes) && j < len(bTimes) {
		switch {
		case aTimes[i].Before(bTimes[j]):
			i++
		case bTimes[j].Before(aTimes[i]):
			j++
		default:
			times = append(times, aTimes[i])
			left = append(left, aValues[i])
			right = append(right, bValues[j])
			i++
			j++
		}
	}
	return times, left, right
}
