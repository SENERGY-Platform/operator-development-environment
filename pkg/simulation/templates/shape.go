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

package templates

import "math"

// sinePeak is a half sine over [0,1]: zero at both ends, one in the middle.
//
// A curve rather than a triangle because the thing being shaped is a day of
// irradiance, and the difference is visible in exactly the place an operator
// looks — the shoulders. A triangular morning ramps linearly and has a corner at
// noon, which a period detector reads differently from a smooth one.
func sinePeak(position float64) float64 {
	if position <= 0 || position >= 1 {
		return 0
	}
	return math.Sin(position * math.Pi)
}
