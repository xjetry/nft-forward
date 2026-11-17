package server

import (
	"errors"
	"net/http"
)

// A push is the panel giving a node something to act on: its rules, a probe,
// a new agent binary. When one fails, whose failure it is decides how the
// request that asked for it is answered:
//
//   - Nothing was sent, because of how things stand: the node is not
//     connected, or the panel has no binary it could send that node. The
//     request conflicts with that state — 409.
//   - Something was sent, or asked for, and a party other than the panel let
//     it down: the node (for the self-node, the local daemon) refused it,
//     answered unreadably, did not answer in time or dropped the connection;
//     or the release host did not deliver a binary matching its checksum —
//     502.
//   - Anything else is the panel's own failure, such as a statement that
//     failed while the rules were being read — 500.
//
// The first two are marked where the error is made (pushBlocked,
// pushUpstream). An error that carries neither mark is the third, so an
// error nobody classified is answered as the panel's.
//
// A probe is pushed the same way and its errors carry the same marks, but a
// probe's failure is its result: the probe routes report it in a 200 reply
// and do not come through pushErrAnswer.
type pushFault int

const (
	faultBlocked pushFault = iota + 1
	faultUpstream
)

// pushError is an error with its pushFault. Its text is the wrapped error's.
type pushError struct {
	fault pushFault
	err   error
}

func (e *pushError) Error() string { return e.err.Error() }
func (e *pushError) Unwrap() error { return e.err }

func pushBlocked(err error) error  { return &pushError{faultBlocked, err} }
func pushUpstream(err error) error { return &pushError{faultUpstream, err} }

// errNodeNotConnected: a push goes to the connection a node holds open to the
// hub, and a node without one cannot be sent anything.
var errNodeNotConnected = pushBlocked(errors.New("节点未连接"))

// pushErrAnswer is the HTTP status and the /api/v1 code for the error of a
// push; err is not nil.
func pushErrAnswer(err error) (status int, code string) {
	var pe *pushError
	if errors.As(err, &pe) {
		switch pe.fault {
		case faultBlocked:
			return http.StatusConflict, codeConflict
		case faultUpstream:
			return http.StatusBadGateway, codeUpstream
		}
	}
	return http.StatusInternalServerError, codeInternal
}
