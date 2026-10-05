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

// Package branding turns a cluster's brand into what its pages read.
//
// The brand's look is a design-token document in the W3C Design Tokens
// Community Group format (2025.10): groups of tokens, each with a $value and
// a $type inherited from its group when not its own, and aliases written
// {group.token}. Every page the platform serves -- the concierge, the
// identity provider's screens, the desktop and the consoles -- reads the
// same tokens as CSS custom properties, --brand-<group>-<token>, and keeps
// its own value where a token is absent.
package branding

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CSSPrefix is what every emitted custom property starts with.
const CSSPrefix = "--brand-"

var (
	aliasPattern = regexp.MustCompile(`^\{([^{}]+)\}$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Token is one resolved design token.
type Token struct {
	// Path is the token's place in the document, group by group.
	Path []string
	// Type is its $type, its own or its group's.
	Type string
	// CSS is its value as a CSS custom property value.
	CSS string
}

// Property is the custom property a token is emitted as.
func (t Token) Property() string {
	return CSSPrefix + strings.Join(t.Path, "-")
}

// Parse reads a DTCG document and returns its tokens in document order of
// their paths. A token whose type this does not render (gradients, typography
// composites and the like) is an error rather than a silent omission: a brand
// that sets something nobody will see should be told so.
func Parse(doc []byte) ([]Token, error) {
	if len(strings.TrimSpace(string(doc))) == 0 {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("tokens are not a JSON object: %w", err)
	}
	var out []Token
	if err := walk(root, nil, "", &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i].Path, ".") < strings.Join(out[j].Path, ".") })
	return out, nil
}

func walk(node map[string]any, path []string, inherited string, out *[]Token) error {
	typ := inherited
	if t, ok := node["$type"].(string); ok {
		typ = t
	}
	if value, ok := node["$value"]; ok {
		if len(path) == 0 {
			return fmt.Errorf("the document root is a group, not a token")
		}
		css, err := render(typ, value)
		if err != nil {
			return fmt.Errorf("%s: %w", strings.Join(path, "."), err)
		}
		*out = append(*out, Token{Path: append([]string(nil), path...), Type: typ, CSS: css})
		return nil
	}
	if _, ok := node["$extends"]; ok {
		return fmt.Errorf("%s: $extends is not supported; write the tokens out", strings.Join(path, "."))
	}
	keys := make([]string, 0, len(node))
	for k := range node {
		if !strings.HasPrefix(k, "$") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !namePattern.MatchString(k) {
			return fmt.Errorf("%q is not a token or group name this can emit", strings.Join(append(path, k), "."))
		}
		child, ok := node[k].(map[string]any)
		if !ok {
			return fmt.Errorf("%s is neither a token nor a group", strings.Join(append(path, k), "."))
		}
		if err := walk(child, append(path, k), typ, out); err != nil {
			return err
		}
	}
	return nil
}

// render turns one $value into CSS. An alias renders as a reference to the
// aliased token's custom property, so the cascade resolves it and a page
// that overrides one token sees every alias of it follow.
func render(typ string, value any) (string, error) {
	if s, ok := value.(string); ok {
		if m := aliasPattern.FindStringSubmatch(s); m != nil {
			parts := strings.Split(m[1], ".")
			for _, p := range parts {
				if !namePattern.MatchString(p) {
					return "", fmt.Errorf("alias %s names no token", s)
				}
			}
			return "var(" + CSSPrefix + strings.Join(parts, "-") + ")", nil
		}
	}
	switch typ {
	case "color":
		return renderColor(value)
	case "dimension", "duration":
		return renderMeasure(value)
	case "fontFamily":
		return renderFontFamily(value)
	case "fontWeight", "number":
		switch v := value.(type) {
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), nil
		case string:
			return safeCSS(v)
		}
		return "", fmt.Errorf("a %s is a number", typ)
	case "cubicBezier":
		arr, ok := value.([]any)
		if !ok || len(arr) != 4 {
			return "", fmt.Errorf("a cubicBezier is four numbers")
		}
		nums := make([]string, 4)
		for i, n := range arr {
			f, ok := n.(float64)
			if !ok {
				return "", fmt.Errorf("a cubicBezier is four numbers")
			}
			nums[i] = strconv.FormatFloat(f, 'f', -1, 64)
		}
		return "cubic-bezier(" + strings.Join(nums, ", ") + ")", nil
	case "shadow":
		return renderShadow(value)
	case "":
		return "", fmt.Errorf("no $type, on the token or a group above it")
	}
	return "", fmt.Errorf("$type %q is not rendered", typ)
}

// renderColor accepts the 2025.10 colour object -- colorSpace, components,
// alpha, and the optional hex fallback -- and, for documents written before
// it, a plain CSS colour string.
func renderColor(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return safeCSS(v)
	case map[string]any:
		alpha := 1.0
		if a, ok := v["alpha"].(float64); ok {
			alpha = a
		}
		if hex, ok := v["hex"].(string); ok && alpha == 1 {
			return safeCSS(hex)
		}
		space, _ := v["colorSpace"].(string)
		raw, ok := v["components"].([]any)
		if !ok || len(raw) != 3 {
			return "", fmt.Errorf("a colour has three components")
		}
		comps := make([]float64, 3)
		for i, c := range raw {
			f, ok := c.(float64)
			if !ok {
				// "none" is a valid component; CSS writes it the same way.
				if s, ok := c.(string); ok && s == "none" {
					comps[i] = math.NaN()
					continue
				}
				return "", fmt.Errorf("a colour component is a number")
			}
			comps[i] = f
		}
		num := func(f float64) string {
			if math.IsNaN(f) {
				return "none"
			}
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		a := ""
		if alpha != 1 {
			a = " / " + num(alpha)
		}
		switch space {
		case "srgb":
			return fmt.Sprintf("rgb(%s %s %s%s)", num(comps[0]*255), num(comps[1]*255), num(comps[2]*255), a), nil
		case "hsl", "hwb", "lab", "lch", "oklab", "oklch":
			return fmt.Sprintf("%s(%s %s %s%s)", space, num(comps[0]), num(comps[1]), num(comps[2]), a), nil
		case "display-p3", "a98-rgb", "prophoto-rgb", "rec2020", "srgb-linear", "xyz-d50", "xyz-d65":
			return fmt.Sprintf("color(%s %s %s %s%s)", space, num(comps[0]), num(comps[1]), num(comps[2]), a), nil
		}
		return "", fmt.Errorf("colour space %q is not rendered", space)
	}
	return "", fmt.Errorf("a colour is a colour object or a CSS colour")
}

func renderMeasure(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return safeCSS(v)
	case map[string]any:
		n, ok := v["value"].(float64)
		unit, _ := v["unit"].(string)
		if !ok || !namePattern.MatchString(unit) {
			return "", fmt.Errorf("a measure is {value, unit}")
		}
		return strconv.FormatFloat(n, 'f', -1, 64) + unit, nil
	}
	return "", fmt.Errorf("a measure is {value, unit}")
}

func renderFontFamily(value any) (string, error) {
	var names []string
	switch v := value.(type) {
	case string:
		names = []string{v}
	case []any:
		for _, n := range v {
			s, ok := n.(string)
			if !ok {
				return "", fmt.Errorf("a font family is a name or a list of names")
			}
			names = append(names, s)
		}
	default:
		return "", fmt.Errorf("a font family is a name or a list of names")
	}
	generic := map[string]bool{"serif": true, "sans-serif": true, "monospace": true, "cursive": true, "fantasy": true,
		"system-ui": true, "ui-serif": true, "ui-sans-serif": true, "ui-monospace": true, "ui-rounded": true,
		"-apple-system": true, "BlinkMacSystemFont": true}
	out := make([]string, len(names))
	for i, n := range names {
		if strings.ContainsAny(n, "\"\\;{}<>") {
			return "", fmt.Errorf("font family %q is not a name", n)
		}
		if generic[n] {
			out[i] = n
		} else {
			out[i] = strconv.Quote(n)
		}
	}
	return strings.Join(out, ", "), nil
}

func renderShadow(value any) (string, error) {
	list, ok := value.([]any)
	if !ok {
		list = []any{value}
	}
	parts := make([]string, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("a shadow is {color, offsetX, offsetY, blur, spread}")
		}
		var fields []string
		if inset, _ := m["inset"].(bool); inset {
			fields = append(fields, "inset")
		}
		for _, k := range []string{"offsetX", "offsetY", "blur", "spread"} {
			v, err := renderMeasure(m[k])
			if err != nil {
				return "", fmt.Errorf("shadow %s: %w", k, err)
			}
			fields = append(fields, v)
		}
		color, err := renderColor(m["color"])
		if err != nil {
			return "", fmt.Errorf("shadow color: %w", err)
		}
		parts = append(parts, strings.Join(append(fields, color), " "))
	}
	return strings.Join(parts, ", "), nil
}

// safeCSS admits a value written as CSS text, refusing what would end the
// declaration or the rule it sits in.
func safeCSS(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, ";{}<>\\\"'") || strings.Contains(s, "/*") || strings.Contains(strings.ToLower(s), "url(") {
		return "", fmt.Errorf("%q is not a value this emits", s)
	}
	return s, nil
}

// CSS renders tokens as one :root rule.
func CSS(tokens []Token) string {
	var b strings.Builder
	b.WriteString("/* The cluster's brand, as design tokens. Generated; edit the Branding. */\n:root {\n")
	for _, t := range tokens {
		b.WriteString("  " + t.Property() + ": " + t.CSS + ";\n")
	}
	b.WriteString("}\n")
	return b.String()
}
