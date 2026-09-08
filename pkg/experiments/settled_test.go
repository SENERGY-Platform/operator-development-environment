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

package experiments

import "testing"

// What may be kept, and what a keeper would freeze.
//
// The cache is only sound while a kept document cannot change again, and two of
// D24's seven reasons are facts about the read rather than about the criterion:
// the poller had no developer credential, or the checkout could not be reached.
// Both answer differently on the next read, so keeping one would leave a developer
// looking at "the criteria could not be evaluated" for criteria sitting in their
// own working copy.
func TestOnlyASummaryThatCannotChangeAgainIsKept(t *testing.T) {
	graded := func(verdict Verdict) Summary {
		return Summary{Finished: true, EvaluationCriteria: Criterion{Met: verdict}}
	}

	cases := []struct {
		name    string
		summary Summary
		want    bool
	}{
		{"a graded run", graded(Met()), true},
		{"a run that missed its target", graded(Unmet()), true},
		{"no criteria file at the commit", graded(
			NotEvaluated(ReasonNoCriteriaFile, "none")), true},
		{"a file that did not parse", graded(
			NotEvaluated(ReasonCriteriaUnparseable, "a tab")), true},
		{"a metric the run never logged", graded(
			NotEvaluated(ReasonMetricNotReported, "no mape")), true},
		{"a criterion with no threshold", graded(
			NotEvaluated(ReasonNoThreshold, "none stated")), true},
		{"nothing stated in the file", graded(
			NotEvaluated(ReasonNoCriterionStated, "none")), true},

		{"built by the poller with no developer credential", graded(
			NotEvaluated(ReasonNoDeveloperCredential, "nobody connected")), false},
		{"a checkout that could not be read", graded(
			NotEvaluated(ReasonCriteriaUnreadable, "no checkout yet")), false},

		{"a run that is still going", Summary{EvaluationCriteria: Criterion{Met: Met()}}, false},
	}
	for _, test := range cases {
		if got := summarySettled(test.summary); got != test.want {
			t.Errorf("%s: settled = %v, want %v", test.name, got, test.want)
		}
	}
}

// A secondary criterion is the developer's too, so it decides this as much as the
// primary one does. Missing that would keep a document whose secondary metrics say
// "could not be evaluated" for as long as the row lives.
func TestASecondaryCriterionThatCouldNotBeReadAlsoHoldsTheSummaryBack(t *testing.T) {
	summary := Summary{
		Finished:           true,
		EvaluationCriteria: Criterion{Met: Met()},
		SecondaryCriteria: []Criterion{
			{Met: Met()},
			{Met: NotEvaluated(ReasonCriteriaUnreadable, "the workbench was still cloning")},
		},
	}
	if summarySettled(summary) {
		t.Error("kept a summary whose secondary criteria the next read would grade")
	}
}
