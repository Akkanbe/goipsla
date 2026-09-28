//declscope:namespace node

package config

import "gopkg.in/yaml.v3"

// This file holds the helpers that look at a YAML node before the parser
// decodes it.

// resolveNode follows aliases.
//
//declscope:package // the parser reads every YAML node through it
func resolveNode(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < 64; i++ {
		n = n.Alias
	}
	return n
}

// isNullNode reports whether n is absent or an explicit null.
//
//declscope:package // the parser reads every YAML node through it
func isNullNode(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}
