package zone

import "errors"

// ErrInvalid is the root of every rejection this package produces. All other
// errors here wrap it, so a caller that only needs to distinguish "the input
// was bad" from "something went wrong" can test for this one alone, while a
// caller that wants to explain the problem can test for the specific error.
var ErrInvalid = errors.New("invalid")
