// Package jsonedit changes single keys of a JSON document while keeping the
// order and content of everything else, so a rewrite touches only what agentcfg
// owns.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

type Member struct {
	Key   string
	Value json.RawMessage
}

// Object is a JSON object that remembers the order of its keys.
type Object []Member

func Parse(data []byte) (Object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("not a JSON object")
	}
	obj := Object{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		obj = obj.Set(token.(string), value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("data after the JSON object")
	}
	return obj, nil
}

func (o Object) Get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Set replaces the value of key in place, or appends it.
func (o Object) Set(key string, value json.RawMessage) Object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, Member{key, value})
}

func (o Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshal(m.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(m.Value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Format renders a document indented by two spaces, the layout Claude Code and
// Codex write, with a trailing newline when the original had one.
func Format(value any, trailingNewline bool) ([]byte, error) {
	compact, err := marshal(value)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	if trailingNewline {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// Marshal encodes without escaping <, > and &, which agents write verbatim.
func marshal(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func Marshal(value any) (json.RawMessage, error) { return marshal(value) }

// Equal reports whether two JSON values are the same regardless of layout and
// key order.
func Equal(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
