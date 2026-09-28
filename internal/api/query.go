//declscope:namespace query

package api

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

// This file reads and checks the query parameters of the requests, and
// encodes the include parameter for the Client.

// checkQuery rejects query parameters not in allowed (400), and parameters
// given more than once unless listed in multi. Keys are checked in sorted
// order so that the error is deterministic.
//
//declscope:package // the handlers read their query with it
func checkQuery(r *http.Request, allowed []string, multi ...string) error {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			if len(allowed) == 0 {
				return fmt.Errorf("unknown query parameter %q: %s takes no parameters", k, r.URL.Path)
			}
			return fmt.Errorf("unknown query parameter %q: must be %s", k, queryKeyList(allowed))
		}
		if len(q[k]) > 1 && !slices.Contains(multi, k) {
			return fmt.Errorf("query parameter %q given more than once", k)
		}
	}
	return nil
}

// queryKeyList renders ["a", "b", "c"] as "a, b or c".
func queryKeyList(v []string) string {
	if len(v) == 1 {
		return v[0]
	}
	return strings.Join(v[:len(v)-1], ", ") + " or " + v[len(v)-1]
}

// stateQueryValues are the accepted values of state=.
var stateQueryValues = []string{StatePending, StateInactive, StateActive}

// parseFilterQuery reads and checks the query of GET /v1/operations.
//
//declscope:package // the handlers read their query with it
func parseFilterQuery(r *http.Request) (OperationFilter, error) {
	var f OperationFilter
	if err := checkQuery(r, []string{"tag", "state", "rc", "type", "include"}, "include"); err != nil {
		return f, err
	}
	q := r.URL.Query()
	f.Tag, f.State, f.RC, f.Type = q.Get("tag"), q.Get("state"), q.Get("rc"), q.Get("type")
	if f.State != "" && !slices.Contains(stateQueryValues, f.State) {
		return f, fmt.Errorf("invalid state %q: must be pending, inactive or active", f.State)
	}
	if f.RC != "" {
		if _, err := op.ParseReturnCode(f.RC); err != nil {
			return f, err
		}
	}
	if f.Type != "" && f.Type != string(config.ICMPEcho) && f.Type != string(config.ICMPJitter) {
		return f, fmt.Errorf("invalid type %q: must be icmp-echo or icmp-jitter", f.Type)
	}
	return f, nil
}

// parseListIncludeQuery parses the include parameter of GET /v1/operations:
// the values of parseIncludeQuery plus "detail", which asks for the details
// without optional parts.
//
//declscope:package // the handlers read their query with it
func parseListIncludeQuery(values []string) (stats.SnapshotOptions, error) {
	return parseQueryInclude(values, true)
}

// parseIncludeQuery parses every value of the include query parameter
// ("hours,history,enhanced"; the parameter may also be repeated). Unknown
// values and empty elements ("include=", "hours,,history") are errors.
//
//declscope:package // the handlers read their query with it
func parseIncludeQuery(values []string) (stats.SnapshotOptions, error) {
	return parseQueryInclude(values, false)
}

// parseQueryInclude parses include; withDetail also accepts "detail".
func parseQueryInclude(values []string, withDetail bool) (stats.SnapshotOptions, error) {
	allowed := "hours, history or enhanced"
	if withDetail {
		allowed = "detail, " + allowed
	}
	var o stats.SnapshotOptions
	for _, value := range values {
		for _, v := range strings.Split(value, ",") {
			switch strings.TrimSpace(v) {
			case "":
				return stats.SnapshotOptions{}, fmt.Errorf("empty element in include %q: must be %s", value, allowed)
			case "hours":
				o.Hours = true
			case "history":
				o.History = true
			case "enhanced":
				o.Enhanced = true
			case "detail":
				if !withDetail {
					return stats.SnapshotOptions{}, fmt.Errorf("unknown include %q: must be %s", v, allowed)
				}
			default:
				return stats.SnapshotOptions{}, fmt.Errorf("unknown include %q: must be %s", v, allowed)
			}
		}
	}
	return o, nil
}

// includeQuery is the include query value of opts, as parseIncludeQuery reads
// it.
//
//declscope:package // the include encoding shared by the server and the client
func includeQuery(o stats.SnapshotOptions) string {
	var parts []string
	if o.Hours {
		parts = append(parts, "hours")
	}
	if o.History {
		parts = append(parts, "history")
	}
	if o.Enhanced {
		parts = append(parts, "enhanced")
	}
	return strings.Join(parts, ",")
}

// queryID parses the optional ?id= of /v1/reactions and /v1/tracks; 0 means
// every one.
//
//declscope:package // the handlers read their query with it
func queryID(r *http.Request) (int, error) {
	if err := checkQuery(r, []string{"id"}); err != nil {
		return 0, err
	}
	v := r.URL.Query().Get("id")
	if v == "" {
		return 0, nil
	}
	id, err := strconv.Atoi(v)
	if err != nil || id < 1 || id > config.MaxOpID {
		return 0, fmt.Errorf("invalid id %q: must be between 1 and %d", v, config.MaxOpID)
	}
	return id, nil
}
