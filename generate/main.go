/***************************************************************
 *
 * Copyright (C) 2024, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package main

//go:generate go run ../generate

// This should not be included in any release of pelican

// Include more generator functions here but keep them encapsulated
// in their separate files under `generate` package
func main() {
	GenParamEnum()
	GenParamStruct()
	GenPlaceholderPathForNext()
	GenSwaggerDoc()
	GenTokenScope()
	GenErrorCodes()
	GenServerFeatures()
}
