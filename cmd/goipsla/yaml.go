// The YAML-like rendering of a JSON object (namespace "yaml").
//
//declscope:namespace yaml

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// writeYAMLish prints a JSON object as indented "key: value" lines, keeping
// the key order of the JSON.
//
//declscope:package // show config writes the effective configuration with it
func writeYAMLish(w io.Writer, raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("decode config: not an object")
	}
	if err := yamlObject(dec, &b, 0); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// yamlObject prints the members of an object whose "{" has been read.
// indent is the column of the keys; firstPrefix, when set, replaces the
// indentation of the first key (for "- " list items).
func yamlObject(dec *json.Decoder, b *strings.Builder, indent int, firstPrefix ...string) error {
	first := true
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		pad := strings.Repeat(" ", indent)
		if first && len(firstPrefix) > 0 {
			pad = firstPrefix[0]
		}
		first = false
		b.WriteString(pad + fmt.Sprint(kt) + ":")
		if err := yamlValue(dec, b, indent); err != nil {
			return err
		}
	}
	_, err := dec.Token() // "}"
	return err
}

// yamlValue prints the value after "key:".
func yamlValue(dec *json.Decoder, b *strings.Builder, indent int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			if !dec.More() {
				b.WriteString(" {}\n")
				_, err := dec.Token() // "}"
				return err
			}
			b.WriteString("\n")
			return yamlObject(dec, b, indent+2)
		case '[':
			if !dec.More() {
				b.WriteString(" []\n")
				_, err := dec.Token() // "]"
				return err
			}
			b.WriteString("\n")
			for dec.More() {
				it, err := dec.Token()
				if err != nil {
					return err
				}
				pad := strings.Repeat(" ", indent+2)
				if it == json.Delim('{') {
					if err := yamlObject(dec, b, indent+4, pad+"- "); err != nil {
						return err
					}
					continue
				}
				b.WriteString(pad + "- " + yamlScalar(it) + "\n")
			}
			_, err := dec.Token() // "]"
			return err
		}
	default:
		b.WriteString(" " + yamlScalar(tok) + "\n")
	}
	return nil
}

// yamlScalar prints a JSON scalar as a YAML scalar.
func yamlScalar(tok json.Token) string {
	switch v := tok.(type) {
	case nil:
		return "null"
	case string:
		if v == "" {
			return `""`
		}
		return v
	default:
		return fmt.Sprint(v)
	}
}
