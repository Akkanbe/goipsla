//declscope:namespace path

package config

import "strconv"

// This file builds the paths of the configuration errors (FieldError.Path):
// "operations[0].target", "operations(id=5).frequency".

// joinPath appends key to path.
//
//declscope:package // the parser and the validation report errors at these paths
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// indexPath appends the sequence index i to path.
//
//declscope:package // the parser and the validation report errors at these paths
func indexPath(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// opPath is the path of the operation with the ID id, after template
// expansion.
//
//declscope:package // the parser and the validation report errors at these paths
func opPath(id int) string { return "operations(id=" + strconv.Itoa(id) + ")" }
