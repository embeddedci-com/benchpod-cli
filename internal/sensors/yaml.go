package sensors

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// YAML for sensor definitions and stimuli, read with the YAML 1.2 core rules: only true/false are
// booleans (a key such as `on:` stays the word "on"), 012 is twelve, 0o17 octal, 1e5 a float and
// 2026-01-01 a string. The yaml package parses the document; plain scalars are resolved here, the
// same way as the sensor library's tools/yaml2json.py and the Python SDK, so all three send the
// pod the same JSON.

var (
	intRE   = regexp.MustCompile(`^[-+]?(0x[0-9a-fA-F_]+|0o[0-7_]+|0b[01_]+|[0-9][0-9_]*)$`)
	floatRE = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9][0-9_]*(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
)

// plainScalar resolves an unquoted scalar with the YAML 1.2 core schema.
func plainScalar(s string) any {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	case ".inf", "+.inf":
		return math.Inf(1)
	case "-.inf":
		return math.Inf(-1)
	}
	if intRE.MatchString(s) {
		t := strings.ReplaceAll(s, "_", "")
		sign := int64(1)
		if strings.HasPrefix(t, "-") {
			sign = -1
		}
		t = strings.TrimLeft(t, "+-")
		base := 10
		switch {
		case strings.HasPrefix(t, "0x"), strings.HasPrefix(t, "0X"):
			base, t = 16, t[2:]
		case strings.HasPrefix(t, "0o"):
			base, t = 8, t[2:]
		case strings.HasPrefix(t, "0b"):
			base, t = 2, t[2:]
		}
		if v, err := strconv.ParseInt(t, base, 64); err == nil {
			return sign * v
		}
		if base == 10 {
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				return float64(sign) * f
			}
		}
		return s
	}
	if floatRE.MatchString(s) {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64); err == nil {
			return f
		}
	}
	return s
}

// ParseYAML parses YAML text into *Map (keys in document order), slices, strings, int64,
// float64, bool and nil. A duplicate key is an error, as in the pod.
func ParseYAML(text string) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil
	}
	return convertNode(doc.Content[0], 0)
}

func convertNode(n *yaml.Node, depth int) (any, error) {
	if depth > 100 {
		return nil, fmt.Errorf("line %d: nested too deep", n.Line)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return convertNode(n.Content[0], depth+1)
	case yaml.AliasNode:
		return convertNode(n.Alias, depth+1)
	case yaml.ScalarNode:
		if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 ||
			n.Tag == "!!str" {
			return n.Value, nil
		}
		return plainScalar(n.Value), nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := convertNode(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := NewMap()
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, vn := n.Content[i], n.Content[i+1]
			key := mapKey(k)
			if _, dup := out.Get(key); dup {
				return nil, fmt.Errorf("line %d: duplicate key '%s'", k.Line, key)
			}
			v, err := convertNode(vn, depth+1)
			if err != nil {
				return nil, err
			}
			out.Set(key, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

// mapKey is a mapping key as text: a plain integer key as its decimal value (0x10 -> "16"), any
// other key as written.
func mapKey(k *yaml.Node) string {
	if k.Kind == yaml.ScalarNode && k.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) == 0 {
		if v, ok := plainScalar(k.Value).(int64); ok {
			return strconv.FormatInt(v, 10)
		}
	}
	return k.Value
}
