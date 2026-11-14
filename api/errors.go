package api

import (
	"net/http"
)

// IsForbidden reports whether the server refused the request with 403: the
// session is valid but the user's role may not do this.
func IsForbidden(err error) bool {
	return HTTPStatus(err) == http.StatusForbidden
}
