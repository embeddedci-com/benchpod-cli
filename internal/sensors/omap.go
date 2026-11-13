package sensors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Map is a JSON object that keeps its keys in document order. Order matters to the pod: a
// definition's constants and functions may use the ones before them.
type Map struct {
	keys   []string
	values map[string]any
}

// NewMap returns an empty Map.
func NewMap() *Map { return &Map{values: map[string]any{}} }

// Get returns the value of key and whether it is set.
func (m *Map) Get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m.values[key]
	return v, ok
}

// Set sets key, appending it when new.
func (m *Map) Set(key string, v any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = v
}

// Delete removes key.
func (m *Map) Delete(key string) {
	if _, ok := m.values[key]; !ok {
		return
	}
	delete(m.values, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// Keys are the keys in order.
func (m *Map) Keys() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.keys...)
}

// Len is the number of keys.
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// MarshalJSON writes the object with its keys in order, without HTML escaping.
func (m *Map) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalNoEscape(m.values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// decodeOrderedJSON decodes JSON text into Maps, slices, strings, json.Number, bool and nil.
func decodeOrderedJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("text after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > 100 {
		return nil, fmt.Errorf("nested too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := NewMap()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				if _, dup := m.Get(key); dup {
					return nil, fmt.Errorf("duplicate key '%s'", key)
				}
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				m.Set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return m, nil
		case '[':
			out := []any{}
			for dec.More() {
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil
	}
}
