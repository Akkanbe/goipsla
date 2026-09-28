// The API of package probe (contract types); part of its core.
//
//declscope:core

package probe

import "strconv"

// String returns a lower-case name for logs: "reply", "timeout",
// "unreachable", "verifyError" or "error".
func (o Outcome) String() string {
	switch o {
	case OutcomeReply:
		return "reply"
	case OutcomeTimeout:
		return "timeout"
	case OutcomeUnreachable:
		return "unreachable"
	case OutcomeVerifyError:
		return "verifyError"
	case OutcomeError:
		return "error"
	}
	return "outcome(" + strconv.Itoa(int(o)) + ")"
}
