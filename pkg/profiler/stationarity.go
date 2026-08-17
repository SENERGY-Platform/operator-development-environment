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
	"errors"
	"math"
	"time"

	"gonum.org/v1/gonum/mat"
)

// detectTrend fits value against time by ordinary least squares.
//
// It runs on the aggregated pass with the observed bucket timestamps rather than
// on a filled grid, so a gap costs a point instead of contributing an invented
// one at the mean.
func detectTrend(times []time.Time, values []float64, coverage Value[Coverage]) Value[Trend] {
	if block, blocked := coverageTooLow(coverage); blocked {
		return uncomputableFrom[Trend](block)
	}
	if len(values) < minTrendPoints || len(times) != len(values) {
		return Uncomputablef[Trend](ReasonInsufficientCoverage,
			"%d aggregated points, need at least %d to fit a trend", len(values), minTrendPoints)
	}

	origin := times[0]
	seconds := make([]float64, len(times))
	for i, at := range times {
		seconds[i] = at.Sub(origin).Seconds()
	}

	design := make([][]float64, len(seconds))
	for i, s := range seconds {
		design[i] = []float64{1, s}
	}
	fit, err := ols(values, design)
	if err != nil {
		return Uncomputablef[Trend](ReasonOutOfScope, "the trend regression did not solve: %v", err)
	}

	slope := fit.Beta[1]
	tStat := 0.0
	if fit.StdErr[1] > 0 {
		tStat = slope / fit.StdErr[1]
	}

	return Computed(Trend{
		Slope:       roundTo(slope, 9),
		SlopePerDay: roundTo(slope*86400, 6),
		R2:          round2(fit.R2),
		// 1.96 is the two-sided 5% normal quantile. With hundreds of points the
		// t distribution is within a percent of the normal, and the t statistic
		// is reported alongside so the judgement can be re-made.
		Significant: math.Abs(tStat) > 1.96,
		TStat:       round2(tStat),
	})
}

const minTrendPoints = 20

// ADF critical values for the constant-only regression, from MacKinnon's
// asymptotic surface: the values every implementation agrees on and every
// textbook prints.
//
// The finite-sample correction terms are deliberately not applied, and
// minADFObservations is the price of leaving them out: at 500 observations the
// 1/T term shifts the 5% value by about 0.006, which is below the precision
// reported here. Below that threshold the test reports not_computed rather than a
// number whose critical value is wrong in the direction of calling things
// stationary.
var adfCriticalValues = map[string]float64{
	"1pct":  -3.4303,
	"5pct":  -2.8615,
	"10pct": -2.5668,
}

const minADFObservations = 500

// degenerateFitTolerance is how close to a perfect fit counts as no fit at all.
// Real measurements never come this close; a series that does is deterministic,
// and a t statistic built on its rounding error is noise amplified rather than
// evidence.
const degenerateFitTolerance = 1e-9

// detectStationarity runs an augmented Dickey-Fuller test with a constant.
//
// This is the one detector §5.4.14 calls a genuine gap: no Go implementation
// exists, and it needs an OLS regression on lagged differences plus critical
// values. Both are here — the regression through gonum/mat, the critical values
// as published asymptotics — and what is *not* here is a point p-value, because
// producing one needs MacKinnon's p-value response surface. §5.4.14 is explicit
// that faking it is not an option, so the result carries the statistic, the
// critical values it was compared against, and a bracket between the two
// quantiles it falls between. See PValueBracket.
//
// The series is the observed sequence of bucket means, not a gap-filled grid:
// filling a gap with the mean injects mean reversion, which biases a unit-root
// test towards exactly the answer it is meant to establish.
func detectStationarity(values []float64, coverage Value[Coverage]) Value[Stationarity] {
	if block, blocked := coverageTooLow(coverage); blocked {
		return uncomputableFrom[Stationarity](block)
	}

	clean := finite(values)
	// Schwert's rule for the maximum lag, the conventional upper bound.
	maxLag := int(math.Floor(12 * math.Pow(float64(len(clean))/100, 0.25)))
	if maxLag < 1 {
		maxLag = 1
	}
	if quarter := (len(clean) - 1) / 4; maxLag > quarter {
		maxLag = quarter
	}
	observations := len(clean) - 1 - maxLag
	if observations < minADFObservations {
		return Uncomputablef[Stationarity](ReasonInsufficientSpan,
			"%d usable observations after %d lags, need at least %d for the asymptotic critical values to hold",
			observations, maxLag, minADFObservations)
	}

	best, bestLag, err := selectADFLag(clean, maxLag)
	if err != nil {
		return Uncomputablef[Stationarity](ReasonOutOfScope, "the ADF regression did not solve: %v", err)
	}
	if best.StdErr[1] <= 0 {
		return Uncomputable[Stationarity](ReasonOutOfScope,
			"the lagged level has no estimable standard error, so no test statistic")
	}
	// A regression that explains everything has no residual variance to divide by,
	// and the resulting statistic is arbitrarily large rather than significant. It
	// happens on a deterministic series — a synthetic fixture, or a sensor
	// reporting a repeating pattern to the last decimal — and reporting
	// "stationary, ADF -5e14" from it would be nonsense stated with confidence.
	if 1-best.R2 < degenerateFitTolerance {
		return Uncomputablef[Stationarity](ReasonOutOfScope,
			"the regression is degenerate (r² %.12f): the series is deterministic at this resolution, "+
				"so the test statistic carries no information", best.R2)
	}

	statistic := best.Beta[1] / best.StdErr[1]
	critical := map[string]float64{}
	for name, value := range adfCriticalValues {
		critical[name] = value
	}

	return Computed(Stationarity{
		ADFStat:        round2(statistic),
		LagOrder:       bestLag,
		NObs:           observations,
		CriticalValues: critical,
		PValueBracket:  bracketADF(statistic),
		Stationary:     statistic < adfCriticalValues["5pct"],
		// A detector never reports certain (D23); certain is for
		// ontology-derived and developer-confirmed values.
		Confidence: Likely,
		Regression: "constant",
	})
}

