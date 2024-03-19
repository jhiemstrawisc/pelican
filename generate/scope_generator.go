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

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"

	"gopkg.in/yaml.v3"
)

type ScopeName struct {
	Raw     string
	Display string
}

var requiredScopeKeys = [3]string{"description", "issuedBy", "acceptedBy"}

func handleCaseConversion(s string) string {
	var camelCase string
	nextCap := true // default as true so we capitalize the first letter

	for _, r := range s {
		if r == '_' || r == '.' {
			nextCap = true
			if r == '.' {
				camelCase += "."
			}
			continue
		}

		if nextCap {
			camelCase += string(unicode.ToUpper(r))
			nextCap = false
		} else {

			camelCase += string(r)
		}
	}

	return camelCase
}

func GenTokenScope() {
	filename, _ := filepath.Abs("../docs/scopes.yaml")
	yamlFile, err := os.Open(filename)
	if err != nil {
		panic(err)
	}
	defer yamlFile.Close()

	decoder := yaml.NewDecoder(yamlFile)

	var values []interface{}

	for {
		var value map[string]interface{}
		if err := decoder.Decode(&value); err != nil {
			if err == io.EOF {
				break
			}
			panic(fmt.Errorf("document decode failed: %w", err))
		}
		values = append(values, value)
	}

	scopes := make([]ScopeName, 0)
	storageScopes := make([]ScopeName, 0)
	lotmanScopes := make([]ScopeName, 0)

	for i := 0; i < len(values); i++ {
		entry := values[i].(map[string]interface{})

		scopeName, ok := entry["name"].(string)
		if !ok {
			panic(fmt.Sprintf("Scope entry at position %d is missing the name attribute", i))
		}
		for _, keyName := range requiredScopeKeys {
			if _, ok := entry[keyName]; !ok {
				panic(fmt.Sprintf("Parameter entry '%s' is missing required key '%s'",
					scopeName, keyName))
			}
		}
		camelScopeName := handleCaseConversion(scopeName)
		scopeNameInSnake := strings.Replace(camelScopeName, ".", "_", 1)
		r := []rune(scopeNameInSnake)
		r[0] = unicode.ToUpper(r[0])
		displayName := string(r)
		if strings.HasPrefix(scopeName, "storage") {
			displayName = strings.TrimSuffix(displayName, ":")
			storageScopes = append(storageScopes, ScopeName{Raw: scopeName, Display: displayName})
		} else if strings.HasPrefix(scopeName, "lot") {
			lotmanScopes = append(lotmanScopes, ScopeName{Raw: scopeName, Display: displayName})
		} else {
			scopes = append(scopes, ScopeName{Raw: scopeName, Display: displayName})
		}
	}

	// Create the file to be generated
	f, err := os.Create("../token_scopes/token_scopes.go")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	err = tokenTemplate.Execute(f, struct {
		Scopes        []ScopeName
		StorageScopes []ScopeName
		LotmanScopes  []ScopeName
	}{
		Scopes:        scopes,
		StorageScopes: storageScopes,
		LotmanScopes:  lotmanScopes,
	})

	if err != nil {
		panic(err)
	}
}

var tokenTemplate = template.Must(template.New("").Parse(`// Code generated by go generate; DO NOT EDIT THIS FILE.
// To make changes to source, see generate/scope_generator.go and docs/scopes.yaml
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

package token_scopes

import (
	"github.com/pkg/errors"
)

type TokenScope string

const (
	{{- range $idx, $scope := .Scopes}}
	{{$scope.Display}} TokenScope = "{{$scope.Raw}}"
	{{- end}}

	// Storage Scopes
	{{- range $idx, $scope := .StorageScopes}}
	{{$scope.Display}} TokenScope = "{{$scope.Raw}}"
	{{- end}}

	// Lotman Scopes
	{{- range $idx, $scope := .LotmanScopes}}
	{{$scope.Display}} TokenScope = "{{$scope.Raw}}"
	{{- end}}
)

func (s TokenScope) String() string {
	return string(s)
}

// Interface that allows us to assign a path to some token scopes, such as "storage.read:/foo/bar"
func (s TokenScope) Path(path string) (TokenScope, error) {
	// Only some of the token scopes can be assigned a path. This list might grow in the future.
	if !(
		{{- range $idx, $scope := .StorageScopes -}}s == {{$scope.Display}} || {{end}}false) { // final "false" is a hack so we don't have to post process the template we generate from
		return "", errors.New("cannot assign path to non-storage token scope")
	}

	return TokenScope(s.String() + ":" + path), nil
}
`))
