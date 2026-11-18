package purelymail

import "errors"

// APIError is an error the api returned; anything else is an http-layer error.
type APIError struct {
	Code    string // invalidToken, invalidPassword, invalidUserName, ruleAlreadyExists, internalError...
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

var ErrSilentNoOp = errors.New("the api reported success but the rule was not created")