// bracketADF places the statistic between the published quantiles.
func bracketADF(statistic float64) PValueBracket {
	switch {
	case statistic < adfCriticalValues["1pct"]:
		return PValueBracket{Lower: 0, Upper: 0.01,
			Note: "below the 1% critical value; the unit root is rejected at every level tabulated"}
	case statistic < adfCriticalValues["5pct"]:
		return PValueBracket{Lower: 0.01, Upper: 0.05,
			Note: "between the 1% and 5% critical values"}
	case statistic < adfCriticalValues["10pct"]:
		return PValueBracket{Lower: 0.05, Upper: 0.10,
			Note: "between the 5% and 10% critical values; not stationary at the conventional 5%"}
	default:
		return PValueBracket{Lower: 0.10, Upper: 1,
			Note: "above the 10% critical value; the unit root is not rejected"}
	}
}

// selectADFLag fits the ADF regression for every lag order up to maxLag and
// keeps the one with the lowest AIC.
//
// Every fit uses the same sample, trimmed to what the largest lag order allows.
// Comparing an AIC across different sample sizes compares different likelihoods
// and would systematically prefer the longest lag.
func selectADFLag(values []float64, maxLag int) (olsFit, int, error) {
	var best olsFit
	bestLag := -1
	bestAIC := math.Inf(1)

	for lag := 0; lag <= maxLag; lag++ {
		y, design := adfDesign(values, lag, maxLag)
		if len(y) == 0 {
			continue
		}
		fit, err := ols(y, design)
		if err != nil {
			continue
		}
		if fit.RSS <= 0 {
			continue
		}
		n := float64(len(y))
		aic := n*math.Log(fit.RSS/n) + 2*float64(len(design[0]))
		if aic < bestAIC {
			bestAIC, bestLag, best = aic, lag, fit
		}
	}
	if bestLag < 0 {
		return olsFit{}, 0, errors.New("no lag order produced a solvable regression")
	}
	return best, bestLag, nil
}

// adfDesign builds the regression
//
//	Δy_t = α + γ·y_{t-1} + Σ δ_i·Δy_{t-i} + ε_t
//
// where γ is the coefficient the test statistic is built from. start is fixed by
// maxLag rather than by lag so that every candidate shares one sample.
func adfDesign(values []float64, lag, maxLag int) ([]float64, [][]float64) {
	differences := diffFloats(values)
	if len(differences) <= maxLag {
		return nil, nil
	}

	y := make([]float64, 0, len(differences)-maxLag)
	design := make([][]float64, 0, len(differences)-maxLag)

	for t := maxLag; t < len(differences); t++ {
		row := make([]float64, 0, 2+lag)
		row = append(row, 1, values[t])
		for i := 1; i <= lag; i++ {
			row = append(row, differences[t-i])
		}
		y = append(y, differences[t])
		design = append(design, row)
	}
	return y, design
}

// olsFit is an ordinary least squares solution with what is needed to test a
// coefficient.
type olsFit struct {
	Beta   []float64
	StdErr []float64
	RSS    float64
	R2     float64
}

// ols solves y = Xβ by normal equations.
//
// §5.4.14 points at gonum/stat/regression for this; there is no such package.
// gonum/mat is the same dependency by another door. The normal equations are
// used rather than a QR factorisation because X here is tall and narrow — a few
// thousand rows by at most a few dozen columns — so the Gram matrix is tiny, and
// a singular one is reported as an error rather than producing a coefficient
// nobody should trust.
func ols(y []float64, design [][]float64) (olsFit, error) {
	rows := len(design)
	if rows == 0 || len(y) != rows {
		return olsFit{}, errors.New("design matrix and response do not agree")
	}
	columns := len(design[0])
	if rows <= columns {
		return olsFit{}, errors.New("fewer observations than parameters")
	}

	flat := make([]float64, 0, rows*columns)
	for _, row := range design {
		if len(row) != columns {
			return olsFit{}, errors.New("ragged design matrix")
		}
		flat = append(flat, row...)
	}
	x := mat.NewDense(rows, columns, flat)
	response := mat.NewVecDense(rows, append([]float64{}, y...))

	var gram mat.Dense
	gram.Mul(x.T(), x)

	var inverse mat.Dense
	if err := inverse.Inverse(&gram); err != nil {
		return olsFit{}, errors.New("the design matrix is collinear: " + err.Error())
	}

	var xty mat.VecDense
	xty.MulVec(x.T(), response)

	var beta mat.VecDense
	beta.MulVec(&inverse, &xty)

	var fitted mat.VecDense
	fitted.MulVec(x, &beta)

	var rss, tss float64
	responseMean := mean(y)
	for i := 0; i < rows; i++ {
		residual := response.AtVec(i) - fitted.AtVec(i)
		rss += residual * residual
		centred := response.AtVec(i) - responseMean
		tss += centred * centred
	}

	sigmaSquared := rss / float64(rows-columns)
	stdErr := make([]float64, columns)
	for i := 0; i < columns; i++ {
		variance := sigmaSquared * inverse.At(i, i)
		if variance > 0 {
			stdErr[i] = math.Sqrt(variance)
		}
	}

	fit := olsFit{Beta: make([]float64, columns), StdErr: stdErr, RSS: rss}
	for i := 0; i < columns; i++ {
		fit.Beta[i] = beta.AtVec(i)
	}
	if tss > 0 {
		fit.R2 = 1 - rss/tss
	}
	return fit, nil
}
