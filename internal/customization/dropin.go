/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package customization

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ValidateDropInContent parses tenant-supplied drop-in content against the format
// the AppProfile declared for that directory.
//
// Parsing here rather than at pod start is the whole point: a malformed fragment
// must fail the reconcile with a message naming the file, not crash the app on its
// next restart — possibly hours later, and for every user of that tenant.
func ValidateDropInContent(format gentianov1alpha1.CustomizationDropInFormat, content string) error {
	switch format {
	case "yaml":
		var out interface{}
		if err := yaml.Unmarshal([]byte(content), &out); err != nil {
			return fmt.Errorf("invalid YAML: %w", err)
		}
	case "json":
		var out interface{}
		if err := json.Unmarshal([]byte(content), &out); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
	case "xml":
		var out interface{}
		if err := xml.Unmarshal([]byte(content), &out); err != nil {
			return fmt.Errorf("invalid XML: %w", err)
		}
	case "ini", "properties":
		if err := validateKeyValueLines(content); err != nil {
			return err
		}
	case "toml", "files":
		// TOML has no parser in the operator's dependency set, and "files" is
		// opaque by definition (images, fonts, CSS). Both are size-checked and
		// mounted as-is.
	default:
		return fmt.Errorf("unsupported drop-in format %q", format)
	}
	return nil
}

// validateKeyValueLines checks INI/properties-style content: comments, section
// headers, and key=value pairs. It is deliberately permissive about values and
// strict about structure.
func validateKeyValueLines(content string) error {
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return fmt.Errorf("line %d: unterminated section header %q", i+1, line)
			}
			continue
		}
		if !strings.Contains(line, "=") && !strings.Contains(line, ":") {
			return fmt.Errorf("line %d: expected key=value, section header or comment, got %q", i+1, line)
		}
	}
	return nil
}
